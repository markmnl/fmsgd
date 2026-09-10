package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/markmnl/fmsgd/pkg/fmsg"
)

type migration struct {
	tx       *sql.Tx
	domain   string
	files    []string
	visiting map[int64]bool
	done     map[int64]bool
}

type oldMessage struct {
	id       int64
	pid      *int64
	time     *float64
	h        *fmsg.Header // expanded payload paths from the old store
	hash     []byte
	wire     []byte
	prepared []byte
}

type oldBatch struct {
	id       int64
	from     fmsg.Address
	time     float64
	to       []fmsg.Address
	hash     []byte
	prepared []byte
}

func address(s string) (fmsg.Address, error) {
	p := strings.SplitN(s, "@", 3)
	if len(p) != 3 || p[0] != "" || p[1] == "" || p[2] == "" || len(s) > 255 {
		return fmsg.Address{}, fmt.Errorf("invalid stored address %q", s)
	}
	return fmsg.Address{User: p[1], Domain: p[2]}, nil
}

func (m *migration) ids(query string, args ...any) ([]int64, error) {
	rows, err := m.tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (m *migration) addresses(query string, id int64) ([]fmsg.Address, error) {
	rows, err := m.tx.Query(query, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []fmsg.Address
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		a, err := address(raw)
		if err != nil {
			return nil, err
		}
		list = append(list, a)
	}
	return list, rows.Err()
}

func (m *migration) load(id int64) (*oldMessage, error) {
	s := &oldMessage{id: id, h: &fmsg.Header{}}
	var from string
	var noReply, important, terminal bool
	err := m.tx.QueryRow(`SELECT version,pid,psha256,no_reply,is_important,is_terminal,time_sent,from_addr,topic,type,size,filepath,sha256,wire_header,wire_message FROM msg WHERE id=$1`, id).Scan(&s.h.Version, &s.pid, &s.h.Pid, &noReply, &important, &terminal, &s.time, &from, &s.h.Topic, &s.h.Type, &s.h.Size, &s.h.Filepath, &s.hash, &s.wire, &s.prepared)
	if err != nil {
		return nil, err
	}
	s.h.From, err = address(from)
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
	s.h.To, err = m.addresses(`SELECT addr FROM msg_to WHERE msg_id=$1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	rows, err := m.tx.Query(`SELECT type,filename,filesize,filepath FROM msg_attachment WHERE msg_id=$1 ORDER BY position,filename`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a fmsg.AttachmentHeader
		if err = rows.Scan(&a.Type, &a.Filename, &a.Size, &a.Filepath); err != nil {
			return nil, err
		}
		s.h.Attachments = append(s.h.Attachments, a)
	}
	return s, rows.Err()
}

func (m *migration) batches(id int64) ([]oldBatch, error) {
	ids, err := m.ids(`SELECT id FROM msg_add_to_batch WHERE msg_id=$1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	var batches []oldBatch
	for _, id := range ids {
		b := oldBatch{id: id}
		var from string
		if err = m.tx.QueryRow(`SELECT add_to_from,time_added,sha256,wire_message FROM msg_add_to_batch WHERE id=$1`, id).Scan(&from, &b.time, &b.hash, &b.prepared); err != nil {
			return nil, err
		}
		b.from, err = address(from)
		if err != nil {
			return nil, err
		}
		b.to, err = m.addresses(`SELECT addr FROM msg_add_to WHERE batch_id=$1 ORDER BY id`, id)
		if err != nil {
			return nil, err
		}
		batches = append(batches, b)
	}
	return batches, nil
}

func (m *migration) message(id int64) error {
	if m.done[id] {
		return nil
	}
	if m.visiting[id] {
		return fmt.Errorf("cyclic parent links at message %d", id)
	}
	m.visiting[id] = true
	defer delete(m.visiting, id)
	s, err := m.load(id)
	if err != nil {
		return err
	}
	if s.time == nil {
		return fmt.Errorf("sent child references draft parent %d", id)
	}
	if s.pid != nil {
		if err = m.message(*s.pid); err != nil {
			return fmt.Errorf("parent %d: %w", *s.pid, err)
		}
		if len(s.h.Pid) == 0 {
			if err = m.tx.QueryRow(`SELECT sha256 FROM msg WHERE id=$1`, *s.pid).Scan(&s.h.Pid); err != nil {
				return err
			}
		}
	}
	// Old receivers sometimes kept wire sizes beside already expanded files.
	// For published identities the full hash remains the authority: use actual
	// expanded lengths when reconstructing, and only persist them after verification.
	if len(s.hash) == 32 && len(s.prepared) == 0 {
		if err = expandedSizes(s.h); err != nil {
			return err
		}
	}
	batches, err := m.batches(id)
	if err != nil {
		return err
	}
	var base, received *fmsg.Header
	if len(s.prepared) > 0 {
		base, err = fmsg.UnmarshalPrepared(s.prepared, s.hash)
	} else if len(s.wire) > 0 {
		received, err = decodeHeader(s.wire)
		if err == nil && received.Flags&fmsg.FlagHasAddTo != 0 {
			for _, b := range batches {
				if len(b.prepared) > 0 {
					candidate, e := fmsg.UnmarshalPrepared(b.prepared, b.hash)
					if e != nil {
						return e
					}
					if bytes.Equal(candidate.Encode(), s.wire) {
						received = candidate
						break
					}
				}
			}
		}
		if err == nil && received.Filepath == "" {
			received, err = m.restore(received, s.h)
		}
		if err == nil && received.Flags&fmsg.FlagHasAddTo == 0 {
			base = received
		} else if err == nil {
			// The canonical row represents an original known only by the pid
			// of the received add-to. Its original header is not recoverable.
			if !bytes.Equal(received.Pid, s.hash) {
				return fmt.Errorf("received batch pid differs from canonical identity")
			}
		}
	} else {
		// Rerunning against a fully migrated add-to-only original is valid.
		for _, b := range batches {
			if len(b.prepared) > 0 {
				received, err = fmsg.UnmarshalPrepared(b.prepared, b.hash)
				break
			}
		}
		if received == nil && err == nil {
			if len(s.hash) == 0 && !strings.EqualFold(s.h.From.Domain, m.domain) {
				return fmt.Errorf("remote message has no published hash or wire header")
			}
			base, err = m.reconstruct(s.h, s.hash)
		}
	}
	if err != nil {
		return err
	}
	if base != nil {
		hash, err := base.GetMessageHash()
		if err != nil {
			return err
		}
		if len(s.hash) > 0 && !bytes.Equal(hash, s.hash) {
			return fmt.Errorf("cannot reproduce published message hash; identity preserved")
		}
		s.hash = hash
		data, err := fmsg.MarshalPrepared(base)
		if err != nil {
			return err
		}
		if _, err = m.tx.Exec(`UPDATE msg SET sha256=$2,psha256=$3,wire_header=$4,wire_message=$5,is_deflate=$6 WHERE id=$1`, id, hash, bytesOrNull(base.Pid), base.Encode(), string(data), base.Flags&fmsg.FlagDeflate != 0); err != nil {
			return err
		}
	} else {
		base = received
	}
	if base == nil || len(s.hash) != 32 {
		return fmt.Errorf("missing canonical identity or payload representation")
	}
	// Early receivers stamped the arrival time on the batch row. An exact
	// retained wire header supplies the sending timestamp. Only repair an
	// unambiguous, unhashed batch; existing batch identities remain authoritative.
	if received != nil && received.Flags&fmsg.FlagHasAddTo != 0 {
		candidate := -1
		matches := 0
		exact := false
		for i, b := range batches {
			h := batchHeader(base, s.hash, b)
			if bytes.Equal(h.Encode(), received.Encode()) {
				exact = true
				break
			}
			h.Timestamp = received.Timestamp
			if len(b.hash) == 0 && bytes.Equal(h.Encode(), received.Encode()) {
				candidate = i
				matches++
			}
		}
		if !exact && matches == 1 {
			batches[candidate].time = received.Timestamp
			if _, err = m.tx.Exec(`UPDATE msg_add_to_batch SET time_added=$2 WHERE id=$1`, batches[candidate].id, received.Timestamp); err != nil {
				return err
			}
		}
	}
	matchedReceived := received == nil || received.Flags&fmsg.FlagHasAddTo == 0
	for _, b := range batches {
		h := batchHeader(base, s.hash, b)
		if received != nil && bytes.Equal(h.Encode(), received.Encode()) {
			matchedReceived = true
		}
		if len(b.prepared) > 0 {
			h, err = fmsg.UnmarshalPrepared(b.prepared, b.hash)
		} else {
			h, err = selectTypes(h, b.hash)
		}
		if err != nil {
			return fmt.Errorf("batch %d: %w", b.id, err)
		}
		if !bytes.Equal(h.Pid, s.hash) {
			return fmt.Errorf("batch %d references a different original", b.id)
		}
		hash, err := h.GetMessageHash()
		if err != nil {
			return err
		}
		data, err := fmsg.MarshalPrepared(h)
		if err != nil {
			return err
		}
		if _, err = m.tx.Exec(`UPDATE msg_add_to_batch SET sha256=$2,wire_message=$3 WHERE id=$1`, b.id, hash, string(data)); err != nil {
			return err
		}
	}
	if !matchedReceived {
		return fmt.Errorf("received wire header has no matching add-to batch")
	}
	if _, err = m.tx.Exec(`UPDATE msg SET size=$2 WHERE id=$1`, id, s.h.Size); err != nil {
		return err
	}
	for _, a := range s.h.Attachments {
		if _, err = m.tx.Exec(`UPDATE msg_attachment SET filesize=$3 WHERE msg_id=$1 AND filename=$2`, id, a.Filename, a.Size); err != nil {
			return err
		}
	}
	m.done[id] = true
	return nil
}

func batchHeader(base *fmsg.Header, hash []byte, b oldBatch) *fmsg.Header {
	h := base.Clone()
	h.Flags |= fmsg.FlagHasPid | fmsg.FlagHasAddTo
	h.Pid, h.Timestamp, h.Topic = hash, b.time, ""
	h.AddToFrom, h.AddTo = &b.from, b.to
	return h
}

// Local sends previously selected compression and common types during the
// first network delivery. Try those historical forms only in this tool, and
// accept a candidate only if it reproduces the entire existing message hash.
func (m *migration) reconstruct(raw *fmsg.Header, expected []byte) (*fmsg.Header, error) {
	input := raw.Clone()
	// Early local notes could have an empty recipient list. Preserve their
	// recorded bytes; normal send validation still requires recipients.
	if len(input.To) == 0 {
		input.To = []fmsg.Address{input.From}
	}
	h, dir, err := fmsg.Prepare(input)
	if err != nil {
		return nil, err
	}
	h = h.Clone()
	h.To = raw.To
	if chosen, err := selectOriginal(h, expected); err == nil {
		m.files = append(m.files, dir)
		return chosen, nil
	}
	_ = os.RemoveAll(dir)
	h, dir, err = fmsg.Preserve(raw, filepath.Dir(raw.Filepath))
	if err != nil {
		return nil, err
	}
	chosen, err := selectOriginal(h, expected)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	m.files = append(m.files, dir)
	return chosen, nil
}

// Some early local writers hashed a root-form header despite retaining a
// relational parent link. Preserve that established identity and local link;
// never select this form for a message without an existing hash to verify.
func selectOriginal(h *fmsg.Header, expected []byte) (*fmsg.Header, error) {
	chosen, err := selectTypes(h, expected)
	if err == nil {
		return chosen, nil
	}
	if len(expected) == 32 && h.Flags&fmsg.FlagHasPid != 0 && h.Flags&fmsg.FlagHasAddTo == 0 {
		root := h.Clone()
		root.Flags &^= fmsg.FlagHasPid
		return selectTypes(root, expected)
	}
	return nil, err
}

func selectTypes(h *fmsg.Header, expected []byte) (*fmsg.Header, error) {
	for _, mode := range []int{0, 1, 2} {
		c := h.Clone()
		switch mode {
		case 1:
			fmsg.ApplyCommonTypes(c)
		case 2:
			c.Flags &^= fmsg.FlagCommonType
			for i := range c.Attachments {
				c.Attachments[i].Flags &^= 1
			}
		}
		hash, err := c.GetMessageHash()
		if err != nil {
			return nil, err
		}
		if len(expected) == 0 || bytes.Equal(expected, hash) {
			return c, nil
		}
	}
	return nil, fmt.Errorf("cannot reproduce published hash; identity preserved")
}

func (m *migration) validate() error {
	var invalid int
	err := m.tx.QueryRow(`SELECT count(*) FROM msg m WHERE
		(m.time_sent IS NOT NULL AND (m.sha256 IS NULL OR octet_length(m.sha256)<>32 OR
		 (m.wire_message IS NULL AND NOT EXISTS (SELECT 1 FROM msg_add_to_batch b WHERE b.msg_id=m.id AND b.wire_message IS NOT NULL)))) OR
		(m.pid IS NOT NULL AND NOT EXISTS (SELECT 1 FROM msg p WHERE p.id=m.pid AND p.time_sent IS NOT NULL AND NOT p.is_terminal AND
		 (m.psha256=p.sha256 OR EXISTS (SELECT 1 FROM msg_add_to_batch b WHERE b.msg_id=p.id AND b.sha256=m.psha256)))) OR
		(m.time_sent IS NULL AND (m.sha256 IS NOT NULL OR m.wire_message IS NOT NULL))`).Scan(&invalid)
	if err != nil {
		return err
	}
	if invalid != 0 {
		return fmt.Errorf("%d messages violate finalized identity or parent invariants", invalid)
	}
	err = m.tx.QueryRow(`SELECT count(*) FROM msg_add_to_batch b JOIN msg m ON m.id=b.msg_id WHERE
		m.is_terminal OR (m.time_sent IS NOT NULL AND
		(b.sha256 IS NULL OR octet_length(b.sha256)<>32 OR b.wire_message IS NULL))`).Scan(&invalid)
	if err != nil {
		return err
	}
	if invalid != 0 {
		return fmt.Errorf("%d batches violate finalized identity invariants", invalid)
	}
	return nil
}

func bytesOrNull(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func expandedSizes(h *fmsg.Header) error {
	size := func(path string) (uint32, error) {
		info, err := os.Stat(path)
		if err != nil {
			return 0, err
		}
		if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > int64(^uint32(0)) {
			return 0, fmt.Errorf("invalid payload size: %s", path)
		}
		return uint32(info.Size()), nil
	}
	var err error
	h.Size, err = size(h.Filepath)
	if err != nil {
		return err
	}
	for i := range h.Attachments {
		h.Attachments[i].Size, err = size(h.Attachments[i].Filepath)
		if err != nil {
			return err
		}
	}
	return nil
}
