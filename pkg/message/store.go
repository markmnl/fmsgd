// Package message finalizes messages in the shared fmsg message store.
// Callers own the transaction and must commit before publishing a message.
package message

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/markmnl/fmsgd/pkg/fmsg"
)

type Row interface{ Scan(...any) error }
type Rows interface {
	Row
	Next() bool
	Err() error
	Close()
}
type Tx interface {
	QueryRow(context.Context, string, ...any) Row
	Query(context.Context, string, ...any) (Rows, error)
	Exec(context.Context, string, ...any) error
}

// SQLTx adapts a database/sql transaction; pgx consumers supply a small adapter.
type SQLTx struct{ *sql.Tx }
type sqlRows struct{ *sql.Rows }

func (r sqlRows) Close() { _ = r.Rows.Close() }
func (t SQLTx) QueryRow(c context.Context, q string, a ...any) Row {
	return t.Tx.QueryRowContext(c, q, a...)
}
func (t SQLTx) Query(c context.Context, q string, a ...any) (Rows, error) {
	r, e := t.Tx.QueryContext(c, q, a...)
	if e != nil {
		return nil, e
	}
	return sqlRows{r}, nil
}
func (t SQLTx) Exec(c context.Context, q string, a ...any) error {
	_, e := t.Tx.ExecContext(c, q, a...)
	return e
}

// Files tracks newly prepared directories. Call Cleanup on rollback, including
// commit failure. Successful commits retain these files for future federation.
type Files []string

func (f Files) Cleanup() {
	for _, p := range f {
		_ = os.RemoveAll(p)
	}
}

func Address(s string) (fmsg.Address, error) {
	p := strings.Split(s, "@")
	if len(p) != 3 || p[0] != "" || p[1] == "" || p[2] == "" || len(s) > 255 {
		return fmsg.Address{}, fmt.Errorf("invalid address %q", s)
	}
	return fmsg.Address{User: p[1], Domain: p[2]}, nil
}

type stored struct {
	h              *fmsg.Header
	time           *float64
	pid            *int64
	hash, prepared []byte
}

func load(ctx context.Context, tx Tx, id int64) (*stored, error) {
	s := &stored{h: &fmsg.Header{}}
	var from string
	var noReply, important, terminal bool
	err := tx.QueryRow(ctx, `SELECT version,pid,psha256,no_reply,is_important,is_terminal,time_sent,from_addr,topic,type,size,filepath,sha256,wire_message FROM msg WHERE id=$1 FOR UPDATE`, id).Scan(&s.h.Version, &s.pid, &s.h.Pid, &noReply, &important, &terminal, &s.time, &from, &s.h.Topic, &s.h.Type, &s.h.Size, &s.h.Filepath, &s.hash, &s.prepared)
	if err != nil {
		return nil, err
	}
	s.h.From, err = Address(from)
	if err != nil {
		return nil, err
	}
	if noReply {
		s.h.Flags |= fmsg.FlagNoReply
	}
	if important {
		s.h.Flags |= fmsg.FlagImportant
	}
	if terminal {
		s.h.Flags |= fmsg.FlagTerminal
	}
	if s.time != nil {
		s.h.Timestamp = *s.time
	}
	if s.pid != nil || len(s.h.Pid) > 0 {
		s.h.Flags |= fmsg.FlagHasPid
	}
	r, err := tx.Query(ctx, `SELECT addr FROM msg_to WHERE msg_id=$1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	for r.Next() {
		var a string
		if err = r.Scan(&a); err != nil {
			break
		}
		var addr fmsg.Address
		addr, err = Address(a)
		if err != nil {
			break
		}
		s.h.To = append(s.h.To, addr)
	}
	if err == nil {
		err = r.Err()
	}
	r.Close()
	if err != nil {
		return nil, err
	}
	r, err = tx.Query(ctx, `SELECT type,filename,filesize,filepath FROM msg_attachment WHERE msg_id=$1 ORDER BY position,filename`, id)
	if err != nil {
		return nil, err
	}
	for r.Next() {
		var a fmsg.AttachmentHeader
		if err = r.Scan(&a.Type, &a.Filename, &a.Size, &a.Filepath); err != nil {
			break
		}
		s.h.Attachments = append(s.h.Attachments, a)
	}
	if err == nil {
		err = r.Err()
	}
	r.Close()
	return s, err
}

// Finalize stamps and hashes a draft, or backfills an unhashed sent message at
// its original time. Parents must already have identities. Existing hashes
// are never replaced. The caller must check ownership/draft status separately.
func Finalize(ctx context.Context, tx Tx, id int64, timestamp float64, files *Files) ([]byte, error) {
	s, err := load(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if len(s.hash) > 0 {
		return s.hash, nil
	}
	if s.time != nil {
		timestamp = *s.time
	}
	s.h.Timestamp = timestamp
	if s.pid != nil {
		var parentHash []byte
		if err = tx.QueryRow(ctx, `SELECT sha256 FROM msg WHERE id=$1 AND time_sent IS NOT NULL AND NOT is_terminal`, *s.pid).Scan(&parentHash); err != nil {
			return nil, fmt.Errorf("parent unavailable: %w", err)
		}
		if len(parentHash) != 32 {
			return nil, fmt.Errorf("parent %d needs hash backfill first", *s.pid)
		}
		if len(s.h.Pid) == 0 {
			s.h.Pid = parentHash
		}
	}
	if len(s.h.Pid) > 0 && len(s.h.Pid) != 32 {
		return nil, fmt.Errorf("invalid parent hash")
	}
	if s.time != nil {
		// A previously hashed child with a missing parent hash cannot be
		// repaired without changing an established protocol identity.
		var inconsistent bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM msg WHERE pid=$1 AND sha256 IS NOT NULL AND psha256 IS NULL)`, id).Scan(&inconsistent); err != nil {
			return nil, err
		}
		if inconsistent {
			return nil, fmt.Errorf("message %d has hashed children without parent hashes; manual repair required", id)
		}
	}
	h, dir, err := fmsg.Prepare(s.h)
	if err != nil {
		return nil, err
	}
	*files = append(*files, dir)
	hash, err := h.GetMessageHash()
	if err != nil {
		return nil, err
	}
	snapshot, err := fmsg.MarshalPrepared(h)
	if err != nil {
		return nil, err
	}
	if err = tx.Exec(ctx, `UPDATE msg SET time_sent=$2,sha256=$3,psha256=$4,wire_header=$5,wire_message=$6 WHERE id=$1`, id, timestamp, hash, h.Pid, h.Encode(), string(snapshot)); err != nil {
		return nil, err
	}
	if err = tx.Exec(ctx, `UPDATE msg SET psha256=$2 WHERE pid=$1 AND psha256 IS NULL AND sha256 IS NULL`, id, hash); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT id FROM msg_add_to_batch WHERE msg_id=$1 AND sha256 IS NULL ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	var batches []int64
	for rows.Next() {
		var b int64
		if err = rows.Scan(&b); err != nil {
			break
		}
		batches = append(batches, b)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, b := range batches {
		if err = tx.Exec(ctx, `UPDATE msg_add_to_batch SET time_added=GREATEST(time_added,$2) WHERE id=$1 AND sha256 IS NULL`, b, timestamp); err != nil {
			return nil, err
		}
		if _, err = FinalizeBatch(ctx, tx, id, b, files); err != nil {
			return nil, err
		}
	}
	return hash, nil
}

// FinalizeBatch seals one add-to exchange. Its payload representation is copied
// from the original (or a received batch), never recompressed independently.
func FinalizeBatch(ctx context.Context, tx Tx, id, batchID int64, files *Files) ([]byte, error) {
	s, err := load(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if s.time == nil {
		return nil, nil
	} // a draft's batches finalize with its send
	if len(s.hash) != 32 {
		return nil, fmt.Errorf("message %d needs hash backfill first", id)
	}
	var from string
	var timestamp float64
	var existing []byte
	if err = tx.QueryRow(ctx, `SELECT add_to_from,time_added,sha256 FROM msg_add_to_batch WHERE id=$1 AND msg_id=$2 FOR UPDATE`, batchID, id).Scan(&from, &timestamp, &existing); err != nil {
		return nil, err
	}
	if len(existing) > 0 {
		return existing, nil
	}
	var h *fmsg.Header
	if len(s.prepared) > 0 {
		h, err = fmsg.UnmarshalPrepared(s.prepared, s.hash)
	} else {
		// A host first receiving this message through add-to holds a batch's
		// exact payload representation and the original hash carried as pid.
		var snapshot, batchHash []byte
		err = tx.QueryRow(ctx, `SELECT wire_message,sha256 FROM msg_add_to_batch WHERE msg_id=$1 AND wire_message IS NOT NULL ORDER BY id LIMIT 1`, id).Scan(&snapshot, &batchHash)
		if err == nil {
			h, err = fmsg.UnmarshalPrepared(snapshot, batchHash)
		} else {
			// Legacy local rows can be upgraded only if preparation reproduces
			// their existing identity. Do not replace a published hash.
			var dir string
			h, dir, err = fmsg.Prepare(s.h)
			if err == nil {
				*files = append(*files, dir)
				var got []byte
				got, err = h.GetMessageHash()
				if err == nil && !bytes.Equal(got, s.hash) {
					h = h.Clone()
					h.Flags &^= fmsg.FlagCommonType
					for i := range h.Attachments {
						h.Attachments[i].Flags &^= 1
					}
					got, err = h.GetMessageHash()
					if err == nil && !bytes.Equal(got, s.hash) {
						err = fmt.Errorf("cannot reproduce legacy message %d; existing hash preserved", id)
					}
				}
				if err == nil {
					var data []byte
					data, err = fmsg.MarshalPrepared(h)
					if err == nil {
						err = tx.Exec(ctx, `UPDATE msg SET wire_message=$2 WHERE id=$1`, id, string(data))
					}
				}
			}
		}
	}
	if err != nil {
		return nil, err
	}
	h = h.Clone()
	h.Flags |= fmsg.FlagHasPid | fmsg.FlagHasAddTo
	h.Pid = s.hash
	h.Timestamp = timestamp
	h.Topic = ""
	h.AddTo = nil
	addr, err := Address(from)
	if err != nil {
		return nil, err
	}
	h.AddToFrom = &addr
	r, err := tx.Query(ctx, `SELECT addr FROM msg_add_to WHERE batch_id=$1 ORDER BY id`, batchID)
	if err != nil {
		return nil, err
	}
	for r.Next() {
		var a string
		if err = r.Scan(&a); err != nil {
			break
		}
		var addr fmsg.Address
		addr, err = Address(a)
		if err != nil {
			break
		}
		h.AddTo = append(h.AddTo, addr)
	}
	if err == nil {
		err = r.Err()
	}
	r.Close()
	if err != nil {
		return nil, err
	}
	if len(h.AddTo) == 0 || len(h.AddTo) > 255 {
		return nil, fmt.Errorf("invalid add-to recipient count")
	}
	hash, err := h.GetMessageHash()
	if err != nil {
		return nil, err
	}
	data, err := fmsg.MarshalPrepared(h)
	if err != nil {
		return nil, err
	}
	err = tx.Exec(ctx, `UPDATE msg_add_to_batch SET sha256=$2,wire_message=$3 WHERE id=$1`, batchID, hash, string(data))
	return hash, err
}
