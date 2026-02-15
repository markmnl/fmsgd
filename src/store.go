package main

import (
	"database/sql"
	"fmt"
	"log"
	"strings"

	"github.com/levenlabs/golib/timeutil"
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

// PendingRecipient represents a single recipient of a message that still needs delivery.
type PendingRecipient struct {
	MsgID int64
	Addr  string
}

// PendingMessage holds everything needed to deliver a message to one or more recipients
// on the same domain.
type PendingMessage struct {
	MsgID     int64
	Version   uint8
	Flags     uint8
	Pid       []byte
	FromAddr  string
	Topic     string
	Type      string
	Timestamp float64
	Size      int
	Filepath  string
	// Recipients to deliver to (all on the same domain when grouped by caller)
	Recipients []PendingRecipient
}

// selectPendingMessages returns messages that still need delivery:
//   - time_delivered is null
//   - response_code is null, 3 (undisclosed) or 5 (insufficient resources) — i.e. retryable
//   - enough time has elapsed since time_last_attempt (retryInterval seconds)
func selectPendingMessages(retryInterval float64) ([]PendingMessage, error) {
	db, err := sql.Open("postgres", "")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	now := timeutil.TimestampNow().Float64()

	rows, err := db.Query(`
		SELECT m.id, m.version, m.flags, m.pid, m.from_addr, m.topic, m.type,
		       m.time_sent, m.size, m.filepath, mt.addr
		FROM msg m
		JOIN msg_to mt ON mt.msg_id = m.id
		WHERE mt.time_delivered IS NULL
		  AND (mt.response_code IS NULL OR mt.response_code IN (3, 5))
		  AND (mt.time_last_attempt IS NULL OR ($1 - mt.time_last_attempt) > $2)
		ORDER BY m.id, mt.addr
	`, now, retryInterval)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// group by msg_id
	msgMap := make(map[int64]*PendingMessage)
	var order []int64
	for rows.Next() {
		var (
			msgID    int64
			version  int
			flags    int
			pid      []byte
			fromAddr string
			topic    string
			typ      string
			timeSent float64
			size     int
			filepath string
			addr     string
		)
		if err := rows.Scan(&msgID, &version, &flags, &pid, &fromAddr, &topic, &typ,
			&timeSent, &size, &filepath, &addr); err != nil {
			return nil, err
		}
		pm, exists := msgMap[msgID]
		if !exists {
			pm = &PendingMessage{
				MsgID:     msgID,
				Version:   uint8(version),
				Flags:     uint8(flags),
				Pid:       pid,
				FromAddr:  fromAddr,
				Topic:     topic,
				Type:      typ,
				Timestamp: timeSent,
				Size:      size,
				Filepath:  filepath,
			}
			msgMap[msgID] = pm
			order = append(order, msgID)
		}
		pm.Recipients = append(pm.Recipients, PendingRecipient{MsgID: msgID, Addr: addr})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	result := make([]PendingMessage, 0, len(order))
	for _, id := range order {
		result = append(result, *msgMap[id])
	}
	return result, nil
}

// updateDelivered marks a recipient as delivered.
func updateDelivered(msgID int64, addr string) error {
	db, err := sql.Open("postgres", "")
	if err != nil {
		return err
	}
	defer db.Close()

	now := timeutil.TimestampNow().Float64()
	_, err = db.Exec(`
		UPDATE msg_to SET time_delivered = $1, response_code = 200
		WHERE msg_id = $2 AND addr = $3
	`, now, msgID, addr)
	return err
}

// updateLastAttempt records a failed delivery attempt. If responseCode is non-nil
// it is stored; otherwise only time_last_attempt is updated.
func updateLastAttempt(msgID int64, addr string, responseCode *uint8) error {
	db, err := sql.Open("postgres", "")
	if err != nil {
		return err
	}
	defer db.Close()

	now := timeutil.TimestampNow().Float64()
	if responseCode != nil {
		_, err = db.Exec(`
			UPDATE msg_to SET time_last_attempt = $1, response_code = $2
			WHERE msg_id = $3 AND addr = $4
		`, now, int(*responseCode), msgID, addr)
	} else {
		_, err = db.Exec(`
			UPDATE msg_to SET time_last_attempt = $1
			WHERE msg_id = $2 AND addr = $3
		`, now, msgID, addr)
	}
	return err
}

// ToFMsgHeader converts a PendingMessage to an FMsgHeader for wire encoding.
// recipientAddrs should be the subset of recipients for a specific target domain.
func (pm *PendingMessage) ToFMsgHeader(recipientAddrs []string) (*FMsgHeader, error) {
	from, err := parseAddress([]byte(pm.FromAddr))
	if err != nil {
		return nil, fmt.Errorf("invalid from address %s: %w", pm.FromAddr, err)
	}

	to := make([]FMsgAddress, 0, len(recipientAddrs))
	for _, a := range recipientAddrs {
		addr, err := parseAddress([]byte(a))
		if err != nil {
			return nil, fmt.Errorf("invalid to address %s: %w", a, err)
		}
		to = append(to, *addr)
	}

	h := &FMsgHeader{
		Version:   pm.Version,
		Flags:     pm.Flags,
		Pid:       pm.Pid,
		From:      *from,
		To:        to,
		Timestamp: pm.Timestamp,
		Topic:     pm.Topic,
		Type:      pm.Type,
		Size:      uint32(pm.Size),
		Filepath:  pm.Filepath,
	}
	return h, nil
}

// groupRecipientsByDomain groups a PendingMessage's recipients by domain,
// returning a map from domain to the list of full addresses on that domain.
func groupRecipientsByDomain(pm *PendingMessage) map[string][]PendingRecipient {
	groups := make(map[string][]PendingRecipient)
	for _, r := range pm.Recipients {
		// addr is @user@domain — extract domain after last @
		lastAt := strings.LastIndex(r.Addr, "@")
		if lastAt == -1 {
			continue
		}
		domain := r.Addr[lastAt+1:]
		groups[domain] = append(groups[domain], r)
	}
	return groups
}
