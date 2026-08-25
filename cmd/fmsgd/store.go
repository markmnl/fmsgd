package main

import (
	"database/sql"
	"fmt"
	"log"
	"strings"

	"github.com/levenlabs/golib/timeutil"
	_ "github.com/lib/pq"
)

func testDb() error {
	db, err := sql.Open("postgres", "")
	if err != nil {
		return err
	}
	defer db.Close()
	err = db.Ping()
	if err != nil {
		return err
	}

	var dbName, user, host, port string
	_ = db.QueryRow("SELECT current_database()").Scan(&dbName)
	_ = db.QueryRow("SELECT current_user").Scan(&user)
	_ = db.QueryRow("SELECT inet_server_addr()::text").Scan(&host)
	_ = db.QueryRow("SELECT inet_server_port()::text").Scan(&port)
	log.Printf("INFO: Database connected: %s@%s:%s/%s", user, host, port, dbName)

	// verify required tables exist
	for _, table := range []string{"msg", "msg_to", "msg_add_to", "msg_add_to_batch", "msg_add_to_notify", "msg_attachment"} {
		var exists bool
		err = db.QueryRow(`SELECT EXISTS (
			SELECT FROM information_schema.tables
			WHERE table_name = $1
		)`, table).Scan(&exists)
		if err != nil {
			return fmt.Errorf("checking table %s: %w", table, err)
		}
		if !exists {
			return fmt.Errorf("required table %s does not exist", table)
		}
	}
	return nil
}

// lookupMsgIdByHash returns the msg id for a message with the given SHA256 hash,
// or 0 if no such message exists.
func lookupMsgIdByHash(hash []byte) (int64, error) {
	db, err := sql.Open("postgres", "")
	if err != nil {
		return 0, err
	}
	defer db.Close()

	var id int64
	err = db.QueryRow("SELECT id FROM msg WHERE sha256 = $1", hash).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// threadHasFromDomain reports whether any message in the thread rooted at hash
// (inclusive), following msg.pid (relational parent id), has a from_addr whose
// domain equals domain (case-insensitive). Used by HAS_NOT_PARTICIPATED
// challenge mode. Returns false when hash is not stored (cannot prove
// participation). Walks via relational pid, not add-to wire pids.
func threadHasFromDomain(hash []byte, domain string) (bool, error) {
	if len(hash) == 0 || domain == "" {
		return false, nil
	}

	db, err := sql.Open("postgres", "")
	if err != nil {
		return false, err
	}
	defer db.Close()

	// from_addr is @user@domain; split_part(..., 3) is the domain.
	// Depth cap and cycle guard limit pathological chains.
	var found bool
	err = db.QueryRow(`
		WITH RECURSIVE thread AS (
			SELECT id, from_addr, pid, 1 AS depth, ARRAY[id] AS seen
			FROM msg
			WHERE sha256 = $1
			UNION ALL
			SELECT m.id, m.from_addr, m.pid, t.depth + 1, t.seen || m.id
			FROM msg m
			INNER JOIN thread t ON m.id = t.pid
			WHERE t.depth < $3
			  AND NOT m.id = ANY (t.seen)
		)
		SELECT EXISTS (
			SELECT 1 FROM thread
			WHERE lower(split_part(from_addr, '@', 3)) = lower($2)
		)
	`, hash, domain, maxThreadWalkDepth).Scan(&found)
	if err != nil {
		return false, err
	}
	return found, nil
}

// hasAddrReceivedMsgHash reports whether addr has already received a stored
// message identified by hash.
func hasAddrReceivedMsgHash(hash []byte, addr *FMsgAddress) (bool, error) {
	if addr == nil || len(hash) == 0 {
		return false, nil
	}

	db, err := sql.Open("postgres", "")
	if err != nil {
		return false, err
	}
	defer db.Close()

	addrStr := strings.ToLower(addr.ToString())

	var exists bool
	err = db.QueryRow(`
		SELECT EXISTS (
			SELECT 1
			FROM msg m
			JOIN msg_to mt ON mt.msg_id = m.id
			WHERE m.sha256 = $1
			  AND lower(mt.addr) = $2
			  AND mt.time_delivered IS NOT NULL
			UNION ALL
			SELECT 1
			FROM msg m
			JOIN msg_add_to mat ON mat.msg_id = m.id
			WHERE m.sha256 = $1
			  AND lower(mat.addr) = $2
			  AND mat.time_delivered IS NOT NULL
		)
	`, hash, addrStr).Scan(&exists)
	if err != nil {
		return false, err
	}

	return exists, nil
}

type parentLinkStore interface {
	lookupParentID(parentHash []byte) (int64, error)
	setParentID(msgID int64, parentID int64) error
	setPendingChildrenParentID(parentID int64, parentHash []byte) error
}

type txParentLinkStore struct {
	tx *sql.Tx
}

func (s txParentLinkStore) lookupParentID(parentHash []byte) (int64, error) {
	var id int64
	err := s.tx.QueryRow("SELECT id FROM msg WHERE sha256 = $1", parentHash).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

func (s txParentLinkStore) setParentID(msgID int64, parentID int64) error {
	_, err := s.tx.Exec("UPDATE msg SET pid = $1 WHERE id = $2", parentID, msgID)
	return err
}

func (s txParentLinkStore) setPendingChildrenParentID(parentID int64, parentHash []byte) error {
	_, err := s.tx.Exec("UPDATE msg SET pid = $1 WHERE psha256 = $2 AND pid IS NULL", parentID, parentHash)
	return err
}

func resolveStoredParent(store parentLinkStore, msgID int64, parentHash []byte, requireParent bool) error {
	if len(parentHash) == 0 {
		return nil
	}

	parentID, err := store.lookupParentID(parentHash)
	if err != nil {
		return err
	}
	if parentID == 0 {
		if requireParent {
			return fmt.Errorf("parent message not found for psha256 %x", parentHash)
		}
		return nil
	}

	return store.setParentID(msgID, parentID)
}

func resolvePendingChildLinks(store parentLinkStore, parentID int64, parentHash []byte) error {
	if len(parentHash) == 0 {
		return nil
	}
	return store.setPendingChildrenParentID(parentID, parentHash)
}

func resolveMsgParentLinks(tx *sql.Tx, msgID int64, msgHash []byte, parentHash []byte, requireParent bool) error {
	store := txParentLinkStore{tx: tx}
	if err := resolveStoredParent(store, msgID, parentHash, requireParent); err != nil {
		return err
	}
	return resolvePendingChildLinks(store, msgID, msgHash)
}

func requiresStoredParent(msg *FMsgHeader) bool {
	return len(msg.Pid) > 0 && msg.Flags&FlagHasAddTo == 0
}

func wirePidForLoadedMessage(storedParentHash []byte, msgHash []byte, hasAddTo bool) []byte {
	if hasAddTo {
		return msgHash
	}
	return storedParentHash
}

// canonicalMsgHash returns the original-form message hash that is a message's
// stable identity: it is stored in msg.sha256 and is what replies carry as
// their wire pid. For an add-to message the row IS the shared message and its
// original-form hash is already in msg.Pid (the add-to wire pid, SPEC §12);
// GetMessageHash() there would instead hash the add-to variant and also needs
// the message payload, which the code-11 path never downloads.
func canonicalMsgHash(msg *FMsgHeader) ([]byte, error) {
	if msg.Flags&FlagHasAddTo != 0 {
		return msg.Pid, nil
	}
	return msg.GetMessageHash()
}

// relationalParentHash returns the hash of the message this one is a reply to,
// or nil when there is none. An add-to message's Pid is its own identity, not
// a parent pointer (SPEC §12), so it must never be resolved as a parent.
func relationalParentHash(msg *FMsgHeader) []byte {
	if msg.Flags&FlagHasAddTo != 0 {
		return nil
	}
	return msg.Pid
}

// getMsgByID loads a message and all its recipients from the database by msg ID.
// Returns the full FMsgHeader or nil if the message doesn't exist.
func getMsgByID(msgID int64) (*FMsgHeader, error) {
	db, err := sql.Open("postgres", "")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	h, err := loadMsg(tx, msgID)
	if err != nil {
		// If the message doesn't exist, loadMsg will return an error,
		// but we want to distinguish "not found" from other errors
		if err.Error() == "no rows in result set" || err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	return h, nil
}

// existingMsgIDForAddTo returns the id of an already-stored message row whose
// canonical sha256 matches msgHash, for an add-to delivery. It returns 0 when
// the message is not an add-to message or no such row exists, so the caller
// falls through to a normal INSERT. Non-add-to messages never take the attach
// path: a colliding sha256 there is a genuine duplicate, not a shared message.
func existingMsgIDForAddTo(tx *sql.Tx, msg *FMsgHeader, msgHash []byte) (int64, error) {
	if msg.Flags&FlagHasAddTo == 0 || len(msgHash) == 0 {
		return 0, nil
	}
	var id int64
	err := tx.QueryRow("SELECT id FROM msg WHERE sha256 = $1", msgHash).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// addToBatchRecorded reports whether this host has already recorded an add-to
// batch with this batch message hash against stored message msgID. Batch
// identity IS the batch message hash, which covers time (SPEC §11): the same
// addresses re-issued at a new time hash differently and are a distinct
// batch, not a duplicate (SPEC §12).
func addToBatchRecorded(msgID int64, batchHash []byte) (bool, error) {
	if len(batchHash) == 0 {
		return false, nil
	}
	db, err := sql.Open("postgres", "")
	if err != nil {
		return false, err
	}
	defer db.Close()

	var exists bool
	err = db.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM msg_add_to_batch WHERE msg_id = $1 AND sha256 = $2
	)`, msgID, batchHash).Scan(&exists)
	return exists, err
}

// insertAddToBatch records one add-to delivery as a batch (its sender, the
// time this host recorded it, and its identifying batch message hash, SPEC
// §11) and returns the new batch id. Recipients carried by the delivery are
// linked to this batch so readers can reconstruct who added which recipients
// and when (SPEC §12).
func insertAddToBatch(tx *sql.Tx, msgID int64, addToFrom string, now float64, batchHash []byte) (int64, error) {
	var batchID int64
	err := tx.QueryRow(`insert into msg_add_to_batch (msg_id, add_to_from, time_added, sha256)
values ($1, $2, $3, $4) returning id`, msgID, addToFrom, now, batchHash).Scan(&batchID)
	return batchID, err
}

// attachAddToRecipients extends an already-stored message with the recipients
// carried by an add-to delivery, marking those on our domain delivered. It is
// the add-to counterpart of an INSERT: when a host already holds the shared
// message, an add-to delivery must grow its recipient list rather than insert
// a second row under the same unique canonical sha256 (SPEC §12). Each call is
// one batch, recorded so its sender and timing are not lost.
func attachAddToRecipients(tx *sql.Tx, msgID int64, msg *FMsgHeader) error {
	now := timeutil.TimestampNow().Float64()

	// This host is recording a batch it RECEIVED: delivering the batch to
	// other domains is the batch sender's job, not ours (SPEC §10.2 — a host
	// delivers iff from or add to from belongs to its domain). Rows for other
	// domains are recorded with localResponseCodeNotOurDelivery so the
	// sender's pending queries never treat them as our delivery work.
	for _, addr := range msg.To {
		var delivered interface{}
		var code interface{}
		if addr.Domain == Domain {
			delivered = now
		} else {
			code = int16(localResponseCodeNotOurDelivery)
		}
		if _, err := tx.Exec(`insert into msg_to (msg_id, addr, time_delivered, response_code)
values ($1, $2, $3, $4)
on conflict (msg_id, addr) do nothing`, msgID, addr.ToString(), delivered, code); err != nil {
			return err
		}
	}

	if len(msg.AddTo) == 0 {
		return nil
	}

	addToFrom := ""
	if msg.AddToFrom != nil {
		addToFrom = msg.AddToFrom.ToString()
	}
	// Cached since the header exchange computed it against the stored
	// parent's payload (handleAddToPath); it is the batch's identity (SPEC
	// §11) and what duplicate detection compares against.
	batchHash, err := msg.GetMessageHash()
	if err != nil {
		return fmt.Errorf("compute add-to batch hash: %w", err)
	}
	batchID, err := insertAddToBatch(tx, msgID, addToFrom, now, batchHash)
	if err != nil {
		return err
	}

	for _, addr := range msg.AddTo {
		var delivered interface{}
		var code interface{}
		if addr.Domain == Domain {
			delivered = now
		} else {
			code = int16(localResponseCodeNotOurDelivery)
		}
		if _, err := tx.Exec(`insert into msg_add_to (msg_id, batch_id, addr, time_delivered, response_code)
values ($1, $2, $3, $4, $5)
on conflict (batch_id, addr) do nothing`, msgID, batchID, addr.ToString(), delivered, code); err != nil {
			return err
		}
	}
	return nil
}

// inboundRecipientRow maps one wire recipient of a received message to its
// stored row state: recipients this host accepted are delivered; recipients
// this host rejected keep the per-recipient code it responded; recipients on
// other domains are recorded for participant checks and faithful message
// reconstruction (SPEC §10.3/§11) but are another host's delivery duty.
func inboundRecipientRow(addr FMsgAddress, localOutcome map[string]uint8, now float64) (delivered interface{}, code interface{}) {
	c, ok := localOutcome[strings.ToLower(addr.ToString())]
	if !ok {
		return nil, int16(localResponseCodeNotOurDelivery)
	}
	if c == RejectCodeAccept {
		return now, nil
	}
	return nil, int16(c)
}

// storeMsgDetail stores a RECEIVED message: the complete wire recipient lists
// are kept in wire order — never truncated to this host's recipients —
// because §10.3's participant checks and §11's hash reconstruction both need
// the message exactly as transmitted. localOutcome maps each lower-cased
// local recipient address to the per-recipient code this host responded.
func storeMsgDetail(msg *FMsgHeader, localOutcome map[string]uint8) error {

	db, err := sql.Open("postgres", "")
	if err != nil {
		return err
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	msgHash, err := canonicalMsgHash(msg)
	if err != nil {
		return err
	}
	parentHash := relationalParentHash(msg)

	// An add-to delivery for a message this host already holds extends the
	// existing row's recipient list; inserting again would collide on the
	// unique canonical sha256 (SPEC §12).
	if existingID, err := existingMsgIDForAddTo(tx, msg, msgHash); err != nil {
		return err
	} else if existingID != 0 {
		if err := attachAddToRecipients(tx, existingID, msg); err != nil {
			return err
		}
		return tx.Commit()
	}

	var msgID int64
	err = tx.QueryRow(`insert into msg (version
	, no_reply
	, is_important
	, is_deflate
	, time_sent
	, from_addr
	, topic
	, type
	, sha256
	, psha256
	, size
	, filepath
	, wire_header)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
returning id`,
		msg.Version,
		msg.Flags&FlagNoReply != 0,
		msg.Flags&FlagImportant != 0,
		msg.Flags&FlagDeflate != 0,
		msg.Timestamp,
		msg.From.ToString(),
		msg.Topic,
		msg.Type,
		msgHash,
		parentHash,
		int(msg.Size),
		msg.Filepath,
		msg.Encode()).Scan(&msgID)
	if err != nil {
		return err
	}

	stmt, err := tx.Prepare(`insert into msg_to (msg_id, addr, time_delivered, response_code)
values ($1, $2, $3, $4)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := timeutil.TimestampNow().Float64()
	for _, addr := range msg.To {
		delivered, code := inboundRecipientRow(addr, localOutcome, now)
		if _, err := stmt.Exec(msgID, addr.ToString(), delivered, code); err != nil {
			return err
		}
	}

	// insert add-to recipients into msg_add_to as one batch
	if len(msg.AddTo) > 0 {
		addToFrom := ""
		if msg.AddToFrom != nil {
			addToFrom = msg.AddToFrom.ToString()
		}
		// Cached from download verification: the wire hash of an add-to
		// message is its batch hash, the batch's identity (SPEC §11).
		batchHash, err := msg.GetMessageHash()
		if err != nil {
			return fmt.Errorf("compute add-to batch hash: %w", err)
		}
		batchID, err := insertAddToBatch(tx, msgID, addToFrom, now, batchHash)
		if err != nil {
			return err
		}

		addToStmt, err := tx.Prepare(`insert into msg_add_to (msg_id, batch_id, addr, time_delivered, response_code)
values ($1, $2, $3, $4, $5)`)
		if err != nil {
			return err
		}
		defer addToStmt.Close()

		for _, addr := range msg.AddTo {
			delivered, code := inboundRecipientRow(addr, localOutcome, now)
			if _, err := addToStmt.Exec(msgID, batchID, addr.ToString(), delivered, code); err != nil {
				return err
			}
		}
	}

	if len(msg.Attachments) > 0 {
		attStmt, err := tx.Prepare(`insert into msg_attachment (msg_id, position, flags, type, filename, filesize, filepath)
values ($1, $2, $3, $4, $5, $6, $7)`)
		if err != nil {
			return err
		}
		defer attStmt.Close()

		for i := range msg.Attachments {
			att := msg.Attachments[i]
			if _, err := attStmt.Exec(msgID, i, int(att.Flags), att.Type, att.Filename, int(att.Size), att.Filepath); err != nil {
				return err
			}
		}
	}

	if err := resolveMsgParentLinks(tx, msgID, msgHash, parentHash, requiresStoredParent(msg)); err != nil {
		return err
	}

	return tx.Commit()

}

// storeMsgHeaderOnly stores just the message header for add-to notifications
// (spec code 11). Only the header is recorded so the header hash can be
// faithfully computed for subsequent messages referencing this one via pid.
func storeMsgHeaderOnly(msg *FMsgHeader) error {
	db, err := sql.Open("postgres", "")
	if err != nil {
		return err
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	msgHash, err := canonicalMsgHash(msg)
	if err != nil {
		return err
	}
	parentHash := relationalParentHash(msg)

	// An add-to delivery for a message this host already holds extends the
	// existing row's recipient list; inserting again would collide on the
	// unique canonical sha256 (SPEC §12).
	if existingID, err := existingMsgIDForAddTo(tx, msg, msgHash); err != nil {
		return err
	} else if existingID != 0 {
		if err := attachAddToRecipients(tx, existingID, msg); err != nil {
			return err
		}
		return tx.Commit()
	}

	var msgID int64
	err = tx.QueryRow(`insert into msg (version
	, no_reply
	, is_important
	, is_deflate
	, time_sent
	, from_addr
	, topic
	, type
	, sha256
	, psha256
	, size
	, filepath
	, wire_header)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
returning id`,
		msg.Version,
		msg.Flags&FlagNoReply != 0,
		msg.Flags&FlagImportant != 0,
		msg.Flags&FlagDeflate != 0,
		msg.Timestamp,
		msg.From.ToString(),
		msg.Topic,
		msg.Type,
		msgHash,
		parentHash,
		int(msg.Size),
		"",
		msg.Encode()).Scan(&msgID)
	if err != nil {
		return err
	}

	// Record-keeping rows only: no delivery happened for anyone here, so every
	// recipient is marked not-our-delivery.
	toStmt, err := tx.Prepare(`insert into msg_to (msg_id, addr, response_code) values ($1, $2, $3)`)
	if err != nil {
		return err
	}
	defer toStmt.Close()
	for _, addr := range msg.To {
		if _, err := toStmt.Exec(msgID, addr.ToString(), int16(localResponseCodeNotOurDelivery)); err != nil {
			return err
		}
	}

	// insert add-to recipients as one batch
	if len(msg.AddTo) > 0 {
		addToFrom := ""
		if msg.AddToFrom != nil {
			addToFrom = msg.AddToFrom.ToString()
		}
		// No stored parent payload here, so the batch hash cannot be computed.
		batchID, err := insertAddToBatch(tx, msgID, addToFrom, timeutil.TimestampNow().Float64(), nil)
		if err != nil {
			return err
		}

		addToStmt, err := tx.Prepare(`insert into msg_add_to (msg_id, batch_id, addr, response_code) values ($1, $2, $3, $4)`)
		if err != nil {
			return err
		}
		defer addToStmt.Close()
		for _, addr := range msg.AddTo {
			if _, err := addToStmt.Exec(msgID, batchID, addr.ToString(), int16(localResponseCodeNotOurDelivery)); err != nil {
				return err
			}
		}
	}

	if len(msg.Attachments) > 0 {
		attStmt, err := tx.Prepare(`insert into msg_attachment (msg_id, position, flags, type, filename, filesize, filepath)
values ($1, $2, $3, $4, $5, $6, $7)`)
		if err != nil {
			return err
		}
		defer attStmt.Close()

		for i := range msg.Attachments {
			att := msg.Attachments[i]
			if _, err := attStmt.Exec(msgID, i, int(att.Flags), att.Type, att.Filename, int(att.Size), att.Filepath); err != nil {
				return err
			}
		}
	}

	if err := resolveMsgParentLinks(tx, msgID, msgHash, parentHash, requiresStoredParent(msg)); err != nil {
		return err
	}

	return tx.Commit()
}

// msgFields holds a message's stored columns plus its original recipients and
// attachments: the raw material for building the message's wire headers. Add-to
// recipients are NOT included here — they belong to batches (see addToBatch),
// each of which is delivered as its own add-to message (SPEC §12).
type msgFields struct {
	version                         int
	size                            int
	noReply, isImportant, isDeflate bool
	parentPid                       []byte // relational parent hash (stored pid column)
	storedHash                      []byte // stored sha256; empty when not yet persisted
	from                            FMsgAddress
	to                              []FMsgAddress
	attachments                     []FMsgAttachmentHeader
	timeSent                        float64
	topic, typ                      string
	filepath                        string
}

// loadMsgFields reads the msg row, its msg_to recipients and its attachments.
func loadMsgFields(tx *sql.Tx, msgID int64) (*msgFields, error) {
	var m msgFields
	var fromAddr string
	if err := tx.QueryRow(`
		SELECT version, no_reply, is_important, is_deflate, psha256, sha256, from_addr, topic, type, time_sent, size, filepath
		FROM msg WHERE id = $1
	`, msgID).Scan(&m.version, &m.noReply, &m.isImportant, &m.isDeflate, &m.parentPid, &m.storedHash,
		&fromAddr, &m.topic, &m.typ, &m.timeSent, &m.size, &m.filepath); err != nil {
		return nil, fmt.Errorf("load msg %d: %w", msgID, err)
	}

	from, err := parseAddress([]byte(fromAddr))
	if err != nil {
		return nil, fmt.Errorf("invalid from address %s: %w", fromAddr, err)
	}
	m.from = *from

	m.to, err = loadRecipientAddrs(tx, `SELECT addr FROM msg_to WHERE msg_id = $1 ORDER BY id`, msgID)
	if err != nil {
		return nil, fmt.Errorf("load recipients for msg %d: %w", msgID, err)
	}

	attRows, err := tx.Query(`
		SELECT flags, type, filename, filesize, filepath
		FROM msg_attachment
		WHERE msg_id = $1
		ORDER BY position, filename
	`, msgID)
	if err != nil {
		return nil, fmt.Errorf("load attachments for msg %d: %w", msgID, err)
	}
	m.attachments = []FMsgAttachmentHeader{}
	for attRows.Next() {
		var flags, filesize int
		var typ, filename, filepath string
		if err := attRows.Scan(&flags, &typ, &filename, &filesize, &filepath); err != nil {
			attRows.Close()
			return nil, fmt.Errorf("scan attachment row: %w", err)
		}
		m.attachments = append(m.attachments, FMsgAttachmentHeader{
			Flags:    uint8(flags),
			Type:     typ,
			Filename: filename,
			Size:     uint32(filesize),
			Filepath: filepath,
		})
	}
	attRows.Close()
	if err := attRows.Err(); err != nil {
		return nil, fmt.Errorf("attachments query err for msg %d: %w", msgID, err)
	}
	return &m, nil
}

// loadRecipientAddrs runs a single-column address query and parses the results.
func loadRecipientAddrs(tx *sql.Tx, query string, msgID int64) ([]FMsgAddress, error) {
	rows, err := tx.Query(query, msgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var addrs []FMsgAddress
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		addr, err := parseAddress([]byte(a))
		if err != nil {
			return nil, fmt.Errorf("invalid address %s: %w", a, err)
		}
		addrs = append(addrs, *addr)
	}
	return addrs, rows.Err()
}

// baseFlags returns the persisted flag bits (no_reply/important/deflate) shared
// by every wire form of the message.
func (m *msgFields) baseFlags() uint8 {
	var f uint8
	if m.noReply {
		f |= FlagNoReply
	}
	if m.isImportant {
		f |= FlagImportant
	}
	if m.isDeflate {
		f |= FlagDeflate
	}
	return f
}

// originalHeader builds the message in its original (non-add-to) wire form,
// whose pid (if any) references the relational parent.
func (m *msgFields) originalHeader() *FMsgHeader {
	flags := m.baseFlags()
	if len(m.parentPid) > 0 {
		flags |= FlagHasPid
	}
	return &FMsgHeader{
		Version:     uint8(m.version),
		Flags:       flags,
		Pid:         m.parentPid,
		From:        m.from,
		To:          m.to,
		Timestamp:   m.timeSent,
		Topic:       m.topic,
		Type:        m.typ,
		Size:        uint32(m.size),
		Attachments: m.attachments,
		Filepath:    m.filepath,
	}
}

// sharedHash returns the canonical hash identifying this message: its persisted
// sha256 (computed at first outbound delivery over the header exactly as
// transmitted — deflated form; see the sender), or — when not yet persisted
// (e.g. local-only delivery, where nothing external can reference it) — its
// undeflated original-form hash as a local fallback. Add-to batches reference
// this value as their pid (SPEC §12).
func (m *msgFields) sharedHash() ([]byte, error) {
	if len(m.storedHash) > 0 {
		return m.storedHash, nil
	}
	return m.originalHeader().GetMessageHash()
}

// addToHeader builds the wire header that delivers one add-to batch: a duplicate
// of the original message carrying this batch's sender, recipients and
// timestamp, with pid set to the shared message hash (SPEC §12). A fresh header
// is returned each call so hash caches never cross between batches.
func (m *msgFields) addToHeader(batch addToBatch, sharedHash []byte) *FMsgHeader {
	h := m.originalHeader()
	h.Flags |= FlagHasPid | FlagHasAddTo
	h.Pid = sharedHash
	from := batch.From
	h.AddToFrom = &from
	h.AddTo = batch.Recipients
	h.Timestamp = batch.TimeAdded
	h.Topic = "" // pid is present, so topic is omitted on the wire
	return h
}

// addToBatch is one add-to delivery: a single sender added a set of recipients
// at a point in time (SPEC §12).
type addToBatch struct {
	ID         int64
	From       FMsgAddress
	TimeAdded  float64
	Recipients []FMsgAddress
}

// loadAddToBatches returns every add-to batch for a message, each with its
// sender, timestamp and recipients, ordered by when it was added.
func loadAddToBatches(tx *sql.Tx, msgID int64) ([]addToBatch, error) {
	rows, err := tx.Query(`
		SELECT b.id, b.add_to_from, b.time_added, a.addr
		FROM msg_add_to_batch b
		LEFT JOIN msg_add_to a ON a.batch_id = b.id
		WHERE b.msg_id = $1
		ORDER BY b.time_added, b.id, a.id
	`, msgID)
	if err != nil {
		return nil, fmt.Errorf("load add-to batches for msg %d: %w", msgID, err)
	}
	defer rows.Close()

	var batches []addToBatch
	byID := make(map[int64]int) // batch id -> index in batches
	for rows.Next() {
		var id int64
		var fromStr string
		var timeAdded float64
		var addr sql.NullString
		if err := rows.Scan(&id, &fromStr, &timeAdded, &addr); err != nil {
			return nil, fmt.Errorf("scan add-to batch row: %w", err)
		}
		idx, ok := byID[id]
		if !ok {
			from, err := parseAddress([]byte(fromStr))
			if err != nil {
				return nil, fmt.Errorf("invalid add_to_from address %s: %w", fromStr, err)
			}
			batches = append(batches, addToBatch{ID: id, From: *from, TimeAdded: timeAdded})
			idx = len(batches) - 1
			byID[id] = idx
		}
		if addr.Valid {
			a, err := parseAddress([]byte(addr.String))
			if err != nil {
				return nil, fmt.Errorf("invalid add-to address %s: %w", addr.String, err)
			}
			batches[idx].Recipients = append(batches[idx].Recipients, *a)
		}
	}
	return batches, rows.Err()
}

// loadMsg loads a message for inspection (participant and retrievability
// checks): the original header plus the flat union of every add-to recipient.
// AddToFrom is deliberately left nil — a stored message may aggregate batches
// from several senders, and each add_to_from is validated to be in From or To
// (SPEC §12), so participant checks need only From/To/AddTo. Outgoing delivery
// never uses this merged form; it builds a separate header per batch.
func loadMsg(tx *sql.Tx, msgID int64) (*FMsgHeader, error) {
	m, err := loadMsgFields(tx, msgID)
	if err != nil {
		return nil, err
	}

	addTo, err := loadRecipientAddrs(tx, `SELECT addr FROM msg_add_to WHERE msg_id = $1 ORDER BY id`, msgID)
	if err != nil {
		return nil, fmt.Errorf("load add-to recipients for msg %d: %w", msgID, err)
	}

	h := m.originalHeader()
	if len(addTo) > 0 {
		// The wire pid of an add-to message references the shared message, not
		// that message's relational parent (SPEC §12).
		sharedHash, err := m.sharedHash()
		if err != nil {
			return nil, fmt.Errorf("compute shared hash for msg %d: %w", msgID, err)
		}
		h.Flags |= FlagHasPid | FlagHasAddTo
		h.Pid = sharedHash
		h.AddTo = addTo
	}
	return h, nil
}
