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
	for _, table := range []string{"msg", "msg_to", "msg_add_to", "msg_attachment"} {
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

// attachAddToRecipients extends an already-stored message with the recipients
// carried by an add-to delivery, marking those on our domain delivered. It is
// the add-to counterpart of an INSERT: when a host already holds the shared
// message, an add-to delivery must grow its recipient list rather than insert
// a second row under the same unique canonical sha256 (SPEC §12).
func attachAddToRecipients(tx *sql.Tx, msgID int64, msg *FMsgHeader) error {
	now := timeutil.TimestampNow().Float64()

	add := func(table string, addr FMsgAddress) error {
		var delivered interface{}
		if addr.Domain == Domain {
			delivered = now
		}
		_, err := tx.Exec(`insert into `+table+` (msg_id, addr, time_delivered)
values ($1, $2, $3)
on conflict (msg_id, addr) do nothing`, msgID, addr.ToString(), delivered)
		return err
	}

	for _, addr := range msg.To {
		if err := add("msg_to", addr); err != nil {
			return err
		}
	}
	for _, addr := range msg.AddTo {
		if err := add("msg_add_to", addr); err != nil {
			return err
		}
	}

	if msg.AddToFrom != nil {
		if _, err := tx.Exec(`update msg set add_to_from = $1
where id = $2 and (add_to_from is null or add_to_from = '')`,
			msg.AddToFrom.ToString(), msgID); err != nil {
			return err
		}
	}
	return nil
}

func storeMsgDetail(msg *FMsgHeader) error {

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

	var addToFrom interface{}
	if msg.AddToFrom != nil {
		addToFrom = msg.AddToFrom.ToString()
	}

	var msgID int64
	err = tx.QueryRow(`insert into msg (version
	, no_reply
	, is_important
	, is_deflate
	, time_sent
	, from_addr
	, add_to_from
	, topic
	, type
	, sha256
	, psha256
	, size
	, filepath)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
returning id`,
		msg.Version,
		msg.Flags&FlagNoReply != 0,
		msg.Flags&FlagImportant != 0,
		msg.Flags&FlagDeflate != 0,
		msg.Timestamp,
		msg.From.ToString(),
		addToFrom,
		msg.Topic,
		msg.Type,
		msgHash,
		parentHash,
		int(msg.Size),
		msg.Filepath).Scan(&msgID)
	if err != nil {
		return err
	}

	stmt, err := tx.Prepare(`insert into msg_to (msg_id, addr, time_delivered)
values ($1, $2, $3)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := timeutil.TimestampNow().Float64()
	for _, addr := range msg.To {
		// recipients on our domain are already delivered; others are pending
		var delivered interface{}
		if addr.Domain == Domain {
			delivered = now
		}
		if _, err := stmt.Exec(msgID, addr.ToString(), delivered); err != nil {
			return err
		}
	}

	// insert add-to recipients into msg_add_to
	if len(msg.AddTo) > 0 {
		addToStmt, err := tx.Prepare(`insert into msg_add_to (msg_id, addr, time_delivered)
values ($1, $2, $3)`)
		if err != nil {
			return err
		}
		defer addToStmt.Close()

		for _, addr := range msg.AddTo {
			var delivered interface{}
			if addr.Domain == Domain {
				delivered = now
			}
			if _, err := addToStmt.Exec(msgID, addr.ToString(), delivered); err != nil {
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

	var addToFrom interface{}
	if msg.AddToFrom != nil {
		addToFrom = msg.AddToFrom.ToString()
	}

	var msgID int64
	err = tx.QueryRow(`insert into msg (version
	, no_reply
	, is_important
	, is_deflate
	, time_sent
	, from_addr
	, add_to_from
	, topic
	, type
	, sha256
	, psha256
	, size
	, filepath)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
returning id`,
		msg.Version,
		msg.Flags&FlagNoReply != 0,
		msg.Flags&FlagImportant != 0,
		msg.Flags&FlagDeflate != 0,
		msg.Timestamp,
		msg.From.ToString(),
		addToFrom,
		msg.Topic,
		msg.Type,
		msgHash,
		parentHash,
		int(msg.Size),
		"").Scan(&msgID)
	if err != nil {
		return err
	}

	// insert to recipients (for record keeping)
	toStmt, err := tx.Prepare(`insert into msg_to (msg_id, addr) values ($1, $2)`)
	if err != nil {
		return err
	}
	defer toStmt.Close()
	for _, addr := range msg.To {
		if _, err := toStmt.Exec(msgID, addr.ToString()); err != nil {
			return err
		}
	}

	// insert add-to recipients
	if len(msg.AddTo) > 0 {
		addToStmt, err := tx.Prepare(`insert into msg_add_to (msg_id, addr) values ($1, $2)`)
		if err != nil {
			return err
		}
		defer addToStmt.Close()
		for _, addr := range msg.AddTo {
			if _, err := addToStmt.Exec(msgID, addr.ToString()); err != nil {
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

// loadMsg loads a message and all its recipients from the database within the
// given transaction and returns a fully populated FMsgHeader.
func loadMsg(tx *sql.Tx, msgID int64) (*FMsgHeader, error) {
	var version, size int
	var noReply, isImportant, isDeflate bool
	var pid, msgHash []byte
	var fromAddr, topic, typ, filepath string
	var addToFromAddr sql.NullString
	var timeSent float64
	err := tx.QueryRow(`
		SELECT version, no_reply, is_important, is_deflate, psha256, sha256, from_addr, add_to_from, topic, type, time_sent, size, filepath
		FROM msg WHERE id = $1
	`, msgID).Scan(&version, &noReply, &isImportant, &isDeflate, &pid, &msgHash, &fromAddr, &addToFromAddr, &topic, &typ, &timeSent, &size, &filepath)
	if err != nil {
		return nil, fmt.Errorf("load msg %d: %w", msgID, err)
	}

	recipRows, err := tx.Query(`SELECT addr FROM msg_to WHERE msg_id = $1 ORDER BY id`, msgID)
	if err != nil {
		return nil, fmt.Errorf("load recipients for msg %d: %w", msgID, err)
	}
	var allRecipientAddrs []string
	for recipRows.Next() {
		var a string
		if err := recipRows.Scan(&a); err != nil {
			recipRows.Close()
			return nil, fmt.Errorf("scan recipient addr: %w", err)
		}
		allRecipientAddrs = append(allRecipientAddrs, a)
	}
	recipRows.Close()
	if err := recipRows.Err(); err != nil {
		return nil, fmt.Errorf("recipients query err for msg %d: %w", msgID, err)
	}

	from, err := parseAddress([]byte(fromAddr))
	if err != nil {
		return nil, fmt.Errorf("invalid from address %s: %w", fromAddr, err)
	}
	allTo := make([]FMsgAddress, 0, len(allRecipientAddrs))
	for _, a := range allRecipientAddrs {
		addr, err := parseAddress([]byte(a))
		if err != nil {
			return nil, fmt.Errorf("invalid to address %s: %w", a, err)
		}
		allTo = append(allTo, *addr)
	}

	// load add-to recipients from msg_add_to
	addToRows, err := tx.Query(`SELECT addr FROM msg_add_to WHERE msg_id = $1 ORDER BY id`, msgID)
	if err != nil {
		return nil, fmt.Errorf("load add-to recipients for msg %d: %w", msgID, err)
	}
	var allAddTo []FMsgAddress
	for addToRows.Next() {
		var a string
		if err := addToRows.Scan(&a); err != nil {
			addToRows.Close()
			return nil, fmt.Errorf("scan add-to addr: %w", err)
		}
		addr, err := parseAddress([]byte(a))
		if err != nil {
			addToRows.Close()
			return nil, fmt.Errorf("invalid add-to address %s: %w", a, err)
		}
		allAddTo = append(allAddTo, *addr)
	}
	addToRows.Close()
	if err := addToRows.Err(); err != nil {
		return nil, fmt.Errorf("add-to recipients query err for msg %d: %w", msgID, err)
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
	attachments := []FMsgAttachmentHeader{}
	for attRows.Next() {
		var flags, filesize int
		var typ, filename, filepath string
		if err := attRows.Scan(&flags, &typ, &filename, &filesize, &filepath); err != nil {
			attRows.Close()
			return nil, fmt.Errorf("scan attachment row: %w", err)
		}
		attachments = append(attachments, FMsgAttachmentHeader{
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

	// Compute flags bitfield from stored booleans and loaded data.
	// has_pid and has_add_to are derived from actual data rather than stored,
	// so add-to recipients added after the original message are included.
	//
	// When add-to recipients exist, the wire pid references the message being
	// shared, not that message's parent. This keeps add-to on replies pointing
	// at the reply payload rather than the root message.
	//
	// If that shared message has no persisted sha256 (e.g. it was delivered
	// locally only and so never sent over the wire), compute its original-form
	// message hash now. Without it the add-to wire header would omit the pid
	// field entirely and be rejected as invalid (SPEC §10.3 step 7, §12).
	if len(allAddTo) > 0 && len(msgHash) == 0 {
		origFlags := uint8(0)
		if noReply {
			origFlags |= FlagNoReply
		}
		if isImportant {
			origFlags |= FlagImportant
		}
		if isDeflate {
			origFlags |= FlagDeflate
		}
		if len(pid) > 0 {
			origFlags |= FlagHasPid
		}
		orig := &FMsgHeader{
			Version:     uint8(version),
			Flags:       origFlags,
			Pid:         pid,
			From:        *from,
			To:          allTo,
			Timestamp:   timeSent,
			Topic:       topic,
			Type:        typ,
			Size:        uint32(size),
			Attachments: attachments,
			Filepath:    filepath,
		}
		msgHash, err = orig.GetMessageHash()
		if err != nil {
			return nil, fmt.Errorf("compute original message hash for msg %d: %w", msgID, err)
		}
	}
	pid = wirePidForLoadedMessage(pid, msgHash, len(allAddTo) > 0)

	var addToFrom *FMsgAddress
	if addToFromAddr.Valid && addToFromAddr.String != "" {
		addr, err := parseAddress([]byte(addToFromAddr.String))
		if err != nil {
			return nil, fmt.Errorf("invalid add_to_from address %s: %w", addToFromAddr.String, err)
		}
		addToFrom = addr
	}
	if len(allAddTo) > 0 && addToFrom == nil {
		// Backward-compatibility for older rows before add_to_from existed.
		fallback := *from
		addToFrom = &fallback
	}

	var flags uint8
	if len(pid) > 0 {
		flags |= FlagHasPid
	}
	if len(allAddTo) > 0 {
		flags |= FlagHasAddTo
	}
	if noReply {
		flags |= FlagNoReply
	}
	if isImportant {
		flags |= FlagImportant
	}
	if isDeflate {
		flags |= FlagDeflate
	}

	return &FMsgHeader{
		Version:     uint8(version),
		Flags:       flags,
		Pid:         pid,
		From:        *from,
		To:          allTo,
		AddToFrom:   addToFrom,
		AddTo:       allAddTo,
		Timestamp:   timeSent,
		Topic:       topic,
		Type:        typ,
		Size:        uint32(size),
		Attachments: attachments,
		Filepath:    filepath,
	}, nil
}
