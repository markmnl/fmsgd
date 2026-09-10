package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/markmnl/fmsgd/pkg/fmsg"
)

func previousStore(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("FMSG_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set FMSG_TEST_DATABASE_URL to test the standalone migration")
	}
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("backfill_%d", time.Now().UnixNano())
	if _, err = admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Exec("DROP SCHEMA " + schema + " CASCADE"); admin.Close() })
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
	dd, err := os.ReadFile("testdata/previous.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(dd)); err != nil {
		t.Fatal(err)
	}
	return db
}

func rawMessage(t *testing.T, domain, content string) *fmsg.Header {
	t.Helper()
	path := filepath.Join(t.TempDir(), "body")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return &fmsg.Header{Version: 1, From: fmsg.Address{User: "alice", Domain: domain},
		To: []fmsg.Address{{User: "bob", Domain: "example.com"}}, Timestamp: 1234.5,
		Topic: "stored", Type: "text/plain;charset=UTF-8", Filepath: path, Size: uint32(len(content))}
}

func putOld(t *testing.T, db *sql.DB, raw *fmsg.Header, pid any, hash, header []byte) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`INSERT INTO msg(version,pid,psha256,time_sent,from_addr,topic,type,size,filepath,sha256,wire_header,no_reply,is_important,is_terminal)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING id`, raw.Version, pid, bytesOrNull(raw.Pid), raw.Timestamp, raw.From.ToString(), raw.Topic, raw.Type, raw.Size, raw.Filepath, bytesOrNull(hash), bytesOrNull(header),
		raw.Flags&fmsg.FlagNoReply != 0, raw.Flags&fmsg.FlagImportant != 0, raw.Flags&fmsg.FlagTerminal != 0).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, a := range raw.To {
		if _, err := db.Exec(`INSERT INTO msg_to(msg_id,addr,time_delivered,response_code) VALUES($1,$2,1235,200)`, id, a.ToString()); err != nil {
			t.Fatal(err)
		}
	}
	for i, a := range raw.Attachments {
		if _, err := db.Exec(`INSERT INTO msg_attachment(msg_id,position,type,filename,filesize,filepath) VALUES($1,$2,$3,$4,$5,$6)`, id, i, a.Type, a.Filename, a.Size, a.Filepath); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func putBatch(t *testing.T, db *sql.DB, id int64, b oldBatch) int64 {
	t.Helper()
	var bid int64
	if err := db.QueryRow(`INSERT INTO msg_add_to_batch(msg_id,add_to_from,time_added,sha256) VALUES($1,$2,$3,$4) RETURNING id`, id, b.from.ToString(), b.time, bytesOrNull(b.hash)).Scan(&bid); err != nil {
		t.Fatal(err)
	}
	for _, a := range b.to {
		if _, err := db.Exec(`INSERT INTO msg_add_to(msg_id,batch_id,addr) VALUES($1,$2,$3)`, id, bid, a.ToString()); err != nil {
			t.Fatal(err)
		}
	}
	return bid
}

func prepared(t *testing.T, raw *fmsg.Header) *fmsg.Header {
	t.Helper()
	h, dir, err := fmsg.Prepare(raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return h
}
func hashOf(t *testing.T, h *fmsg.Header) []byte {
	t.Helper()
	hash, err := h.GetMessageHash()
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func TestStandaloneMigration(t *testing.T) {
	db := previousStore(t)
	rootRaw := rawMessage(t, "example.com", "local root")
	root := putOld(t, db, rootRaw, nil, nil, nil)
	childRaw := rawMessage(t, "example.com", "local reply")
	childRaw.Flags = fmsg.FlagHasPid
	child := putOld(t, db, childRaw, root, nil, nil)
	localBatch := putBatch(t, db, root, oldBatch{from: rootRaw.From, time: 1300, to: []fmsg.Address{{User: "carol", Domain: "example.com"}}})

	// Published string-form hash must remain string-form, even though new
	// finalization chooses common type IDs.
	oldString := rawMessage(t, "example.com", "published")
	oldHash := hashOf(t, oldString)
	published := putOld(t, db, oldString, nil, oldHash, nil)

	localCompressed := rawMessage(t, "example.com", strings.Repeat("published compressed ", 300))
	localWire := prepared(t, localCompressed)
	localHash := hashOf(t, localWire)
	compressed := putOld(t, db, localCompressed, nil, localHash, nil)
	publishedBatch := oldBatch{from: oldString.From, time: 1301, to: []fmsg.Address{{User: "carol", Domain: "example.com"}}}
	publishedBatch.hash = hashOf(t, batchHeader(oldString, oldHash, publishedBatch))
	publishedBatchID := putBatch(t, db, published, publishedBatch)

	// Pending drafts and draft batches remain editable; an existing draft
	// reply gains its newly finalized parent's protocol identity.
	var draft int64
	if err := db.QueryRow(`INSERT INTO msg(version,pid,from_addr,topic,type,size,filepath) VALUES(1,$1,'@alice@example.com','','text/plain',0,'') RETURNING id`, root).Scan(&draft); err != nil {
		t.Fatal(err)
	}
	draftBatch := putBatch(t, db, draft, oldBatch{from: rootRaw.From, time: 1302, to: []fmsg.Address{{User: "carol", Domain: "example.com"}}})

	// Received compression and mixed type encodings must preserve the exact
	// wire header. Expanded body and attachment files are all the old store has.
	remote := rawMessage(t, "example.org", strings.Repeat("compressible content ", 400))
	att := rawMessage(t, "example.org", strings.Repeat("attachment ", 300))
	remote.Attachments = []fmsg.AttachmentHeader{{Type: "text/plain;charset=UTF-8", Filename: "note.txt", Size: att.Size, Filepath: att.Filepath}}
	remoteWire := prepared(t, remote).Clone()
	remoteWire.Attachments[0].Flags &^= 1
	remoteHash := hashOf(t, remoteWire)
	received := putOld(t, db, remote, nil, remoteHash, remoteWire.Encode())

	// First delivery through add-to retains the canonical hash carried as pid,
	// and prepares the batch without inventing an original header.
	batchRaw := rawMessage(t, "example.org", strings.Repeat("forwarded content ", 400))
	batchBase := prepared(t, batchRaw)
	canonicalHash := hashOf(t, batchBase)
	b := oldBatch{from: batchRaw.From, time: 1400, to: []fmsg.Address{{User: "carol", Domain: "example.com"}}}
	batchWire := batchHeader(batchBase, canonicalHash, b)
	b.hash = hashOf(t, batchWire)
	batchRaw.Timestamp = b.time
	addToOnly := putOld(t, db, batchRaw, nil, canonicalHash, batchWire.Encode())
	receivedBatch := putBatch(t, db, addToOnly, b)

	// A dry run exercises the full conversion, then removes its columns and files.
	if err := migrate(context.Background(), db, "example.com", false, io.Discard); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND column_name='wire_message'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("dry run changed schema: %d %v", count, err)
	}
	for _, raw := range []*fmsg.Header{rootRaw, childRaw, oldString} {
		matches, _ := filepath.Glob(filepath.Join(filepath.Dir(raw.Filepath), ".fmsg-wire-*"))
		if len(matches) != 0 {
			t.Fatal("dry run left files", matches)
		}
	}
	if err := migrate(context.Background(), db, "example.com", true, io.Discard); err != nil {
		t.Fatal(err)
	}
	var rootHash []byte
	var snapshots = make(map[int64][]byte)
	for _, id := range []int64{root, child, published, compressed, received, addToOnly} {
		var hash, data, parent []byte
		var stamp float64
		if err := db.QueryRow(`SELECT sha256,wire_message,psha256,time_sent FROM msg WHERE id=$1`, id).Scan(&hash, &data, &parent, &stamp); err != nil {
			t.Fatal(err)
		}
		if id == root {
			rootHash = hash
		}
		if id == child && !bytes.Equal(parent, rootHash) {
			t.Fatal("reply parent not backfilled")
		}
		if id == published && !bytes.Equal(hash, oldHash) {
			t.Fatal("published string hash changed")
		}
		if id == compressed && !bytes.Equal(hash, localHash) {
			t.Fatal("published compressed hash changed")
		}
		if id == received && !bytes.Equal(hash, remoteHash) {
			t.Fatal("received hash changed")
		}
		if id == addToOnly {
			if !bytes.Equal(hash, canonicalHash) || len(data) != 0 || stamp != 1400 {
				t.Fatal("invented original identity")
			}
		} else {
			if stamp != 1234.5 {
				t.Fatal("timestamp changed")
			}
			if _, err := fmsg.UnmarshalPrepared(data, hash); err != nil {
				t.Fatal(err)
			}
		}
		snapshots[id] = data
	}
	for _, id := range []int64{localBatch, publishedBatchID, receivedBatch} {
		var hash, data []byte
		if err := db.QueryRow(`SELECT sha256,wire_message FROM msg_add_to_batch WHERE id=$1`, id).Scan(&hash, &data); err != nil {
			t.Fatal(err)
		}
		if _, err := fmsg.UnmarshalPrepared(data, hash); err != nil {
			t.Fatal(err)
		}
		if id == publishedBatchID && !bytes.Equal(hash, publishedBatch.hash) {
			t.Fatal("published batch hash changed")
		}
		if id == receivedBatch && !bytes.Equal(hash, b.hash) {
			t.Fatal("received batch identity changed")
		}
	}
	var draftHash, draftParent, draftSnapshot []byte
	var draftTime *float64
	if err := db.QueryRow(`SELECT sha256,psha256,wire_message,time_sent FROM msg WHERE id=$1`, draft).Scan(&draftHash, &draftParent, &draftSnapshot, &draftTime); err != nil {
		t.Fatal(err)
	}
	if len(draftHash) != 0 || len(draftSnapshot) != 0 || draftTime != nil || !bytes.Equal(draftParent, rootHash) {
		t.Fatal("draft identity/state changed")
	}
	if err := db.QueryRow(`SELECT sha256,wire_message FROM msg_add_to_batch WHERE id=$1`, draftBatch).Scan(&draftHash, &draftSnapshot); err != nil {
		t.Fatal(err)
	}
	if len(draftHash) != 0 || len(draftSnapshot) != 0 {
		t.Fatal("prematurely finalized draft batch")
	}

	if err := migrate(context.Background(), db, "example.com", true, io.Discard); err != nil {
		t.Fatal("rerun", err)
	}
	for id, expected := range snapshots {
		var got []byte
		db.QueryRow(`SELECT wire_message FROM msg WHERE id=$1`, id).Scan(&got)
		if !bytes.Equal(expected, got) {
			t.Fatal("rerun changed snapshot", id)
		}
	}
	if _, err := db.Exec(`UPDATE msg SET topic='changed' WHERE id=$1`, root); err == nil {
		t.Fatal("migration did not install strict immutability")
	}
	if _, err := db.Exec(`UPDATE msg SET wire_message=NULL WHERE id=$1`, root); err == nil {
		t.Fatal("migration permits clearing snapshot")
	}
	if _, err := db.Exec(`UPDATE msg_to SET time_read=2000 WHERE msg_id=$1`, root); err != nil {
		t.Fatal("receipt blocked", err)
	}
	if _, err := db.Exec(`INSERT INTO msg(version,time_sent,from_addr,topic,type,size,filepath) VALUES(1,1234,'@alice@example.com','','text/plain',0,'')`); err == nil {
		t.Fatal("migration permits sent message without identity")
	}
}

func TestMigrationFailureRollsBackEverything(t *testing.T) {
	for _, cause := range []string{"missing file", "hash mismatch", "hashed child without parent hash"} {
		t.Run(cause, func(t *testing.T) {
			db := previousStore(t)
			raw := rawMessage(t, "example.com", "good")
			root := putOld(t, db, raw, nil, nil, nil)
			bad := rawMessage(t, "example.com", "bad")
			switch cause {
			case "missing file":
				putOld(t, db, bad, nil, nil, nil)
				os.Remove(bad.Filepath)
			case "hash mismatch":
				putOld(t, db, bad, nil, bytes.Repeat([]byte{7}, 32), nil)
			case "hashed child without parent hash":
				putOld(t, db, bad, root, bytes.Repeat([]byte{7}, 32), nil)
			}
			if err := migrate(context.Background(), db, "example.com", true, io.Discard); err == nil {
				t.Fatal("unsafe migration succeeded")
			}
			var hash []byte
			if err := db.QueryRow(`SELECT sha256 FROM msg WHERE id=$1`, root).Scan(&hash); err != nil || hash != nil {
				t.Fatal("partial data conversion", err)
			}
			var count int
			db.QueryRow(`SELECT count(*) FROM information_schema.columns WHERE table_schema=current_schema() AND column_name='wire_message'`).Scan(&count)
			if count != 0 {
				t.Fatal("partial schema conversion")
			}
			files, _ := filepath.Glob(filepath.Join(filepath.Dir(raw.Filepath), ".fmsg-wire-*"))
			if len(files) > 0 {
				t.Fatal("rollback left staged files", files)
			}
		})
	}
}

func TestDecodeHeaderRejectsTruncation(t *testing.T) {
	h := prepared(t, rawMessage(t, "example.com", strings.Repeat("content ", 200)))
	data := h.Encode()
	for i := 0; i < len(data); i++ {
		if _, err := decodeHeader(data[:i]); err == nil {
			t.Fatalf("accepted prefix %d", i)
		}
	}
	if _, err := decodeHeader(append(data, 0)); err == nil {
		t.Fatal("accepted trailing data")
	}
}
