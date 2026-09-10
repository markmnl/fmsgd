// fmsg-backfill assigns missing identities without changing sent timestamps.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"

	_ "github.com/lib/pq"
	"github.com/markmnl/fmsgd/pkg/message"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func run() error {
	domain := flag.String("domain", "", "local sending domain (required)")
	apply := flag.Bool("apply", false, "write hashes; default only lists pending messages")
	flag.Parse()
	if *domain == "" {
		return fmt.Errorf("-domain is required")
	}
	db, err := sql.Open("postgres", "")
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, `SELECT id FROM msg WHERE time_sent IS NOT NULL AND sha256 IS NULL AND lower(split_part(from_addr,'@',3))=lower($1) ORDER BY id`, *domain)
	if err != nil {
		return err
	}
	var pending []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			break
		}
		pending = append(pending, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	if !*apply {
		for _, id := range pending {
			fmt.Printf("message %d needs finalization\n", id)
		}
	} else {
		for len(pending) > 0 {
			var remaining []int64
			for _, id := range pending {
				err = finalize(ctx, db, id, 0)
				if err != nil {
					remaining = append(remaining, id)
					log.Printf("message %d: %v", id, err)
				} else {
					fmt.Printf("finalized message %d\n", id)
				}
			}
			if len(remaining) == len(pending) {
				return fmt.Errorf("%d messages could not be finalized; repair reported data and rerun", len(remaining))
			}
			pending = remaining
		}
	}
	rows, err = db.QueryContext(ctx, `SELECT b.msg_id,b.id FROM msg_add_to_batch b JOIN msg m ON m.id=b.msg_id WHERE m.time_sent IS NOT NULL AND b.sha256 IS NULL AND lower(split_part(b.add_to_from,'@',3))=lower($1) ORDER BY b.id`, *domain)
	if err != nil {
		return err
	}
	var batches [][2]int64
	for rows.Next() {
		var b [2]int64
		if err = rows.Scan(&b[0], &b[1]); err != nil {
			break
		}
		batches = append(batches, b)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	failed := 0
	for _, b := range batches {
		if !*apply {
			fmt.Printf("batch %d of message %d needs finalization\n", b[1], b[0])
			continue
		}
		if err = finalize(ctx, db, b[0], b[1]); err != nil {
			log.Printf("batch %d: %v", b[1], err)
			failed++
		} else {
			fmt.Printf("finalized batch %d\n", b[1])
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d batches could not be finalized", failed)
	}
	return nil
}
func finalize(ctx context.Context, db *sql.DB, id, batch int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var files message.Files
	committed := false
	commitAttempted := false
	defer func() {
		if !committed && !commitAttempted {
			files.Cleanup()
		}
	}()
	if batch == 0 {
		_, err = message.Finalize(ctx, message.SQLTx{Tx: tx}, id, 0, &files)
	} else {
		_, err = message.FinalizeBatch(ctx, message.SQLTx{Tx: tx}, id, batch, &files)
	}
	if err != nil {
		return err
	}
	commitAttempted = true
	err = tx.Commit()
	committed = err == nil
	return err
}
