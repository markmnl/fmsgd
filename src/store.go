package main

import (
	"database/sql"
	"fmt"
	"log"

	"github.com/levenlabs/golib/timeutil"
	_ "github.com/lib/pq"
)

// commonTypeParam returns the common media type number (as *int16) if the
// FlagCommonType flag is set on msg, or nil otherwise. Used as the SQL
// parameter for the nullable common_type column.
func commonTypeParam(msg *FMsgHeader) interface{} {
	if msg.Flags&FlagCommonType != 0 {
		if num, ok := mediaTypeToNumber[msg.Type]; ok {
			v := int16(num)
			return &v
		}
	}
	return nil
}

// typeParam returns the type string when common type flag is NOT set, or nil
// when common type flag IS set (the number in common_type is sufficient).
func typeParam(msg *FMsgHeader) interface{} {
	if msg.Flags&FlagCommonType != 0 {
		return nil
	}
	return msg.Type
}

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
	for _, table := range []string{"msg", "msg_to", "msg_add_to_batch", "msg_add_to", "msg_attachment"} {
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
// or 0 if no such message exists. Checks both msg.sha256 and
// msg_add_to_batch.sha256 (add-to batches produce distinct hashes that can be
// referenced as pid by future messages).
func lookupMsgIdByHash(hash []byte) (int64, error) {
	db, err := sql.Open("postgres", "")
	if err != nil {
		return 0, err
	}
	defer db.Close()

	var id int64
	err = db.QueryRow("SELECT id FROM msg WHERE sha256 = $1", hash).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}

	// check add-to batch hashes
	err = db.QueryRow("SELECT msg_id FROM msg_add_to_batch WHERE sha256 = $1", hash).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
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

	msgHash, err := msg.GetMessageHash()
	if err != nil {
		return err
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
	, common_type
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
		msg.Topic,
		typeParam(msg),
		commonTypeParam(msg),
		msgHash,
		msg.Pid,
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

	// insert add-to batch and recipients
	if len(msg.AddTo) > 0 {
		addToHash, err := msg.GetMessageHash()
		if err != nil {
			return err
		}

		// determine next batch_no for this msg
		var batchNo int
		err = tx.QueryRow(`SELECT coalesce(max(batch_no), 0) + 1 FROM msg_add_to_batch WHERE msg_id = $1`, msgID).Scan(&batchNo)
		if err != nil {
			return err
		}

		var batchID int64
		err = tx.QueryRow(`INSERT INTO msg_add_to_batch (msg_id, batch_no, sha256) VALUES ($1, $2, $3) RETURNING id`,
			msgID, batchNo, addToHash).Scan(&batchID)
		if err != nil {
			return err
		}

		addToStmt, err := tx.Prepare(`INSERT INTO msg_add_to (batch_id, addr, time_delivered) VALUES ($1, $2, $3)`)
		if err != nil {
			return err
		}
		defer addToStmt.Close()

		for _, addr := range msg.AddTo {
			var delivered interface{}
			if addr.Domain == Domain {
				delivered = now
			}
			if _, err := addToStmt.Exec(batchID, addr.ToString(), delivered); err != nil {
				return err
			}
		}
	}

	// resolve pid from psha256 (parent message hash) — check both msg.sha256
	// and msg_add_to_batch.sha256 since a reply can reference an add-to batch
	if len(msg.Pid) > 0 {
		var parentID sql.NullInt64
		err = tx.QueryRow("SELECT id FROM msg WHERE sha256 = $1", msg.Pid).Scan(&parentID)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if !parentID.Valid {
			err = tx.QueryRow("SELECT msg_id FROM msg_add_to_batch WHERE sha256 = $1", msg.Pid).Scan(&parentID)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
		}
		if parentID.Valid {
			if _, err = tx.Exec("UPDATE msg SET pid = $1 WHERE id = $2", parentID.Int64, msgID); err != nil {
				return err
			}
		}
	}

	return tx.Commit()

}

// storeAddToBatch stores a batch of add-to recipients for an existing message.
// Called when we receive an add-to notification (code 11) — the parent message
// is already stored, we just need to record the new add-to recipients and the
// header hash so future messages can reference this version via pid.
func storeAddToBatch(msgID int64, msg *FMsgHeader) error {
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

	headerHash := msg.GetHeaderHash()

	// determine next batch_no for this msg
	var batchNo int
	err = tx.QueryRow(`SELECT coalesce(max(batch_no), 0) + 1 FROM msg_add_to_batch WHERE msg_id = $1`, msgID).Scan(&batchNo)
	if err != nil {
		return err
	}

	var batchID int64
	err = tx.QueryRow(`INSERT INTO msg_add_to_batch (msg_id, batch_no, sha256) VALUES ($1, $2, $3) RETURNING id`,
		msgID, batchNo, headerHash).Scan(&batchID)
	if err != nil {
		return err
	}

	addToStmt, err := tx.Prepare(`INSERT INTO msg_add_to (batch_id, addr) VALUES ($1, $2)`)
	if err != nil {
		return err
	}
	defer addToStmt.Close()
	for _, addr := range msg.AddTo {
		if _, err := addToStmt.Exec(batchID, addr.ToString()); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// loadMsg loads a message and its recipients from the database within the
// given transaction and returns a fully populated FMsgHeader.
//
// batchNo controls add-to recipient loading:
//   - 0: load the original message without any add-to recipients
//   - >0: load add-to recipients from the specified batch only
//
// When batchNo > 0, pid is overridden with the msg row's own sha256
// (the original message hash) since add-to messages reference the original.
// TODO [Spec]: Attachment headers are not loaded. Once the msg_attachment table
// stores attachment metadata (flags, type, filename, size, filepath), loadMsg
// should populate FMsgHeader.Attachments so the sender can write attachment
// headers and data on the wire and compute a correct header/message hash.
func loadMsg(tx *sql.Tx, msgID int64, batchNo int) (*FMsgHeader, error) {
	var version, size int
	var noReply, isImportant, isDeflate bool
	var pid, msgHash []byte
	var fromAddr, topic, filepath string
	var typ sql.NullString
	var commonType sql.NullInt16
	var timeSent float64
	err := tx.QueryRow(`
		SELECT version, no_reply, is_important, is_deflate, psha256, sha256, from_addr, topic, type, common_type, time_sent, size, filepath
		FROM msg WHERE id = $1
	`, msgID).Scan(&version, &noReply, &isImportant, &isDeflate, &pid, &msgHash, &fromAddr, &topic, &typ, &commonType, &timeSent, &size, &filepath)
	if err != nil {
		return nil, fmt.Errorf("load msg %d: %w", msgID, err)
	}

	// Resolve the type string: prefer the stored string; fall back to common type number.
	var resolvedType string
	if typ.Valid {
		resolvedType = typ.String
	} else if commonType.Valid {
		resolvedType = numberToMediaType[uint8(commonType.Int16)]
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

	// load add-to recipients up to the specified batch number
	var allAddTo []FMsgAddress
	if batchNo > 0 {
		addToRows, err := tx.Query(`SELECT a.addr FROM msg_add_to a
			JOIN msg_add_to_batch b ON a.batch_id = b.id
			WHERE b.msg_id = $1 AND b.batch_no = $2
			ORDER BY a.id`, msgID, batchNo)
		if err != nil {
			return nil, fmt.Errorf("load add-to recipients for msg %d batch %d: %w", msgID, batchNo, err)
		}
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
	}

	// When loading an add-to batch, pid is the original message's own hash
	// (not its parent), since the add-to message references the original.
	if batchNo > 0 {
		pid = msgHash
	}

	// Compute flags bitfield from stored booleans and loaded data.
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
	if commonType.Valid {
		flags |= FlagCommonType
	}

	return &FMsgHeader{
		Version:   uint8(version),
		Flags:     flags,
		Pid:       pid,
		From:      *from,
		To:        allTo,
		AddTo:     allAddTo,
		Timestamp: timeSent,
		Topic:     topic,
		Type:      resolvedType,
		Size:      uint32(size),
		Filepath:  filepath,
	}, nil
}
