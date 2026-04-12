package main

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"

	env "github.com/caitlinelfring/go-env-default"
	"github.com/levenlabs/golib/timeutil"
	"github.com/lib/pq"
)

var RetryInterval float64 = 20
var RetryMaxAge float64 = 86400
var PollInterval = 10
var MaxConcurrentSend = 1024

func loadSenderEnvConfig() {
	RetryInterval = env.GetFloatDefault("FMSG_RETRY_INTERVAL", 20)
	RetryMaxAge = env.GetFloatDefault("FMSG_RETRY_MAX_AGE", 86400)
	PollInterval = env.GetIntDefault("FMSG_POLL_INTERVAL", 10)
	MaxConcurrentSend = env.GetIntDefault("FMSG_MAX_CONCURRENT_SEND", 1024)
}

// pendingTarget identifies a (message, domain) pair that needs delivery.
type pendingTarget struct {
	MsgID  int64
	Domain string
}

// findPendingTargets discovers (msg_id, domain) pairs with undelivered,
// retryable recipients. This is a lightweight read-only query — row-level
// locks are acquired per-delivery in deliverMessage.
func findPendingTargets() ([]pendingTarget, error) {
	db, err := sql.Open("postgres", "")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	now := timeutil.TimestampNow().Float64()

	// query both msg_to and msg_add_to for pending targets
	rows, err := db.Query(`
		SELECT mt.msg_id, mt.addr
		FROM msg_to mt
		INNER JOIN msg m ON m.id = mt.msg_id
		WHERE mt.time_delivered IS NULL
		  AND m.time_sent IS NOT NULL
		  AND (mt.response_code IS NULL OR mt.response_code IN (3, 5, 101))
		  AND (mt.time_last_attempt IS NULL OR ($1 - mt.time_last_attempt) > LEAST($2 * POWER(2.0, GREATEST(mt.attempt_count - 1, 0)::float), $3))
		  AND ($1 - m.time_sent) < $3
		UNION ALL
		SELECT mat.msg_id, mat.addr
		FROM msg_add_to mat
		INNER JOIN msg m ON m.id = mat.msg_id
		WHERE mat.time_delivered IS NULL
		  AND m.time_sent IS NOT NULL
		  AND (mat.response_code IS NULL OR mat.response_code IN (3, 5, 101))
		  AND (mat.time_last_attempt IS NULL OR ($1 - mat.time_last_attempt) > LEAST($2 * POWER(2.0, GREATEST(mat.attempt_count - 1, 0)::float), $3))
		  AND ($1 - m.time_sent) < $3
	`, now, RetryInterval, RetryMaxAge)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type key struct {
		msgID  int64
		domain string
	}
	seen := make(map[key]bool)
	var targets []pendingTarget

	for rows.Next() {
		var msgID int64
		var addr string
		if err := rows.Scan(&msgID, &addr); err != nil {
			return nil, err
		}
		lastAt := strings.LastIndex(addr, "@")
		if lastAt == -1 {
			continue
		}
		domain := addr[lastAt+1:]
		if strings.EqualFold(domain, Domain) {
			continue // local domain — no remote delivery needed
		}
		k := key{msgID, domain}
		if !seen[k] {
			seen[k] = true
			targets = append(targets, pendingTarget{MsgID: msgID, Domain: domain})
		}
	}
	return targets, rows.Err()
}

// sendMsgData transmits the message body then all attachment payloads on conn.
func sendMsgData(conn net.Conn, h *FMsgHeader) error {
	fd, err := os.Open(h.Filepath)
	if err != nil {
		return fmt.Errorf("opening data file %s: %w", h.Filepath, err)
	}
	defer fd.Close()

	conn.SetWriteDeadline(time.Now().Add(calcNetIODuration(int(h.Size), MinUploadRate)))
	if _, err := io.CopyN(conn, fd, int64(h.Size)); err != nil {
		return fmt.Errorf("sending data: %w", err)
	}
	for _, att := range h.Attachments {
		af, err := os.Open(att.Filepath)
		if err != nil {
			return fmt.Errorf("opening attachment %s: %w", att.Filename, err)
		}
		_, copyErr := io.CopyN(conn, af, int64(att.Size))
		af.Close()
		if copyErr != nil {
			return fmt.Errorf("sending attachment %s: %w", att.Filename, copyErr)
		}
	}
	return nil
}

// updateRecipient records a delivery outcome for one address in table.
// Deliveries set time_delivered; failures set time_last_attempt and increment
// attempt_count to drive exponential back-off on subsequent retries.
func updateRecipient(tx *sql.Tx, table, addr string, msgID int64, now float64, code int, delivered bool) {
	var err error
	if delivered {
		_, err = tx.Exec(fmt.Sprintf(`
			UPDATE %s SET time_delivered = $1, response_code = $2
			WHERE msg_id = $3 AND addr = $4
		`, table), now, code, msgID, addr)
	} else {
		_, err = tx.Exec(fmt.Sprintf(`
			UPDATE %s SET time_last_attempt = $1, response_code = $2,
			              attempt_count = attempt_count + 1
			WHERE msg_id = $3 AND addr = $4
		`, table), now, code, msgID, addr)
	}
	if err != nil {
		log.Printf("ERROR: sender: update recipient %s: %s", addr, err)
	}
}

// updateAllLocked applies the same outcome to every locked to and add-to address.
func updateAllLocked(tx *sql.Tx, lockedAddrs, lockedAddToAddrs []string, msgID int64, now float64, code int, delivered bool) {
	for _, a := range lockedAddrs {
		updateRecipient(tx, "msg_to", a, msgID, now, code, delivered)
	}
	for _, a := range lockedAddToAddrs {
		updateRecipient(tx, "msg_add_to", a, msgID, now, code, delivered)
	}
}

// commitOrLog commits the transaction and marks it as committed.
func commitOrLog(tx *sql.Tx, committed *bool, msgID int64) {
	if err := tx.Commit(); err != nil {
		log.Printf("ERROR: sender: commit tx for msg %d: %s", msgID, err)
	} else {
		*committed = true
	}
}

// deliverMessage handles delivery of a single message to a single remote domain.
//
// It manages its own database transaction with the following lifecycle:
//   - Locks the pending msg_to rows for this (message, domain) via FOR UPDATE SKIP LOCKED.
//   - Loads the full message including ALL recipients (for the original wire header).
//   - Sends the complete original message to the remote host.
//   - On success: updates time_delivered + response_code, commits.
//   - On rejection (got response code): updates response_code + time_last_attempt, commits.
//   - On error (no response): logs error, rolls back — delivery retried in future.
func deliverMessage(target pendingTarget) {
	if strings.EqualFold(target.Domain, Domain) {
		// local domain — mark as delivered rather than sending remotely
		db, err := sql.Open("postgres", "")
		if err != nil {
			log.Printf("ERROR: sender: db open for local delivery: %s", err)
			return
		}
		defer db.Close()
		now := timeutil.TimestampNow().Float64()
		if _, err := db.Exec(`
			UPDATE msg_to SET time_delivered = $1, response_code = 200
			WHERE msg_id = $2 AND time_delivered IS NULL
			  AND lower(split_part(addr, '@', 3)) = lower($3)
		`, now, target.MsgID, target.Domain); err != nil {
			log.Printf("ERROR: sender: marking local recipients delivered for msg %d: %s", target.MsgID, err)
		}
		return
	}

	db, err := sql.Open("postgres", "")
	if err != nil {
		log.Printf("ERROR: sender: db open: %s", err)
		return
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		log.Printf("ERROR: sender: begin tx: %s", err)
		return
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback()
		}
	}()

	now := timeutil.TimestampNow().Float64()

	// Lock pending (undelivered, retryable) msg_to rows for this message
	// on the target domain. SKIP LOCKED avoids blocking concurrent senders.
	lockRows, err := tx.Query(`
		SELECT mt.addr
		FROM msg_to mt
		INNER JOIN msg m ON m.id = mt.msg_id
		WHERE mt.msg_id = $1
		  AND mt.time_delivered IS NULL
		  AND m.time_sent IS NOT NULL
		  AND (mt.response_code IS NULL OR mt.response_code IN (3, 5, 101))
		  AND (mt.time_last_attempt IS NULL OR ($2 - mt.time_last_attempt) > LEAST($3 * POWER(2.0, GREATEST(mt.attempt_count - 1, 0)::float), $4))
		  AND ($2 - m.time_sent) < $4
		FOR UPDATE OF mt SKIP LOCKED
	`, target.MsgID, now, RetryInterval, RetryMaxAge)
	if err != nil {
		log.Printf("ERROR: sender: lock rows for msg %d: %s", target.MsgID, err)
		return
	}

	var lockedAddrs []string
	for lockRows.Next() {
		var addr string
		if err := lockRows.Scan(&addr); err != nil {
			lockRows.Close()
			log.Printf("ERROR: sender: scan locked addr: %s", err)
			return
		}
		lastAt := strings.LastIndex(addr, "@")
		if lastAt != -1 && strings.EqualFold(addr[lastAt+1:], target.Domain) {
			lockedAddrs = append(lockedAddrs, addr)
		}
	}
	lockRows.Close()
	if err := lockRows.Err(); err != nil {
		log.Printf("ERROR: sender: lock rows err for msg %d: %s", target.MsgID, err)
		return
	}

	// Also lock pending msg_add_to rows for this message on the target domain.
	lockAddToRows, err := tx.Query(`
		SELECT mat.addr
		FROM msg_add_to mat
		INNER JOIN msg m ON m.id = mat.msg_id
		WHERE mat.msg_id = $1
		  AND mat.time_delivered IS NULL
		  AND m.time_sent IS NOT NULL
		  AND (mat.response_code IS NULL OR mat.response_code IN (3, 5, 101))
		  AND (mat.time_last_attempt IS NULL OR ($2 - mat.time_last_attempt) > LEAST($3 * POWER(2.0, GREATEST(mat.attempt_count - 1, 0)::float), $4))
		  AND ($2 - m.time_sent) < $4
		FOR UPDATE OF mat SKIP LOCKED
	`, target.MsgID, now, RetryInterval, RetryMaxAge)
	if err != nil {
		log.Printf("ERROR: sender: lock add-to rows for msg %d: %s", target.MsgID, err)
		return
	}

	var lockedAddToAddrs []string
	for lockAddToRows.Next() {
		var addr string
		if err := lockAddToRows.Scan(&addr); err != nil {
			lockAddToRows.Close()
			log.Printf("ERROR: sender: scan locked add-to addr: %s", err)
			return
		}
		lastAt := strings.LastIndex(addr, "@")
		if lastAt != -1 && strings.EqualFold(addr[lastAt+1:], target.Domain) {
			lockedAddToAddrs = append(lockedAddToAddrs, addr)
		}
	}
	lockAddToRows.Close()
	if err := lockAddToRows.Err(); err != nil {
		log.Printf("ERROR: sender: lock add-to rows err for msg %d: %s", target.MsgID, err)
		return
	}

	if len(lockedAddrs) == 0 && len(lockedAddToAddrs) == 0 {
		return // already locked by another sender or no longer eligible
	}

	// Load the full message from msg table
	h, err := loadMsg(tx, target.MsgID)
	if err != nil {
		log.Printf("ERROR: sender: %s", err)
		return
	}

	// Try zlib-deflate compression for message data and attachment data.
	// Compressed temp files are cleaned up after delivery completes.
	var deflateCleanup []string
	defer func() {
		for _, p := range deflateCleanup {
			_ = os.Remove(p)
		}
	}()
	if shouldCompress(h.Type, h.Size) {
		dp, cs, ok, derr := tryCompress(h.Filepath, h.Size)
		if derr != nil {
			log.Printf("WARN: sender: compress msg data for msg %d: %s", target.MsgID, derr)
		} else if ok {
			log.Printf("INFO: sender: compressed msg %d data: %d -> %d bytes", target.MsgID, h.Size, cs)
			deflateCleanup = append(deflateCleanup, dp)
			h.Filepath = dp
			h.Size = cs
			h.Flags |= FlagDeflate
		}
	}
	for i := range h.Attachments {
		att := &h.Attachments[i]
		if shouldCompress(att.Type, att.Size) {
			dp, cs, ok, derr := tryCompress(att.Filepath, att.Size)
			if derr != nil {
				log.Printf("WARN: sender: compress attachment %s for msg %d: %s", att.Filename, target.MsgID, derr)
			} else if ok {
				log.Printf("INFO: sender: compressed msg %d attachment %s: %d -> %d bytes", target.MsgID, att.Filename, att.Size, cs)
				deflateCleanup = append(deflateCleanup, dp)
				att.Filepath = dp
				att.Size = cs
				att.Flags |= 1 << 1
			}
		}
	}

	// Ensure sha256 is populated for outgoing messages so future pid lookups
	// (e.g. add-to notifications referencing this message) can find it.
	msgHash, err := h.GetMessageHash()
	if err != nil {
		log.Printf("ERROR: sender: computing message hash for msg %d: %s", target.MsgID, err)
		return
	}
	if _, err := tx.Exec(`UPDATE msg SET sha256 = $1 WHERE id = $2 AND sha256 IS NULL`,
		msgHash, target.MsgID); err != nil {
		log.Printf("ERROR: sender: storing sha256 for msg %d: %s", target.MsgID, err)
		return
	}

	// Compute header hash now; registerOutgoing with Host B's IP happens after
	// the connection is established (IP needed for challenge validation §10.5).
	hash := h.GetHeaderHash()
	hashArr := *(*[32]byte)(hash)

	// Build the list of recipients on the target domain in to then add-to order.
	// Per spec, per-recipient response codes follow the same order.
	lockedSet := make(map[string]bool)
	for _, a := range lockedAddrs {
		lockedSet[strings.ToLower(a)] = true
	}
	for _, a := range lockedAddToAddrs {
		lockedSet[strings.ToLower(a)] = true
	}
	type domainRecip struct {
		addr     string
		isLocked bool
		isAddTo  bool
	}
	var domainRecips []domainRecip
	for _, addr := range h.To {
		if strings.EqualFold(addr.Domain, target.Domain) {
			s := addr.ToString()
			domainRecips = append(domainRecips, domainRecip{
				addr:     s,
				isLocked: lockedSet[strings.ToLower(s)],
				isAddTo:  false,
			})
		}
	}
	for _, addr := range h.AddTo {
		if strings.EqualFold(addr.Domain, target.Domain) {
			s := addr.ToString()
			domainRecips = append(domainRecips, domainRecip{
				addr:     s,
				isLocked: lockedSet[strings.ToLower(s)],
				isAddTo:  true,
			})
		}
	}

	// --- network delivery ---

	targetIPs, err := lookupAuthorisedIPs(target.Domain)
	if err != nil {
		log.Printf("ERROR: sender: DNS lookup for _fmsg.%s failed: %s", target.Domain, err)
		return
	}

	var conn net.Conn
	for _, ip := range targetIPs {
		addr := net.JoinHostPort(ip.String(), fmt.Sprintf("%d", RemotePort))
		conn, err = net.DialTimeout("tcp", addr, 10*time.Second)
		if err == nil {
			break
		}
		log.Printf("WARN: sender: connect to %s failed: %s", addr, err)
	}
	if conn == nil {
		log.Printf("ERROR: sender: could not connect to any IP for _fmsg.%s", target.Domain)
		return
	}
	defer conn.Close()

	// Register in outgoing map with Host B's IP before sending the header so
	// any incoming challenge can be matched by hash AND IP (§10.2 step 2).
	connectedIP := conn.RemoteAddr().(*net.TCPAddr).IP.String()
	log.Printf("INFO: sender: registering outgoing message %s (%s)", hex.EncodeToString(hashArr[:]), connectedIP)
	registerOutgoing(hashArr, h, connectedIP)
	defer removeOutgoingIP(hashArr, connectedIP)

	// Step 3: Transmit message header.
	if _, err := conn.Write(h.Encode()); err != nil {
		log.Printf("ERROR: sender: writing header for msg %d: %s", target.MsgID, err)
		return
	}

	// Step 5: Read the initial response byte before sending any data (§10.2 step 5).
	// The challenge handler may fire on a separate goroutine during this wait.
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	initCode := make([]byte, 1)
	if _, err := io.ReadFull(conn, initCode); err != nil {
		log.Printf("ERROR: sender: reading initial response for msg %d: %s", target.MsgID, err)
		return
	}
	now = timeutil.TimestampNow().Float64()
	isAddToMsg := h.Flags&FlagHasAddTo != 0

	switch initCode[0] {
	case AcceptCodeContinue: // 64 — send data + attachments, then per-recipient codes
		if err := sendMsgData(conn, h); err != nil {
			log.Printf("ERROR: sender: %s (msg %d)", err, target.MsgID)
			return
		}
	case AcceptCodeSkipData: // 65 — add-to, parent stored, recipients on this host; skip data
		if !isAddToMsg {
			log.Printf("WARN: sender: msg %d received protocol-invalid code 65 from %s for non-add-to message, terminating",
				target.MsgID, target.Domain)
			return
		}
		// do not transmit data; per-recipient codes follow below
	case AcceptCodeAddTo: // 11 — add-to accepted, no recipients on this host
		if !isAddToMsg {
			log.Printf("WARN: sender: msg %d received protocol-invalid code 11 from %s for non-add-to message, terminating",
				target.MsgID, target.Domain)
			return
		}
		log.Printf("INFO: sender: msg %d add-to accepted by %s (code 11)", target.MsgID, target.Domain)
		updateAllLocked(tx, lockedAddrs, lockedAddToAddrs, target.MsgID, now, int(initCode[0]), true)
		commitOrLog(tx, &committed, target.MsgID)
		return
	default:
		if initCode[0] >= 1 && initCode[0] <= 10 {
			// global rejection
			log.Printf("WARN: sender: msg %d rejected by %s: %s (%d)",
				target.MsgID, target.Domain, responseCodeName(initCode[0]), initCode[0])
			updateAllLocked(tx, lockedAddrs, lockedAddToAddrs, target.MsgID, now, int(initCode[0]), false)
			commitOrLog(tx, &committed, target.MsgID)
		} else {
			// unexpected code — TERMINATE
			log.Printf("WARN: sender: msg %d unexpected response code %d from %s, terminating",
				target.MsgID, initCode[0], target.Domain)
		}
		return
	}

	// Step 6: Read one per-recipient code per recipient on this host, in
	// to-field order then add-to order (§10.2 step 6).
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	codes := make([]byte, len(domainRecips))
	if _, err := io.ReadFull(conn, codes); err != nil {
		log.Printf("ERROR: sender: reading per-recipient codes for msg %d: %s", target.MsgID, err)
		return
	}
	now = timeutil.TimestampNow().Float64()

	for i, dr := range domainRecips {
		if !dr.isLocked {
			continue
		}
		c := codes[i]
		table := "msg_to"
		if dr.isAddTo {
			table = "msg_add_to"
		}
		delivered := c == RejectCodeAccept
		if delivered {
			log.Printf("INFO: sender: delivered msg %d to %s", target.MsgID, dr.addr)
		} else {
			log.Printf("WARN: sender: msg %d to %s: %s (%d)", target.MsgID, dr.addr, responseCodeName(c), c)
		}
		updateRecipient(tx, table, dr.addr, target.MsgID, now, int(c), delivered)
	}

	commitOrLog(tx, &committed, target.MsgID)
}

// processPendingMessages finds messages needing delivery and dispatches a
// goroutine per (message, domain) pair, bounded by the semaphore.
func processPendingMessages(sem chan struct{}) {
	targets, err := findPendingTargets()
	if err != nil {
		log.Printf("ERROR: sender: finding pending targets: %s", err)
		return
	}
	if len(targets) == 0 {
		return
	}
	log.Printf("INFO: sender: found %d pending target(s)", len(targets))

	for _, t := range targets {
		sem <- struct{}{} // acquire
		go func(t pendingTarget) {
			defer func() { <-sem }()
			deliverMessage(t)
		}(t)
	}
}

// startSender runs the sender loop: polls the database periodically and also
// listens for PostgreSQL notifications for immediate pickup of new messages.
func startSender() {
	loadSenderEnvConfig()
	log.Printf("INFO: sender: started (poll=%ds, retry=%.0fs, max_concurrent=%d)",
		PollInterval, RetryInterval, MaxConcurrentSend)

	sem := make(chan struct{}, MaxConcurrentSend)

	// set up PostgreSQL LISTEN for immediate notification
	notifyCh := make(chan struct{}, 1)
	go func() {
		listener := pq.NewListener("", 10*time.Second, time.Minute, func(ev pq.ListenerEventType, err error) {
			if err != nil {
				log.Printf("ERROR: sender: pg listener: %s", err)
			}
		})
		if err := listener.Listen("new_msg_to"); err != nil {
			log.Printf("ERROR: sender: could not LISTEN on new_msg_to: %s", err)
			return
		}
		defer listener.Close()
		log.Println("INFO: sender: listening for new_msg_to notifications")
		for {
			select {
			case n := <-listener.Notify:
				if n != nil {
					log.Printf("INFO: sender: notification received: %s", n.Extra)
					select {
					case notifyCh <- struct{}{}:
					default:
					}
				}
			case <-time.After(32 * time.Second):
				// ping to keep connection alive
				if err := listener.Ping(); err != nil {
					log.Printf("ERROR: sender: pg listener ping: %s", err)
				}
			}
		}
	}()

	// initial poll on startup
	processPendingMessages(sem)

	ticker := time.NewTicker(time.Duration(PollInterval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			processPendingMessages(sem)
		case <-notifyCh:
			// small delay to batch rapid inserts
			time.Sleep(256 * time.Millisecond)
			processPendingMessages(sem)
		}
	}
}
