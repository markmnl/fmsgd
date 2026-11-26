package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	env "github.com/caitlinelfring/go-env-default"
	"github.com/levenlabs/golib/timeutil"
	"github.com/lib/pq"
	"github.com/markmnl/fmsgd/pkg/fmsg"
)

var RetryInterval float64 = 20
var RetryMaxAge float64 = 86400
var PollInterval = 10
var MaxConcurrentSend = 1024

// Local response codes are stored only in the database; they are not fmsg
// protocol response codes (negative so they can never collide with one).
const (
	// localResponseCodeNoResponse marks a delivery attempt that got no
	// response; the row stays retryable.
	localResponseCodeNoResponse = -1
	// localResponseCodeNotOurDelivery marks a recipient recorded from a
	// received exchange purely for participant checks and faithful message
	// reconstruction (SPEC §10.3/§11) — delivering to them is another host's
	// job, so the row is never retried and never treated as pending.
	localResponseCodeNotOurDelivery = -2
)

var retryableResponseCodes = []int16{
	int16(localResponseCodeNoResponse),
	int16(RejectCodeUndisclosed),
	int16(RejectCodeInsufficentResources),
	int16(RejectCodeUserFull),
}

func loadSenderEnvConfig() {
	RetryInterval = env.GetFloatDefault("FMSG_RETRY_INTERVAL", 20)
	RetryMaxAge = env.GetFloatDefault("FMSG_RETRY_MAX_AGE", 86400)
	PollInterval = env.GetIntDefault("FMSG_POLL_INTERVAL", 10)
	MaxConcurrentSend = env.GetIntDefault("FMSG_MAX_CONCURRENT_SEND", 1024)
	SenderDBMaxConns = max(2, env.GetIntDefault("FMSG_SENDER_DB_MAX_CONNS", 32))
}

// deliverySlots is how many deliveries may run at once. A delivery holds one
// connection from the sender's database pool for its whole exchange, so the
// pool bounds it as well as FMSG_MAX_CONCURRENT_SEND; one connection is kept
// back for the query that finds pending work.
func deliverySlots(maxConcurrentSend, senderDBMaxConns int) int {
	return max(1, min(maxConcurrentSend, senderDBMaxConns-1))
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
	db, err := getSenderDB()
	if err != nil {
		return nil, err
	}

	now := timeutil.TimestampNow().Float64()

	// query both msg_to and msg_add_to for pending targets; add-to rows age
	// against their batch, not the message — recipients may be added long
	// after the message was sent
	rows, err := db.Query(`
		SELECT mt.msg_id, mt.addr
		FROM msg_to mt
		INNER JOIN msg m ON m.id = mt.msg_id
		WHERE mt.time_delivered IS NULL
		  AND m.time_sent IS NOT NULL
		  AND (mt.response_code IS NULL OR mt.response_code = ANY($4))
		  AND (mt.time_last_attempt IS NULL OR ($1 - mt.time_last_attempt) > LEAST($2 * POWER(2.0, GREATEST(mt.attempt_count - 1, 0)::float), $3))
		  AND ($1 - m.time_sent) < $3
		UNION ALL
		SELECT mat.msg_id, mat.addr
		FROM msg_add_to mat
		INNER JOIN msg m ON m.id = mat.msg_id
		INNER JOIN msg_add_to_batch b ON b.id = mat.batch_id
		WHERE mat.time_delivered IS NULL
		  AND m.time_sent IS NOT NULL
		  AND (mat.response_code IS NULL OR mat.response_code = ANY($4))
		  AND (mat.time_last_attempt IS NULL OR ($1 - mat.time_last_attempt) > LEAST($2 * POWER(2.0, GREATEST(mat.attempt_count - 1, 0)::float), $3))
		  AND ($1 - b.time_added) < $3
	`, now, RetryInterval, RetryMaxAge, pq.Array(retryableResponseCodes))
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
	addTarget := func(msgID int64, domain string) {
		if strings.EqualFold(domain, Domain) {
			return // local domain — no remote delivery needed
		}
		k := key{msgID, strings.ToLower(domain)}
		if !seen[k] {
			seen[k] = true
			targets = append(targets, pendingTarget{MsgID: msgID, Domain: domain})
		}
	}

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
		addTarget(msgID, addr[lastAt+1:])
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Participant domains pending add-to notification (SPEC §10.2): domains
	// hosting no recipient of a batch still receive the batch's add-to message
	// so all participants learn recipients were added.
	nrows, err := db.Query(`
		SELECT b.msg_id, n.domain
		FROM msg_add_to_notify n
		INNER JOIN msg_add_to_batch b ON b.id = n.batch_id
		INNER JOIN msg m ON m.id = b.msg_id
		WHERE n.time_notified IS NULL
		  AND m.time_sent IS NOT NULL
		  AND (n.response_code IS NULL OR n.response_code = ANY($4))
		  AND (n.time_last_attempt IS NULL OR ($1 - n.time_last_attempt) > LEAST($2 * POWER(2.0, GREATEST(n.attempt_count - 1, 0)::float), $3))
		  AND ($1 - b.time_added) < $3
	`, now, RetryInterval, RetryMaxAge, pq.Array(retryableResponseCodes))
	if err != nil {
		return nil, err
	}
	defer nrows.Close()
	for nrows.Next() {
		var msgID int64
		var domain string
		if err := nrows.Scan(&msgID, &domain); err != nil {
			return nil, err
		}
		addTarget(msgID, domain)
	}
	return targets, nrows.Err()
}

// sendMsgData transmits the message body then all attachment payloads on conn.
func sendMsgData(conn net.Conn, h *FMsgHeader) error {
	fd, err := os.Open(h.Filepath)
	if err != nil {
		return fmt.Errorf("opening data file %s: %w", h.Filepath, err)
	}
	defer fd.Close()

	conn.SetWriteDeadline(time.Now().Add(calcNetIODuration(totalWirePayloadSize(h), MinUploadRate)))
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
// For msg_add_to, rows are scoped by batch_id so independent batches sharing
// the same recipient address do not overwrite each other.
func updateRecipient(tx *sql.Tx, table, addr string, msgID, batchID int64, now float64, code int, delivered bool) error {
	targetTable, scopeCol, scopeID := "msg_to", "msg_id", msgID
	if table == "msg_add_to" {
		targetTable, scopeCol, scopeID = "msg_add_to", "batch_id", batchID
	}

	var err error
	if delivered {
		_, err = tx.Exec(fmt.Sprintf(`
			UPDATE %s SET time_delivered = $1, response_code = $2
			WHERE %s = $3 AND addr = $4
		`, targetTable, scopeCol), now, code, scopeID, addr)
	} else {
		_, err = tx.Exec(fmt.Sprintf(`
			UPDATE %s SET time_last_attempt = $1, response_code = $2,
			              attempt_count = attempt_count + 1
			WHERE %s = $3 AND addr = $4
		`, targetTable, scopeCol), now, code, scopeID, addr)
	}
	if err != nil {
		log.Printf("ERROR: sender: update recipient %s: %s", addr, err)
	}
	return err
}

// updateLocked applies the same outcome to every locked address in one table.
func updateLocked(tx *sql.Tx, table string, addrs []string, msgID, batchID int64, now float64, code int, delivered bool) error {
	for _, a := range addrs {
		if err := updateRecipient(tx, table, a, msgID, batchID, now, code, delivered); err != nil {
			return err
		}
	}
	return nil
}

// commitOrLog commits the transaction and marks it as committed.
func commitOrLog(tx *sql.Tx, committed *bool, msgID int64) {
	if err := tx.Commit(); err != nil {
		log.Printf("ERROR: sender: commit tx for msg %d: %s", msgID, err)
	} else {
		*committed = true
	}
}

// recordRetryableFailure marks one delivery unit's locked recipients and
// locked notify row (if any) for retry.
func recordRetryableFailure(tx *sql.Tx, committed *bool, table string, locked []string, msgID, batchID, notifyID int64) {
	now := timeutil.TimestampNow().Float64()
	if err := updateLocked(tx, table, locked, msgID, batchID, now, localResponseCodeNoResponse, false); err != nil {
		return
	}
	if err := updateNotify(tx, notifyID, now, localResponseCodeNoResponse, false); err != nil {
		return
	}
	commitOrLog(tx, committed, msgID)
}

// lockPendingNotify locks (FOR UPDATE SKIP LOCKED) the pending
// msg_add_to_notify row for one batch and domain, returning its id — 0 when
// none is pending or another sender holds it. A notify row is a participant
// domain owed the batch's add-to message even though it hosts none of the
// batch's recipients (SPEC §10.2).
func lockPendingNotify(tx *sql.Tx, batchID int64, domain string, now float64) (int64, error) {
	if batchID == 0 {
		return 0, nil
	}
	var id int64
	err := tx.QueryRow(`
		SELECT n.id
		FROM msg_add_to_notify n
		INNER JOIN msg_add_to_batch b ON b.id = n.batch_id
		INNER JOIN msg m ON m.id = b.msg_id
		WHERE n.batch_id = $1
		  AND lower(n.domain) = lower($2)
		  AND n.time_notified IS NULL
		  AND m.time_sent IS NOT NULL
		  AND (n.response_code IS NULL OR n.response_code = ANY($6))
		  AND (n.time_last_attempt IS NULL OR ($3 - n.time_last_attempt) > LEAST($4 * POWER(2.0, GREATEST(n.attempt_count - 1, 0)::float), $5))
		  AND ($3 - b.time_added) < $5
		FOR UPDATE OF n SKIP LOCKED
	`, batchID, domain, now, RetryInterval, RetryMaxAge, pq.Array(retryableResponseCodes)).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

// updateNotify records a notification outcome for one msg_add_to_notify row.
// notifyID 0 is a no-op so callers need not branch on whether the unit
// carried a notify row.
func updateNotify(tx *sql.Tx, notifyID int64, now float64, code int, notified bool) error {
	if notifyID == 0 {
		return nil
	}
	var err error
	if notified {
		_, err = tx.Exec(`
			UPDATE msg_add_to_notify SET time_notified = $1, response_code = $2
			WHERE id = $3
		`, now, code, notifyID)
	} else {
		_, err = tx.Exec(`
			UPDATE msg_add_to_notify SET time_last_attempt = $1, response_code = $2,
			                             attempt_count = attempt_count + 1
			WHERE id = $3
		`, now, code, notifyID)
	}
	if err != nil {
		log.Printf("ERROR: sender: update notify row %d: %s", notifyID, err)
	}
	return err
}

// lockPendingRecipients locks (FOR UPDATE SKIP LOCKED) the undelivered,
// retryable rows in `table` for one message on `domain`, returning the locked
// addresses. For msg_add_to it locks only rows in batchID, so each add-to batch
// is delivered as an independent unit.
func lockPendingRecipients(tx *sql.Tx, table string, msgID int64, domain string, batchID int64, now float64) ([]string, error) {
	// msg_add_to rows age against their batch, not the message — recipients
	// may be added long after the message was sent.
	ageRef := "m.time_sent"
	joinBatch := ""
	if table == "msg_add_to" {
		ageRef = "b.time_added"
		joinBatch = "INNER JOIN msg_add_to_batch b ON b.id = r.batch_id"
	}
	q := fmt.Sprintf(`
		SELECT r.addr
		FROM %s r
		INNER JOIN msg m ON m.id = r.msg_id
		%s
		WHERE r.msg_id = $1
		  AND r.time_delivered IS NULL
		  AND m.time_sent IS NOT NULL
		  AND (r.response_code IS NULL OR r.response_code = ANY($5))
		  AND (r.time_last_attempt IS NULL OR ($2 - r.time_last_attempt) > LEAST($3 * POWER(2.0, GREATEST(r.attempt_count - 1, 0)::float), $4))
		  AND ($2 - %s) < $4`, table, joinBatch, ageRef)
	args := []any{msgID, now, RetryInterval, RetryMaxAge, pq.Array(retryableResponseCodes)}
	if table == "msg_add_to" {
		q += " AND r.batch_id = $6"
		args = append(args, batchID)
	}
	q += " FOR UPDATE OF r SKIP LOCKED"

	rows, err := tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var locked []string
	for rows.Next() {
		var addr string
		if err := rows.Scan(&addr); err != nil {
			return nil, err
		}
		lastAt := strings.LastIndex(addr, "@")
		if lastAt != -1 && strings.EqualFold(addr[lastAt+1:], domain) {
			locked = append(locked, addr)
		}
	}
	return locked, rows.Err()
}

// deliverMessage delivers a message to a single remote domain. A stored message
// is several wire messages: the original message (its msg_to recipients) plus
// one add-to message per batch — each batch was added by a single sender at a
// point in time and MUST be delivered under that sender (SPEC §12). Each unit is
// sent over its own connection in its own transaction by deliverUnit.
func deliverMessage(target pendingTarget) {
	if strings.EqualFold(target.Domain, Domain) {
		markLocalDelivered(target)
		return
	}

	db, err := getSenderDB()
	if err != nil {
		log.Printf("ERROR: sender: db open: %s", err)
		return
	}

	// Load the message fields and its add-to batches (read-only).
	rtx, err := db.Begin()
	if err != nil {
		log.Printf("ERROR: sender: begin read tx: %s", err)
		return
	}
	m, err := loadMsgFields(rtx, target.MsgID)
	if err != nil {
		log.Printf("ERROR: sender: %s", err)
		rtx.Rollback()
		return
	}
	batches, err := loadAddToBatches(rtx, target.MsgID)
	if err != nil {
		log.Printf("ERROR: sender: %s", err)
		rtx.Rollback()
		return
	}
	rtx.Rollback()

	// SPEC §10.2: a host must not transmit a reply to a terminal message, nor
	// an add-to batch of one. dd.sql refuses to create such rows, so this is
	// defensive; matching units are recorded as code 1 (invalid, not
	// retryable) rather than sent, so they stop being picked up.
	parentTerminal, err := isStoredMsgTerminal(db, m.parentPid)
	if err != nil {
		log.Printf("ERROR: sender: checking parent of msg %d for terminal: %s", target.MsgID, err)
		return
	}

	// Deliver the original message to its pending msg_to recipients.
	parentPending, err := parentPendingForDomain(db, m.parentPid, target.Domain)
	if err != nil {
		log.Printf("ERROR: sender: checking pending parent of %d: %s", target.MsgID, err)
		return
	}
	if parentTerminal {
		log.Printf("ERROR: sender: msg %d references a terminal parent; not sending (SPEC §10.2)", target.MsgID)
		recordUnitInvalid(db, target, "msg_to", 0)
	} else if len(m.prepared) > 0 && !parentPending {
		deliverUnit(db, target, m.prepared, m.storedHash, "msg_to", 0)
	}

	// Deliver each add-to batch as its own add-to message (one sender each).
	for _, b := range batches {
		if len(b.Recipients) == 0 {
			continue // degenerate batch; an add-to message needs ≥ 1 recipient
		}
		if m.isTerminal {
			log.Printf("ERROR: sender: msg %d is terminal; not sending add-to batch %d (SPEC §12)", target.MsgID, b.ID)
			recordUnitInvalid(db, target, "msg_add_to", b.ID)
			continue
		}
		deliverUnit(db, target, b.Prepared, b.Hash, "msg_add_to", b.ID)
	}
}

// Hashes exist before network delivery now. Keep a reply queued while this
// host still owes the target domain its parent (including a selected batch),
// instead of racing that delivery and receiving a terminal parent-not-found.
func parentPendingForDomain(db *sql.DB, hash []byte, domain string) (bool, error) {
	if len(hash) == 0 {
		return false, nil
	}
	var pending bool
	err := db.QueryRow(`
	 WITH parent AS (
	   SELECT m.id, NULL::bigint AS batch_id FROM msg m WHERE m.sha256=$1
	   UNION ALL SELECT b.msg_id,b.id FROM msg_add_to_batch b WHERE b.sha256=$1
	 ), deliveries AS (
	   SELECT t.time_delivered AS delivered,t.response_code AS code
	   FROM parent p JOIN msg_to t ON t.msg_id=p.id
	   WHERE p.batch_id IS NULL AND lower(split_part(t.addr,'@',3))=lower($2)
	   UNION ALL
	   SELECT a.time_delivered,a.response_code FROM parent p JOIN msg_add_to a ON a.msg_id=p.id
	   WHERE (p.batch_id IS NULL OR a.batch_id=p.batch_id) AND lower(split_part(a.addr,'@',3))=lower($2)
	   UNION ALL
	   SELECT n.time_notified,n.response_code FROM parent p
	   JOIN msg_add_to_batch b ON b.msg_id=p.id JOIN msg_add_to_notify n ON n.batch_id=b.id
	   WHERE (p.batch_id IS NULL OR b.id=p.batch_id) AND lower(n.domain)=lower($2)
	 )
	 SELECT EXISTS(SELECT 1 FROM deliveries WHERE delivered IS NULL AND (code IS NULL OR code=ANY($3)))
	 AND NOT EXISTS(SELECT 1 FROM deliveries WHERE delivered IS NOT NULL)
	`, hash, domain, pq.Array(retryableResponseCodes)).Scan(&pending)
	return pending, err
}

// recordUnitInvalid records code 1 (invalid) against one delivery unit's
// pending recipients and notify row for a domain without transmitting
// anything, for a unit the protocol forbids sending. Code 1 is not retryable,
// so the rows drop out of findPendingTargets.
func recordUnitInvalid(db *sql.DB, target pendingTarget, table string, batchID int64) {
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
	locked, err := lockPendingRecipients(tx, table, target.MsgID, target.Domain, batchID, now)
	if err != nil {
		log.Printf("ERROR: sender: lock %s rows for msg %d: %s", table, target.MsgID, err)
		return
	}
	var notifyID int64
	if table == "msg_add_to" {
		notifyID, err = lockPendingNotify(tx, batchID, target.Domain, now)
		if err != nil {
			log.Printf("ERROR: sender: lock notify row for batch %d: %s", batchID, err)
			return
		}
	}
	if len(locked) == 0 && notifyID == 0 {
		return
	}
	if err := updateLocked(tx, table, locked, target.MsgID, batchID, now, int(RejectCodeInvalid), false); err != nil {
		return
	}
	if err := updateNotify(tx, notifyID, now, int(RejectCodeInvalid), false); err != nil {
		return
	}
	commitOrLog(tx, &committed, target.MsgID)
}

// markLocalDelivered marks a message's local-domain msg_to recipients delivered
// rather than sending over the network. (findPendingTargets skips the local
// domain, so this is a defensive path.)
func markLocalDelivered(target pendingTarget) {
	db, err := getSenderDB()
	if err != nil {
		log.Printf("ERROR: sender: db open for local delivery: %s", err)
		return
	}
	now := timeutil.TimestampNow().Float64()
	if _, err := db.Exec(`
		UPDATE msg_to SET time_delivered = $1, response_code = 200
		WHERE msg_id = $2 AND time_delivered IS NULL
		  AND lower(split_part(addr, '@', 3)) = lower($3)
	`, now, target.MsgID, target.Domain); err != nil {
		log.Printf("ERROR: sender: marking local recipients delivered for msg %d: %s", target.MsgID, err)
	}
}

// dialTargetIPs returns only a successfully connected TLS connection. Failed
// dials return a nil *tls.Conn, which must not be retained in a net.Conn interface.
func dialTargetIPs(targetIPs []net.IP, port int, tlsConf *tls.Config) net.Conn {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	for _, ip := range targetIPs {
		addr := net.JoinHostPort(ip.String(), fmt.Sprintf("%d", port))
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, tlsConf)
		if err == nil {
			return conn
		}
		log.Printf("WARN: sender: connect to %s failed: %s", addr, err)
	}
	return nil
}

type domainRecip struct {
	addr     string
	isLocked bool
}

// buildDomainRecips builds the per-recipient wire-order list for targetDomain
// (to-field order, then add-to order per SPEC §10.2 step 6). Only entries
// belonging to table (msg_to vs msg_add_to) are marked isLocked so an address
// present in both To and AddTo is never double-updated against the wrong table.
func buildDomainRecips(h *FMsgHeader, targetDomain, table string, locked []string) []domainRecip {
	lockedSet := make(map[string]bool, len(locked))
	for _, a := range locked {
		lockedSet[strings.ToLower(a)] = true
	}
	var domainRecips []domainRecip
	appendDomain := func(addrs []FMsgAddress, matchTable bool) {
		for _, addr := range addrs {
			if !strings.EqualFold(addr.Domain, targetDomain) {
				continue
			}
			s := addr.ToString()
			domainRecips = append(domainRecips, domainRecip{
				addr:     s,
				isLocked: matchTable && lockedSet[strings.ToLower(s)],
			})
		}
	}
	appendDomain(h.To, table == "msg_to")
	if h.Flags&FlagHasAddTo != 0 {
		appendDomain(h.AddTo, table == "msg_add_to")
	}
	return domainRecips
}

// deliverUnit sends one wire message — the original message or a single add-to
// batch — to target.Domain over its own connection, recording per-recipient
// outcomes. It owns its transaction: it locks this unit's pending recipients in
// `table` (msg_add_to rows restricted to batchID; batchID is 0 for the original
// message) and marks them for retry on early failure.
func deliverUnit(db *sql.DB, target pendingTarget, prepared, expectedHash []byte, table string, batchID int64) {
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
	locked, err := lockPendingRecipients(tx, table, target.MsgID, target.Domain, batchID, now)
	if err != nil {
		log.Printf("ERROR: sender: lock %s rows for msg %d: %s", table, target.MsgID, err)
		return
	}
	// An add-to unit may be owed to this domain as a participant notification
	// even when it hosts none of the batch's recipients (SPEC §10.2).
	var notifyID int64
	if table == "msg_add_to" {
		notifyID, err = lockPendingNotify(tx, batchID, target.Domain, now)
		if err != nil {
			log.Printf("ERROR: sender: lock notify row for batch %d: %s", batchID, err)
			return
		}
	}
	if len(locked) == 0 && notifyID == 0 {
		return // nothing pending for this unit (or locked by another sender)
	}

	h, err := fmsg.UnmarshalPrepared(prepared, expectedHash)
	if err != nil {
		log.Printf("ERROR: sender: load finalized %s (batch %d) of msg %d: %s", table, batchID, target.MsgID, err)
		return
	}

	deferRetry := true
	defer func() {
		if deferRetry && !committed {
			recordRetryableFailure(tx, &committed, table, locked, target.MsgID, batchID, notifyID)
		}
	}()

	// Per-recipient response codes arrive one per recipient entry, in to-field
	// order then add-to order (SPEC §10.2 step 6). An address in both lists is a
	// recipient of each and so is counted, and answered, twice. Only this
	// unit's locked recipients are recorded; the recipients carried only to
	// reconstruct that ordering stay unlocked.
	domainRecips := buildDomainRecips(h, target.Domain, table, locked)

	// --- network delivery ---

	hash := h.GetHeaderHash()
	hashArr := *(*[32]byte)(hash)

	targetIPs, err := lookupAuthorisedIPs(target.Domain)
	if err != nil {
		log.Printf("ERROR: sender: DNS lookup for fmsg.%s failed: %s", target.Domain, err)
		outboundDomains.setReachable(target.Domain, false)
		return
	}

	tlsConf := buildClientTLSConfig("fmsg." + target.Domain)
	conn := dialTargetIPs(targetIPs, RemotePort, tlsConf)
	if conn == nil {
		log.Printf("ERROR: sender: could not connect to any IP for fmsg.%s", target.Domain)
		outboundDomains.setReachable(target.Domain, false)
		return
	}
	defer conn.Close()
	outboundDomains.setReachable(target.Domain, true)

	// Register in outgoing map with Host B's IP before sending the header so
	// any incoming challenge can be matched by hash AND IP (§10.2 step 2).
	connectedIP := conn.RemoteAddr().(*net.TCPAddr).IP.String()
	log.Printf("INFO: sender: registering outgoing message %s (%s)", hex.EncodeToString(hashArr[:]), connectedIP)
	registerOutgoing(hashArr, h, connectedIP)
	defer removeOutgoingIP(hashArr, connectedIP)

	// Step 3: Transmit message header.
	encodedHeader := h.Encode()
	conn.SetWriteDeadline(time.Now().Add(calcNetIODuration(int64(len(encodedHeader)), MinUploadRate)))
	if _, err := conn.Write(encodedHeader); err != nil {
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
		deferRetry = false
		if err := updateLocked(tx, table, locked, target.MsgID, batchID, now, int(initCode[0]), true); err != nil {
			return
		}
		if err := updateNotify(tx, notifyID, now, int(initCode[0]), true); err != nil {
			return
		}
		commitOrLog(tx, &committed, target.MsgID)
		return
	default:
		if initCode[0] >= 1 && initCode[0] <= 10 {
			// global rejection
			log.Printf("WARN: sender: msg %d rejected by %s: %s (%d)",
				target.MsgID, target.Domain, responseCodeName(initCode[0]), initCode[0])
			deferRetry = false
			if err := updateLocked(tx, table, locked, target.MsgID, batchID, now, int(initCode[0]), false); err != nil {
				return
			}
			if err := updateNotify(tx, notifyID, now, int(initCode[0]), false); err != nil {
				return
			}
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

	deferRetry = false
	for i, dr := range domainRecips {
		if !dr.isLocked {
			continue
		}
		c := codes[i]
		delivered := c == RejectCodeAccept
		if delivered {
			log.Printf("INFO: sender: delivered msg %d to %s", target.MsgID, dr.addr)
		} else {
			log.Printf("WARN: sender: msg %d to %s: %s (%d)", target.MsgID, dr.addr, responseCodeName(c), c)
		}
		if err := updateRecipient(tx, table, dr.addr, target.MsgID, batchID, now, int(c), delivered); err != nil {
			return
		}
	}

	// The exchange completed, so a locked notify row is satisfied too: the
	// domain was informed of the batch through this delivery.
	if err := updateNotify(tx, notifyID, now, int(initCode[0]), true); err != nil {
		return
	}

	commitOrLog(tx, &committed, target.MsgID)
}

// domainReachability remembers which domains the sender could not connect to
// on its last attempt. A domain is forgotten (reachable again) as soon as a
// connection to it succeeds, so the set stays as small as the number of peers
// that are down right now.
type domainReachability struct {
	mu          sync.Mutex
	unreachable map[string]struct{}
}

// outboundDomains is the sender's view of which peers it can currently reach.
var outboundDomains = &domainReachability{unreachable: make(map[string]struct{})}

func (d *domainReachability) setReachable(domain string, reachable bool) {
	k := strings.ToLower(domain)
	d.mu.Lock()
	defer d.mu.Unlock()
	if reachable {
		delete(d.unreachable, k)
	} else {
		d.unreachable[k] = struct{}{}
	}
}

func (d *domainReachability) isUnreachable(domain string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, down := d.unreachable[strings.ToLower(domain)]
	return down
}

// inFlightTracker deduplicates dispatch against deliveries already running,
// and holds a domain the sender could not connect to down to one delivery at
// a time. That one delivery is the probe: while it fails, the rest of the
// domain's backlog waits for a later poll instead of each taking a delivery
// slot for the length of a connect timeout, and when it connects the domain
// gets its full share again.
type inFlightTracker struct {
	mu        sync.Mutex
	active    map[pendingTarget]struct{}
	perDomain map[string]int
	domains   *domainReachability
}

func newInFlightTracker() *inFlightTracker {
	return &inFlightTracker{
		active:    make(map[pendingTarget]struct{}),
		perDomain: make(map[string]int),
		domains:   outboundDomains,
	}
}

func (f *inFlightTracker) acquire(t pendingTarget) bool {
	k := pendingTarget{MsgID: t.MsgID, Domain: strings.ToLower(t.Domain)}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.active[k]; exists {
		return false
	}
	if f.perDomain[k.Domain] > 0 && f.domains.isUnreachable(k.Domain) {
		return false
	}
	f.active[k] = struct{}{}
	f.perDomain[k.Domain]++
	return true
}

func (f *inFlightTracker) release(t pendingTarget) {
	k := pendingTarget{MsgID: t.MsgID, Domain: strings.ToLower(t.Domain)}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.active[k]; !exists {
		return
	}
	delete(f.active, k)
	if f.perDomain[k.Domain]--; f.perDomain[k.Domain] <= 0 {
		delete(f.perDomain, k.Domain)
	}
}

// processPendingMessages finds messages needing delivery and dispatches a
// goroutine per (message, domain) pair, bounded by the semaphore and
// deduplicated against currently in-flight targets.
func processPendingMessages(ctx context.Context, sem chan struct{}, inFlight *inFlightTracker) {
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
		if !inFlight.acquire(t) {
			continue
		}
		select {
		case <-ctx.Done():
			inFlight.release(t)
			return
		case sem <- struct{}{}:
		}
		go func(t pendingTarget) {
			defer func() {
				<-sem
				inFlight.release(t)
			}()
			deliverMessage(t)
		}(t)
	}
}

// startSender runs the sender loop: polls the database periodically and also
// listens for PostgreSQL notifications for immediate pickup of new messages.
func startSender(ctx context.Context) {
	loadSenderEnvConfig()
	slots := deliverySlots(MaxConcurrentSend, SenderDBMaxConns)
	log.Printf("INFO: sender: started (poll=%ds, retry=%.0fs, max_concurrent=%d, db_max_conns=%d, deliveries_at_once=%d)",
		PollInterval, RetryInterval, MaxConcurrentSend, SenderDBMaxConns, slots)

	sem := make(chan struct{}, slots)
	inFlight := newInFlightTracker()

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
		pingTimer := time.NewTimer(32 * time.Second)
		defer pingTimer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case n := <-listener.Notify:
				if !pingTimer.Stop() {
					select {
					case <-pingTimer.C:
					default:
					}
				}
				pingTimer.Reset(32 * time.Second)
				if n != nil {
					log.Printf("INFO: sender: notification received: %s", n.Extra)
					select {
					case notifyCh <- struct{}{}:
					default:
					}
				}
			case <-pingTimer.C:
				pingTimer.Reset(32 * time.Second)
				// ping to keep connection alive
				if err := listener.Ping(); err != nil {
					log.Printf("ERROR: sender: pg listener ping: %s", err)
				}
			}
		}
	}()

	// initial poll on startup
	processPendingMessages(ctx, sem, inFlight)

	ticker := time.NewTicker(time.Duration(PollInterval) * time.Second)
	defer ticker.Stop()
	batchTimer := time.NewTimer(0)
	if !batchTimer.Stop() {
		select {
		case <-batchTimer.C:
		default:
		}
	}
	defer batchTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			processPendingMessages(ctx, sem, inFlight)
		case <-notifyCh:
			// small delay to batch rapid inserts
			batchTimer.Reset(256 * time.Millisecond)
			select {
			case <-ctx.Done():
				return
			case <-batchTimer.C:
			}
			processPendingMessages(ctx, sem, inFlight)
		}
	}
}
