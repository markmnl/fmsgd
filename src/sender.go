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

// pendingTarget identifies a (message, batch, domain) tuple that needs delivery.
// BatchID and BatchNo are both 0 for original message delivery. For add-to
// batch delivery they identify the specific msg_add_to_batch row.
type pendingTarget struct {
	MsgID   int64
	BatchID int64
	BatchNo int
	Domain  string
}

// findPendingTargets discovers (msg_id, domain) pairs with undelivered,
// retryable recipients. This is a lightweight read-only query — row-level
// locks are acquired per-delivery in deliverMessage.
//
// TODO [Spec]: Spec says to retry "with back-off" (e.g. exponential back-off).
// Currently uses a fixed RetryInterval. Implement an exponential back-off
// strategy — e.g. double the wait after each failed attempt per (msg, domain).
//
// TODO [Spec]: Per-user code 101 (user full = "insufficient resources for
// specific recipient") is analogous to global code 5 and is likely transient.
// Consider adding 101 to the retryable set alongside codes 3 and 5.
func findPendingTargets() ([]pendingTarget, error) {
	db, err := sql.Open("postgres", "")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	now := timeutil.TimestampNow().Float64()

	// Original message recipients (batch_id=0, batch_no=0).
	rows, err := db.Query(`
		SELECT mt.msg_id, mt.addr
		FROM msg_to mt
		INNER JOIN msg m ON m.id = mt.msg_id
		WHERE mt.time_delivered IS NULL
		  AND m.time_sent IS NOT NULL
		  AND (mt.response_code IS NULL OR mt.response_code IN (3, 5))
		  AND (mt.time_last_attempt IS NULL OR ($1 - mt.time_last_attempt) > $2)
		  AND ($1 - m.time_sent) < $3
	`, now, RetryInterval, RetryMaxAge)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type key struct {
		msgID   int64
		batchNo int
		domain  string
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
			continue
		}
		k := key{msgID, 0, domain}
		if !seen[k] {
			seen[k] = true
			targets = append(targets, pendingTarget{MsgID: msgID, Domain: domain})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Add-to batch recipients. Join through msg_add_to_batch.
	// m.sha256 IS NOT NULL ensures original was delivered (hash populated)
	// before we attempt add-to delivery.
	addToRows, err := db.Query(`
		SELECT b.msg_id, b.id, b.batch_no, mat.addr
		FROM msg_add_to mat
		INNER JOIN msg_add_to_batch b ON b.id = mat.batch_id
		INNER JOIN msg m ON m.id = b.msg_id
		WHERE mat.time_delivered IS NULL
		  AND m.time_sent IS NOT NULL
		  AND m.sha256 IS NOT NULL
		  AND (mat.response_code IS NULL OR mat.response_code IN (3, 5))
		  AND (mat.time_last_attempt IS NULL OR ($1 - mat.time_last_attempt) > $2)
		  AND ($1 - m.time_sent) < $3
	`, now, RetryInterval, RetryMaxAge)
	if err != nil {
		return nil, err
	}
	defer addToRows.Close()

	for addToRows.Next() {
		var msgID, batchID int64
		var batchNo int
		var addr string
		if err := addToRows.Scan(&msgID, &batchID, &batchNo, &addr); err != nil {
			return nil, err
		}
		lastAt := strings.LastIndex(addr, "@")
		if lastAt == -1 {
			continue
		}
		domain := addr[lastAt+1:]
		if strings.EqualFold(domain, Domain) {
			continue
		}
		k := key{msgID, batchNo, domain}
		if !seen[k] {
			seen[k] = true
			targets = append(targets, pendingTarget{MsgID: msgID, BatchID: batchID, BatchNo: batchNo, Domain: domain})
		}
	}
	return targets, addToRows.Err()
}

// deliverMessage handles delivery of a single message to a single remote domain.
//
// For original messages (target.BatchNo == 0): locks pending msg_to rows,
// loads the original message, sends the complete message, handles per-recipient
// response codes for to recipients on the target domain.
//
// For add-to batches (target.BatchNo > 0): locks pending msg_add_to rows for
// the specific batch, loads the message with that batch's add-to recipients
// (pid = original hash), sends the complete message, handles per-recipient
// response codes for both to and add-to recipients on the target domain.
func deliverMessage(target pendingTarget) {
	if strings.EqualFold(target.Domain, Domain) {
		deliverLocal(target)
		return
	}

	if target.BatchNo == 0 {
		deliverOriginal(target)
	} else {
		deliverAddToBatch(target)
	}
}

// deliverLocal marks local-domain recipients as delivered without network I/O.
func deliverLocal(target pendingTarget) {
	db, err := sql.Open("postgres", "")
	if err != nil {
		log.Printf("ERROR: sender: db open for local delivery: %s", err)
		return
	}
	defer db.Close()
	now := timeutil.TimestampNow().Float64()

	if target.BatchNo == 0 {
		if _, err := db.Exec(`
			UPDATE msg_to SET time_delivered = $1, response_code = 200
			WHERE msg_id = $2 AND time_delivered IS NULL
			  AND lower(split_part(addr, '@', 3)) = lower($3)
		`, now, target.MsgID, target.Domain); err != nil {
			log.Printf("ERROR: sender: marking local recipients delivered for msg %d: %s", target.MsgID, err)
		}
	} else {
		if _, err := db.Exec(`
			UPDATE msg_add_to SET time_delivered = $1, response_code = 200
			WHERE batch_id = $2 AND time_delivered IS NULL
			  AND lower(split_part(addr, '@', 3)) = lower($3)
		`, now, target.BatchID, target.Domain); err != nil {
			log.Printf("ERROR: sender: marking local add-to recipients delivered for msg %d batch %d: %s",
				target.MsgID, target.BatchNo, err)
		}
	}
}

// lockedRow holds a locked recipient row's database ID and address.
type lockedRow struct {
	ID   int64
	Addr string
}

// deliverOriginal handles delivery of the original message (no add-to) to a
// remote domain. Locks msg_to rows, sends the full message, processes response.
func deliverOriginal(target pendingTarget) {
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

	// Lock pending msg_to rows for this message on the target domain.
	lockRows, err := tx.Query(`
		SELECT mt.id, mt.addr
		FROM msg_to mt
		INNER JOIN msg m ON m.id = mt.msg_id
		WHERE mt.msg_id = $1
		  AND mt.time_delivered IS NULL
		  AND m.time_sent IS NOT NULL
		  AND (mt.response_code IS NULL OR mt.response_code IN (3, 5))
		  AND (mt.time_last_attempt IS NULL OR ($2 - mt.time_last_attempt) > $3)
		  AND ($2 - m.time_sent) < $4
		FOR UPDATE OF mt SKIP LOCKED
	`, target.MsgID, now, RetryInterval, RetryMaxAge)
	if err != nil {
		log.Printf("ERROR: sender: lock rows for msg %d: %s", target.MsgID, err)
		return
	}

	var locked []lockedRow
	for lockRows.Next() {
		var r lockedRow
		if err := lockRows.Scan(&r.ID, &r.Addr); err != nil {
			lockRows.Close()
			log.Printf("ERROR: sender: scan locked row: %s", err)
			return
		}
		lastAt := strings.LastIndex(r.Addr, "@")
		if lastAt != -1 && strings.EqualFold(r.Addr[lastAt+1:], target.Domain) {
			locked = append(locked, r)
		}
	}
	lockRows.Close()
	if err := lockRows.Err(); err != nil {
		log.Printf("ERROR: sender: lock rows err for msg %d: %s", target.MsgID, err)
		return
	}
	if len(locked) == 0 {
		return
	}

	// Load the original message (batchNo=0, no add-to).
	h, err := loadMsg(tx, target.MsgID, 0)
	if err != nil {
		log.Printf("ERROR: sender: %s", err)
		return
	}

	// Ensure sha256 is populated so future add-to batches can reference it.
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

	// Register in outgoing map for challenge handler.
	hash := h.GetHeaderHash()
	hashArr := *(*[32]byte)(hash)
	log.Printf("INFO: sender: registering outgoing message %s", hex.EncodeToString(hash[:]))
	registerOutgoing(hashArr, h)
	defer deleteOutgoing(hashArr)

	// Build the list of to-recipients on target domain, noting which we locked.
	lockedSet := make(map[string]int64) // lowercase addr → row ID
	for _, r := range locked {
		lockedSet[strings.ToLower(r.Addr)] = r.ID
	}
	type domainRecip struct {
		rowID    int64
		addr     string
		isLocked bool
	}
	var domainRecips []domainRecip
	for _, addr := range h.To {
		if strings.EqualFold(addr.Domain, target.Domain) {
			s := addr.ToString()
			rid, ok := lockedSet[strings.ToLower(s)]
			domainRecips = append(domainRecips, domainRecip{
				rowID:    rid,
				addr:     s,
				isLocked: ok,
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

	// Send header + message data + attachments.
	if err := sendMessageData(conn, h); err != nil {
		log.Printf("ERROR: sender: msg %d: %s", target.MsgID, err)
		return
	}

	// --- read response ---
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	firstByte := make([]byte, 1)
	if _, err := io.ReadFull(conn, firstByte); err != nil {
		log.Printf("ERROR: sender: reading response: %s", err)
		return
	}

	code := firstByte[0]
	now = timeutil.TimestampNow().Float64()

	if code < 100 {
		// Global code — update all locked rows.
		if code == AcceptCodeAddTo {
			log.Printf("INFO: sender: msg %d accepted (code 11) by %s", target.MsgID, target.Domain)
		} else {
			log.Printf("WARN: sender: msg %d rejected by %s: %s (%d)",
				target.MsgID, target.Domain, responseCodeName(code), code)
		}
		for _, r := range locked {
			if code == AcceptCodeAddTo || code == RejectCodeAccept {
				if _, err := tx.Exec(`UPDATE msg_to SET time_delivered = $1, response_code = $2 WHERE id = $3`,
					now, int(code), r.ID); err != nil {
					log.Printf("ERROR: sender: update delivered for %s: %s", r.Addr, err)
				}
			} else {
				if _, err := tx.Exec(`UPDATE msg_to SET time_last_attempt = $1, response_code = $2 WHERE id = $3`,
					now, int(code), r.ID); err != nil {
					log.Printf("ERROR: sender: update last attempt for %s: %s", r.Addr, err)
				}
			}
		}
		if err := tx.Commit(); err != nil {
			log.Printf("ERROR: sender: commit tx for msg %d: %s", target.MsgID, err)
		} else {
			committed = true
		}
		return
	}

	// Per-recipient codes.
	codes := make([]byte, len(domainRecips))
	codes[0] = code
	if len(domainRecips) > 1 {
		rest := make([]byte, len(domainRecips)-1)
		if _, err := io.ReadFull(conn, rest); err != nil {
			log.Printf("ERROR: sender: reading remaining response codes: %s", err)
			return
		}
		copy(codes[1:], rest)
	}

	for i, dr := range domainRecips {
		if !dr.isLocked {
			continue
		}
		c := codes[i]
		if c == RejectCodeAccept {
			log.Printf("INFO: sender: delivered msg %d to %s", target.MsgID, dr.addr)
			if _, err := tx.Exec(`UPDATE msg_to SET time_delivered = $1, response_code = 200 WHERE id = $2`,
				now, dr.rowID); err != nil {
				log.Printf("ERROR: sender: update delivered for %s: %s", dr.addr, err)
			}
		} else {
			log.Printf("WARN: sender: msg %d to %s rejected: %s (%d)",
				target.MsgID, dr.addr, responseCodeName(c), c)
			if _, err := tx.Exec(`UPDATE msg_to SET time_last_attempt = $1, response_code = $2 WHERE id = $3`,
				now, int(c), dr.rowID); err != nil {
				log.Printf("ERROR: sender: update last attempt for %s: %s", dr.addr, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		log.Printf("ERROR: sender: commit tx for msg %d: %s", target.MsgID, err)
	} else {
		committed = true
	}
}

// deliverAddToBatch handles delivery of an add-to batch message to a remote
// domain. Locks pending msg_add_to rows for the batch, loads the message with
// that batch's recipients, sends the complete message, and processes response.
func deliverAddToBatch(target pendingTarget) {
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

	// Lock pending msg_add_to rows for this batch on the target domain.
	lockRows, err := tx.Query(`
		SELECT mat.id, mat.addr
		FROM msg_add_to mat
		INNER JOIN msg_add_to_batch b ON b.id = mat.batch_id
		INNER JOIN msg m ON m.id = b.msg_id
		WHERE mat.batch_id = $1
		  AND mat.time_delivered IS NULL
		  AND m.time_sent IS NOT NULL
		  AND m.sha256 IS NOT NULL
		  AND (mat.response_code IS NULL OR mat.response_code IN (3, 5))
		  AND (mat.time_last_attempt IS NULL OR ($2 - mat.time_last_attempt) > $3)
		  AND ($2 - m.time_sent) < $4
		FOR UPDATE OF mat SKIP LOCKED
	`, target.BatchID, now, RetryInterval, RetryMaxAge)
	if err != nil {
		log.Printf("ERROR: sender: lock add-to rows for msg %d batch %d: %s", target.MsgID, target.BatchNo, err)
		return
	}

	var lockedAddTo []lockedRow
	for lockRows.Next() {
		var r lockedRow
		if err := lockRows.Scan(&r.ID, &r.Addr); err != nil {
			lockRows.Close()
			log.Printf("ERROR: sender: scan locked add-to row: %s", err)
			return
		}
		lastAt := strings.LastIndex(r.Addr, "@")
		if lastAt != -1 && strings.EqualFold(r.Addr[lastAt+1:], target.Domain) {
			lockedAddTo = append(lockedAddTo, r)
		}
	}
	lockRows.Close()
	if err := lockRows.Err(); err != nil {
		log.Printf("ERROR: sender: lock add-to rows err for msg %d batch %d: %s", target.MsgID, target.BatchNo, err)
		return
	}
	if len(lockedAddTo) == 0 {
		return
	}

	// Load the message with this batch's add-to recipients.
	// pid will be the original message's hash, add-to will be this batch only.
	h, err := loadMsg(tx, target.MsgID, target.BatchNo)
	if err != nil {
		log.Printf("ERROR: sender: %s", err)
		return
	}

	// Register in outgoing map for challenge handler.
	hash := h.GetHeaderHash()
	hashArr := *(*[32]byte)(hash)
	log.Printf("INFO: sender: registering outgoing add-to batch %d for msg %d: %s",
		target.BatchNo, target.MsgID, hex.EncodeToString(hash[:]))
	registerOutgoing(hashArr, h)
	defer deleteOutgoing(hashArr)

	// Build domain recipients: to + add-to on target domain.
	// Per spec, response codes arrive per-recipient in to then add-to order
	// excluding other domains.
	lockedSet := make(map[string]int64) // lowercase addr → row ID
	for _, r := range lockedAddTo {
		lockedSet[strings.ToLower(r.Addr)] = r.ID
	}
	type domainRecip struct {
		rowID    int64
		addr     string
		isLocked bool
		isAddTo  bool
	}
	var domainRecips []domainRecip
	for _, addr := range h.To {
		if strings.EqualFold(addr.Domain, target.Domain) {
			domainRecips = append(domainRecips, domainRecip{
				addr:    addr.ToString(),
				isAddTo: false,
			})
		}
	}
	for _, addr := range h.AddTo {
		if strings.EqualFold(addr.Domain, target.Domain) {
			s := addr.ToString()
			rid, ok := lockedSet[strings.ToLower(s)]
			domainRecips = append(domainRecips, domainRecip{
				rowID:    rid,
				addr:     s,
				isLocked: ok,
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

	// Send header + message data + attachments. The receiver may already
	// have the body and respond with code 11 before we finish writing —
	// TCP is full duplex, so a write error is non-fatal if we got a response.
	sendErr := sendMessageData(conn, h)

	// --- read response ---
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	firstByte := make([]byte, 1)
	if _, err := io.ReadFull(conn, firstByte); err != nil {
		if sendErr != nil {
			log.Printf("ERROR: sender: msg %d batch %d: send: %s; read response: %s",
				target.MsgID, target.BatchNo, sendErr, err)
		} else {
			log.Printf("ERROR: sender: msg %d batch %d: reading response: %s",
				target.MsgID, target.BatchNo, err)
		}
		return
	}
	if sendErr != nil {
		log.Printf("INFO: sender: msg %d batch %d: receiver responded before send completed",
			target.MsgID, target.BatchNo)
	}

	code := firstByte[0]
	now = timeutil.TimestampNow().Float64()

	if code < 100 {
		if code == AcceptCodeAddTo {
			log.Printf("INFO: sender: msg %d batch %d add-to accepted by %s",
				target.MsgID, target.BatchNo, target.Domain)
			for _, r := range lockedAddTo {
				if _, err := tx.Exec(`UPDATE msg_add_to SET time_delivered = $1, response_code = $2 WHERE id = $3`,
					now, int(code), r.ID); err != nil {
					log.Printf("ERROR: sender: update delivered add-to for %s: %s", r.Addr, err)
				}
			}
		} else {
			log.Printf("WARN: sender: msg %d batch %d rejected by %s: %s (%d)",
				target.MsgID, target.BatchNo, target.Domain, responseCodeName(code), code)
			for _, r := range lockedAddTo {
				if _, err := tx.Exec(`UPDATE msg_add_to SET time_last_attempt = $1, response_code = $2 WHERE id = $3`,
					now, int(code), r.ID); err != nil {
					log.Printf("ERROR: sender: update last attempt add-to for %s: %s", r.Addr, err)
				}
			}
		}
		if err := tx.Commit(); err != nil {
			log.Printf("ERROR: sender: commit tx for msg %d batch %d: %s", target.MsgID, target.BatchNo, err)
		} else {
			committed = true
		}
		return
	}

	// Per-recipient codes (to order then add-to order, this domain only).
	codes := make([]byte, len(domainRecips))
	codes[0] = code
	if len(domainRecips) > 1 {
		rest := make([]byte, len(domainRecips)-1)
		if _, err := io.ReadFull(conn, rest); err != nil {
			log.Printf("ERROR: sender: reading remaining response codes: %s", err)
			return
		}
		copy(codes[1:], rest)
	}

	for i, dr := range domainRecips {
		c := codes[i]
		if !dr.isAddTo {
			// to-recipient codes during add-to delivery — log but don't update msg_to.
			if c != RejectCodeAccept {
				log.Printf("WARN: sender: msg %d batch %d to-recip %s code: %s (%d)",
					target.MsgID, target.BatchNo, dr.addr, responseCodeName(c), c)
			}
			continue
		}
		if !dr.isLocked {
			continue
		}
		if c == RejectCodeAccept {
			log.Printf("INFO: sender: delivered msg %d batch %d to %s",
				target.MsgID, target.BatchNo, dr.addr)
			if _, err := tx.Exec(`UPDATE msg_add_to SET time_delivered = $1, response_code = 200 WHERE id = $2`,
				now, dr.rowID); err != nil {
				log.Printf("ERROR: sender: update delivered add-to for %s: %s", dr.addr, err)
			}
		} else {
			log.Printf("WARN: sender: msg %d batch %d to %s rejected: %s (%d)",
				target.MsgID, target.BatchNo, dr.addr, responseCodeName(c), c)
			if _, err := tx.Exec(`UPDATE msg_add_to SET time_last_attempt = $1, response_code = $2 WHERE id = $3`,
				now, int(c), dr.rowID); err != nil {
				log.Printf("ERROR: sender: update last attempt add-to for %s: %s", dr.addr, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		log.Printf("ERROR: sender: commit tx for msg %d batch %d: %s", target.MsgID, target.BatchNo, err)
	} else {
		committed = true
	}
}

// sendMessageData writes the encoded header, message body, and attachment data
// to conn. Returns an error on any I/O failure.
func sendMessageData(conn net.Conn, h *FMsgHeader) error {
	headerBytes := h.Encode()
	if _, err := conn.Write(headerBytes); err != nil {
		return fmt.Errorf("writing header: %w", err)
	}

	fd, err := os.Open(h.Filepath)
	if err != nil {
		return fmt.Errorf("opening file %s: %w", h.Filepath, err)
	}
	defer fd.Close()

	conn.SetWriteDeadline(time.Now().Add(calcNetIODuration(int(h.Size), MinUploadRate)))
	n, err := io.CopyN(conn, fd, int64(h.Size))
	if n != int64(h.Size) {
		return fmt.Errorf("file size mismatch: expected %d, got %d", h.Size, n)
	}
	if err != nil {
		return fmt.Errorf("sending data (%d/%d bytes): %w", n, h.Size, err)
	}

	for _, att := range h.Attachments {
		af, err := os.Open(att.Filepath)
		if err != nil {
			return fmt.Errorf("opening attachment %s: %w", att.Filename, err)
		}
		an, err := io.CopyN(conn, af, int64(att.Size))
		af.Close()
		if an != int64(att.Size) {
			return fmt.Errorf("attachment %s size mismatch: expected %d, got %d", att.Filename, att.Size, an)
		}
		if err != nil {
			return fmt.Errorf("sending attachment %s: %w", att.Filename, err)
		}
	}
	return nil
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
