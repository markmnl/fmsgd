package message

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/markmnl/fmsgd/pkg/fmsg"
)

func testStore(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("FMSG_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set FMSG_TEST_DATABASE_URL to test PostgreSQL finalization")
	}
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("hash_store_%d", time.Now().UnixNano())
	if _, err = admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE"); admin.Close() })
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	dd, err := os.ReadFile("../../dd.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(dd)); err != nil {
		t.Fatal(err)
	}
	return db, string(dd)
}
func insertDraft(t *testing.T, db *sql.DB, parent any, body string) int64 {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := db.QueryRow(`INSERT INTO msg(version,pid,from_addr,topic,type,size,filepath) VALUES(1,$1,'@alice@example.com','','text/plain;charset=UTF-8',$2,$3) RETURNING id`, parent, len(body), path).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO msg_to(msg_id,addr) VALUES($1,'@bob@example.com')`, id); err != nil {
		t.Fatal(err)
	}
	return id
}
func seal(t *testing.T, db *sql.DB, id int64) []byte {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var files Files
	hash, err := Finalize(context.Background(), SQLTx{tx}, id, 1234.5, &files)
	if err != nil {
		files.Cleanup()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		files.Cleanup()
		t.Fatal(err)
	}
	return hash
}
func TestFinalizeAndImmutability(t *testing.T) {
	db, _ := testStore(t)
	root := insertDraft(t, db, nil, "root")
	hash := seal(t, db, root)
	child := insertDraft(t, db, root, "child")
	childHash := seal(t, db, child)
	if len(hash) != 32 || len(childHash) != 32 {
		t.Fatal("missing identities")
	}
	var stamp float64
	var parent []byte
	if err := db.QueryRow(`SELECT time_sent,psha256 FROM msg WHERE id=$1`, child).Scan(&stamp, &parent); err != nil {
		t.Fatal(err)
	}
	if stamp != 1234.5 || !bytes.Equal(parent, hash) {
		t.Fatal("finalization lost timestamp or parent")
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var files Files
	if _, err = Finalize(context.Background(), SQLTx{tx}, root, 999, &files); err == nil {
		t.Fatal("accepted an already finalized message")
	}
	tx.Rollback()
	for _, query := range []string{
		`UPDATE msg SET time_sent=102 WHERE id=$1`,
		`UPDATE msg SET topic='changed' WHERE id=$1`,
		`UPDATE msg SET sha256=NULL WHERE id=$1`,
		`UPDATE msg SET wire_message=NULL WHERE id=$1`,
		`UPDATE msg SET wire_header=NULL WHERE id=$1`,
		`UPDATE msg_to SET addr='@mallory@example.com' WHERE msg_id=$1`,
		`INSERT INTO msg_to(msg_id,addr) VALUES($1,'@carol@example.com')`,
		`DELETE FROM msg_to WHERE msg_id=$1`,
		`INSERT INTO msg_attachment(msg_id,filename,filesize,filepath) VALUES($1,'new.txt',0,'')`,
	} {
		if _, err := db.Exec(query, root); err == nil {
			t.Fatalf("immutable mutation allowed: %s", query)
		}
	}
	if _, err := db.Exec(`UPDATE msg_to SET time_read=102,response_code=200 WHERE msg_id=$1`, root); err != nil {
		t.Fatal("receipt mutation rejected", err)
	}
	if _, err := db.Exec(`INSERT INTO msg_to(msg_id,addr) VALUES($1,'@bob@example.com') ON CONFLICT DO NOTHING`, root); err != nil {
		t.Fatal("duplicate receipt rejected", err)
	}
	newDraft := insertDraft(t, db, nil, "draft")
	if _, err := db.Exec(`UPDATE msg SET time_sent=102 WHERE id=$1`, newDraft); err == nil {
		t.Fatal("committed sent message without hash")
	}
}
func TestFinalizeDraftBatchesAndRollback(t *testing.T) {
	db, _ := testStore(t)
	root := insertDraft(t, db, nil, "root with draft batch")
	var batch int64
	if err := db.QueryRow(`INSERT INTO msg_add_to_batch(msg_id,add_to_from,time_added) VALUES($1,'@alice@example.com',1234.5) RETURNING id`, root).Scan(&batch); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO msg_add_to(msg_id,batch_id,addr) VALUES($1,$2,'@carol@example.com')`, root, batch); err != nil {
		t.Fatal(err)
	}
	hash := seal(t, db, root)
	var batchHash, snapshot []byte
	if err := db.QueryRow(`SELECT sha256,wire_message FROM msg_add_to_batch WHERE id=$1`, batch).Scan(&batchHash, &snapshot); err != nil {
		t.Fatal(err)
	}
	h, err := fmsg.UnmarshalPrepared(snapshot, batchHash)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(hash, batchHash) || !bytes.Equal(h.Pid, hash) || len(h.AddTo) != 1 {
		t.Fatal("wrong batch identity")
	}
	if _, err = db.Exec(`UPDATE msg_add_to SET addr='@mallory@example.com' WHERE batch_id=$1`, batch); err == nil {
		t.Fatal("batch recipients mutable")
	}
	if _, err = db.Exec(`UPDATE msg_add_to_batch SET time_added=9999 WHERE id=$1`, batch); err == nil {
		t.Fatal("batch timestamp mutable")
	}
	// A failed commit removes its staged payloads and leaves the draft intact.
	draft := insertDraft(t, db, nil, "rollback")
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var files Files
	if _, err = Finalize(context.Background(), SQLTx{tx}, draft, 1234.5, &files); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()
	files.Cleanup()
	for _, path := range files {
		if _, err = os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("orphaned wire files", err)
		}
	}
	var sent *float64
	if err = db.QueryRow(`SELECT time_sent FROM msg WHERE id=$1`, draft).Scan(&sent); err != nil || sent != nil {
		t.Fatal("rollback stamped message", err)
	}
}
