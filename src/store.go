package main

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/levenlabs/golib/timeutil"
	"github.com/lib/pq"
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
	// TODO check at least tables exist?
	return nil
}

func storeMsgDetail(msg *FMsgHeader) error {

	db, err := sql.Open("postgres", "")
	if err != nil {
		return err
	}
	defer db.Close()

	stmt, err := db.Prepare(`insert into msg (version	
	, flags
	, time_sent
	, time_recv
	, from_addr
	, to_addrs
	, topic
	, type
	, sha256
	, psha256
	, size
	, filepath) 
values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
returning id`)
	if err != nil {
		return err
	}

	count := len(msg.To)
	to_addrs := make([]string, count)
	for i := 0; i < count; i++ {
		to_addrs[i] = msg.To[i].ToString()
	}

	msgHash, err := msg.GetMessageHash()
	if err != nil {
		return err
	}

	_, err = stmt.Exec(msg.Version,
		msg.Flags,
		msg.Timestamp,
		timeutil.TimestampNow().Float64(),
		msg.From.ToString(),
		pq.Array(to_addrs),
		msg.Topic,
		msg.Type,
		msgHash,
		msg.Pid,
		int(msg.Size),
		msg.Filepath)
	if err != nil {
		return err
	}
	// TODO set pid if psha256
	// TODO attachments

	return nil

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

// updateLastAttempt records a failed delivery attempt with the response code.
func updateLastAttempt(msgID int64, addr string, responseCode uint8) error {
	db, err := sql.Open("postgres", "")
	if err != nil {
		return err
	}
	defer db.Close()

	now := timeutil.TimestampNow().Float64()
	_, err = db.Exec(`
		UPDATE msg_to SET time_last_attempt = $1, response_code = $2
		WHERE msg_id = $3 AND addr = $4
	`, now, int(responseCode), msgID, addr)
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
