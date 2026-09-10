// fmsg-backfill upgrades the pre-finalization message store offline. All legacy
// reconstruction and schema conversion live in this standalone command.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strings"

	_ "github.com/lib/pq"
	"github.com/markmnl/fmsgd"
)

func main() {
	domain := flag.String("domain", "", "local sending domain (required)")
	apply := flag.Bool("apply", false, "commit schema and data conversion; default validates then rolls back")
	flag.Parse()
	if *domain == "" {
		log.Fatal("-domain is required")
	}
	db, err := sql.Open("postgres", "") // standard PG* environment variables
	if err == nil {
		defer db.Close()
		err = migrate(context.Background(), db, *domain, *apply, os.Stdout)
	}
	if err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

// One transaction owns both the schema change and every converted row. The
// operator stops services first; NOWAIT also refuses a store still in use.
func migrate(ctx context.Context, db *sql.DB, domain string, apply bool, out io.Writer) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	m := &migration{tx: tx, domain: domain, visiting: make(map[int64]bool), done: make(map[int64]bool)}
	commitAttempted := false
	defer func() {
		_ = tx.Rollback()
		// A lost COMMIT acknowledgement does not prove rollback. Retain the
		// files in that case and let the next run verify committed snapshots.
		if !commitAttempted {
			for _, dir := range m.files {
				_ = os.RemoveAll(dir)
			}
		}
	}()
	if _, err = tx.ExecContext(ctx, `LOCK TABLE msg,msg_to,msg_attachment,msg_add_to_batch,msg_add_to,msg_add_to_notify IN ACCESS EXCLUSIVE MODE NOWAIT`); err != nil {
		return fmt.Errorf("stop daemon and API before migration: %w", err)
	}
	// Bootstrap SQL remains plain CREATE statements. Only this command knows
	// the previous schema and how to replace its triggers in place.
	_, functions, ok := strings.Cut(fmsgd.Schema, "-- Functions and triggers.\n")
	if !ok {
		return fmt.Errorf("embedded schema has no functions section")
	}
	triggerPattern := regexp.MustCompile(`(?s)create (?:constraint )?trigger (\w+)\s+.*?\bon (\w+)\s`)
	for _, match := range triggerPattern.FindAllStringSubmatch(functions, -1) {
		if _, err = tx.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+match[1]+" ON "+match[2]); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `
		DROP TRIGGER IF EXISTS trg_msg_prevent_unreferenceable_parent ON msg;
		DROP FUNCTION IF EXISTS prevent_referenced_msg_from_becoming_unreferenceable();
		ALTER TABLE msg ADD COLUMN IF NOT EXISTS wire_message jsonb;
		ALTER TABLE msg_add_to_batch ADD COLUMN IF NOT EXISTS wire_message jsonb;
		CREATE INDEX IF NOT EXISTS msg_add_to_batch_sha256_idx ON msg_add_to_batch (sha256) WHERE sha256 IS NOT NULL;
		CREATE INDEX IF NOT EXISTS msg_pid_idx ON msg (pid) WHERE pid IS NOT NULL;
	`); err != nil {
		return err
	}
	ids, err := m.ids(`SELECT id FROM msg WHERE time_sent IS NOT NULL ORDER BY id`)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = m.message(id); err != nil {
			return fmt.Errorf("message %d: %w; database changes rolled back", id, err)
		}
		fmt.Fprintf(out, "verified message %d\n", id)
	}
	// Draft children may have existed before their local parent had a hash.
	if _, err = tx.ExecContext(ctx, `UPDATE msg child SET psha256=parent.sha256 FROM msg parent WHERE child.pid=parent.id AND child.psha256 IS NULL AND child.time_sent IS NULL`); err != nil {
		return err
	}
	if err = m.validate(); err != nil {
		return err
	}
	// Replace function definitions only here, without duplicating them or
	// carrying upgrade statements in the bootstrap schema.
	functions = strings.ReplaceAll(functions, "create function ", "create or replace function ")
	if _, err = tx.ExecContext(ctx, functions); err != nil {
		return err
	}
	if !apply {
		if err = tx.Rollback(); err != nil {
			return err
		}
		fmt.Fprintln(out, "Dry run passed; schema, data and staged files rolled back. Run with -apply to commit.")
		return nil
	}
	commitAttempted = true
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit outcome uncertain; keep payload files and rerun to verify: %w", err)
	}
	fmt.Fprintln(out, "Migration committed. Start the matching daemon and API; do not rerun dd.sql.")
	return nil
}
