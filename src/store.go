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
	for _, table := range []string{"msg", "msg_to", "msg_attachment"} {
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
