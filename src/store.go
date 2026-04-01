package main

import (
	"database/sql"
	"fmt"
	"log"

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
	, flags
	, time_sent
	, from_addr
	, topic
	, type
	, sha256
	, psha256
	, size
	, filepath)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
returning id`,
		msg.Version,
		msg.Flags,
		msg.Timestamp,
		msg.From.ToString(),
		msg.Topic,
		msg.Type,
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

	// resolve pid from psha256 (parent message hash)
	if len(msg.Pid) > 0 {
		var parentID sql.NullInt64
		err = tx.QueryRow("SELECT id FROM msg WHERE sha256 = $1", msg.Pid).Scan(&parentID)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if parentID.Valid {
			if _, err = tx.Exec("UPDATE msg SET pid = $1 WHERE id = $2", parentID.Int64, msgID); err != nil {
				return err
			}
		}
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

	headerHash := msg.GetHeaderHash()

	var msgID int64
	err = tx.QueryRow(`insert into msg (version
	, flags
	, time_sent
	, from_addr
	, topic
	, type
	, sha256
	, psha256
	, size
	, filepath)
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
returning id`,
		msg.Version,
		msg.Flags,
		msg.Timestamp,
		msg.From.ToString(),
		msg.Topic,
		msg.Type,
		headerHash,
		msg.Pid,
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

	// resolve pid from psha256
	if len(msg.Pid) > 0 {
		var parentID sql.NullInt64
		err = tx.QueryRow("SELECT id FROM msg WHERE sha256 = $1", msg.Pid).Scan(&parentID)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if parentID.Valid {
			if _, err = tx.Exec("UPDATE msg SET pid = $1 WHERE id = $2", parentID.Int64, msgID); err != nil {
				return err
			}
		}
	}

	return tx.Commit()
}

// loadMsg loads a message and all its recipients from the database within the
// given transaction and returns a fully populated FMsgHeader.
//
// TODO [Spec]: Attachment headers are not loaded. Once the msg_attachment table
// stores attachment metadata (flags, type, filename, size, filepath), loadMsg
// should populate FMsgHeader.Attachments so the sender can write attachment
// headers and data on the wire and compute a correct header/message hash.
func loadMsg(tx *sql.Tx, msgID int64) (*FMsgHeader, error) {
	var version, flags, size int
	var pid []byte
	var fromAddr, topic, typ, filepath string
	var timeSent float64
	err := tx.QueryRow(`
		SELECT version, flags, psha256, from_addr, topic, type, time_sent, size, filepath
		FROM msg WHERE id = $1
	`, msgID).Scan(&version, &flags, &pid, &fromAddr, &topic, &typ, &timeSent, &size, &filepath)
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

	return &FMsgHeader{
		Version:   uint8(version),
		Flags:     uint8(flags),
		Pid:       pid,
		From:      *from,
		To:        allTo,
		AddTo:     allAddTo,
		Timestamp: timeSent,
		Topic:     topic,
		Type:      typ,
		Size:      uint32(size),
		Filepath:  filepath,
	}, nil
}
