package main

import (
	"crypto/tls"
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

// updateLocked applies the same outcome to every locked address in one table.
func updateLocked(tx *sql.Tx, table string, addrs []string, msgID int64, now float64, code int, delivered bool) {
	for _, a := range addrs {
		updateRecipient(tx, table, a, msgID, now, code, delivered)
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

// recordRetryableFailure marks one delivery unit's locked recipients and
// locked notify row (if any) for retry.
func recordRetryableFailure(tx *sql.Tx, committed *bool, table string, locked []string, msgID int64, notifyID int64) {
	now := timeutil.TimestampNow().Float64()
	updateLocked(tx, table, locked, msgID, now, localResponseCodeNoResponse, false)
	updateNotify(tx, notifyID, now, localResponseCodeNoResponse, false)
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
func updateNotify(tx *sql.Tx, notifyID int64, now float64, code int, notified bool) {
	if notifyID == 0 {
		return
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
}

// deflatePart is one compressed payload (message body or attachment).
type deflatePart struct {
	path     string
	size     uint32
	expanded uint32
}

// deflateState captures the result of compressing a message's body and
// attachments once, so the same compressed payload can be applied to every
// outgoing wire header — the original message and each add-to batch share the
// shared message data (SPEC §12).
type deflateState struct {
	body     deflatePart
	bodyUsed bool
	atts     []deflatePart
	cleanup  []string // temp files to remove once delivery completes
}

// computeDeflate compresses the message body and each attachment of m where
// worthwhile. Apply the result to a unit header with applyTo; remove its temp
// files with removeTempFiles after delivery.
func computeDeflate(m *msgFields, msgID int64) deflateState {
	var d deflateState
	d.atts = make([]deflatePart, len(m.attachments))

	if shouldCompress(m.typ, uint32(m.size)) {
		dp, cs, ok, derr := tryCompress(m.filepath, uint32(m.size))
		if derr != nil {
			log.Printf("WARN: sender: compress msg data for msg %d: %s", msgID, derr)
		} else if ok {
			log.Printf("INFO: sender: compressed msg %d data: %d -> %d bytes", msgID, m.size, cs)
			d.body = deflatePart{path: dp, size: cs, expanded: uint32(m.size)}
			d.bodyUsed = true
			d.cleanup = append(d.cleanup, dp)
		}
	}
	for i := range m.attachments {
		att := m.attachments[i]
		if !shouldCompress(att.Type, att.Size) {
			continue
		}
		dp, cs, ok, derr := tryCompress(att.Filepath, att.Size)
		if derr != nil {
			log.Printf("WARN: sender: compress attachment %s for msg %d: %s", att.Filename, msgID, derr)
		} else if ok {
			log.Printf("INFO: sender: compressed msg %d attachment %s: %d -> %d bytes", msgID, att.Filename, att.Size, cs)
			d.atts[i] = deflatePart{path: dp, size: cs, expanded: att.Size}
			d.cleanup = append(d.cleanup, dp)
		}
	}
	return d
}

// applyTo rewrites h's body and attachment fields to send the compressed
// payloads, setting the corresponding deflate flags.
func (d deflateState) applyTo(h *FMsgHeader) {
	if d.bodyUsed {
		h.Filepath = d.body.path
		h.ExpandedSize = d.body.expanded
		h.Size = d.body.size
		h.Flags |= FlagDeflate
	}
	for i := range h.Attachments {
		if i >= len(d.atts) || d.atts[i].path == "" {
			continue
		}
		h.Attachments[i].Filepath = d.atts[i].path
		h.Attachments[i].ExpandedSize = d.atts[i].expanded
		h.Attachments[i].Size = d.atts[i].size
		h.Attachments[i].Flags |= 1 << 1
	}
}

// removeTempFiles deletes the compression temp files.
func (d deflateState) removeTempFiles() {
	for _, p := range d.cleanup {
		_ = os.Remove(p)
	}
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
	args := []interface{}{msgID, now, RetryInterval, RetryMaxAge, pq.Array(retryableResponseCodes)}
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

	db, err := sql.Open("postgres", "")
	if err != nil {
		log.Printf("ERROR: sender: db open: %s", err)
		return
	}
	defer db.Close()

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

	// Compress the shared payload once; every unit header reuses it. Deflate
	// must be applied BEFORE the shared hash is computed: the message hash
	// covers the header fields exactly as transmitted (SPEC "Message hash"),
	// and applyTo changes flags, size and expanded size. Hashing the
	// undeflated form recorded a sha256 the receiving host never computes,
	// so cross-host replies bounced with code 6 (parent not found).
	d := computeDeflate(m, target.MsgID)
	defer d.removeTempFiles()

	orig := m.originalHeader()
	d.applyTo(orig)

	sharedHash := m.storedHash
	if len(sharedHash) == 0 {
		sharedHash, err = orig.GetMessageHash()
		if err != nil {
			log.Printf("ERROR: sender: computing message hash for msg %d: %s", target.MsgID, err)
			return
		}
	}

	// Persist the shared hash (so replies/add-to referencing this message
	// resolve) and link any pending children — once for the whole message.
	if err := ensureSharedHash(db, target.MsgID, sharedHash); err != nil {
		log.Printf("ERROR: sender: %s", err)
		return
	}

	// Deliver the original message to its pending msg_to recipients.
	deliverUnit(db, target, orig, "msg_to", 0)

	// Deliver each add-to batch as its own add-to message (one sender each).
	for _, b := range batches {
		if len(b.Recipients) == 0 {
			continue // degenerate batch; an add-to message needs ≥ 1 recipient
		}
		h := m.addToHeader(b, sharedHash)
		d.applyTo(h)
		deliverUnit(db, target, h, "msg_add_to", b.ID)
	}
}

// markLocalDelivered marks a message's local-domain msg_to recipients delivered
// rather than sending over the network. (findPendingTargets skips the local
// domain, so this is a defensive path.)
func markLocalDelivered(target pendingTarget) {
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
}

// ensureSharedHash persists the message's canonical hash when not yet stored and
// resolves any pending child (reply/add-to) links that reference it.
func ensureSharedHash(db *sql.DB, msgID int64, sharedHash []byte) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE msg SET sha256 = $1 WHERE id = $2 AND sha256 IS NULL`, sharedHash, msgID); err != nil {
		return fmt.Errorf("storing sha256 for msg %d: %w", msgID, err)
	}
	if err := resolvePendingChildLinks(txParentLinkStore{tx: tx}, msgID, sharedHash); err != nil {
		return fmt.Errorf("resolving child pids for msg %d: %w", msgID, err)
	}
	return tx.Commit()
}

// deliverUnit sends one wire message — the original message or a single add-to
// batch — to target.Domain over its own connection, recording per-recipient
// outcomes. It owns its transaction: it locks this unit's pending recipients in
// `table` (msg_add_to rows restricted to batchID; batchID is 0 for the original
// message) and marks them for retry on early failure.
func deliverUnit(db *sql.DB, target pendingTarget, h *FMsgHeader, table string, batchID int64) {
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

	deferRetry := true
	defer func() {
		if deferRetry && !committed {
			recordRetryableFailure(tx, &committed, table, locked, target.MsgID, notifyID)
		}
	}()

	// Per-recipient response codes arrive one per recipient entry, in to-field
	// order then add-to order (SPEC §10.2 step 6). An address in both lists is a
	// recipient of each and so is counted, and answered, twice. Only this
	// unit's locked recipients are recorded; the recipients carried only to
	// reconstruct that ordering stay unlocked.
	lockedSet := make(map[string]bool, len(locked))
	for _, a := range locked {
		lockedSet[strings.ToLower(a)] = true
	}
	type domainRecip struct {
		addr     string
		isLocked bool
	}
	var domainRecips []domainRecip
	appendDomain := func(addrs []FMsgAddress) {
		for _, addr := range addrs {
			if !strings.EqualFold(addr.Domain, target.Domain) {
				continue
			}
			s := addr.ToString()
			domainRecips = append(domainRecips, domainRecip{addr: s, isLocked: lockedSet[strings.ToLower(s)]})
		}
	}
	appendDomain(h.To)
	if h.Flags&FlagHasAddTo != 0 {
		appendDomain(h.AddTo)
	}

	// --- network delivery ---

	hash := h.GetHeaderHash()
	hashArr := *(*[32]byte)(hash)

	targetIPs, err := lookupAuthorisedIPs(target.Domain)
	if err != nil {
		log.Printf("ERROR: sender: DNS lookup for fmsg.%s failed: %s", target.Domain, err)
		return
	}

	var conn net.Conn
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	tlsConf := buildClientTLSConfig("fmsg." + target.Domain)
	for _, ip := range targetIPs {
		addr := net.JoinHostPort(ip.String(), fmt.Sprintf("%d", RemotePort))
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConf)
		if err == nil {
			break
		}
		log.Printf("WARN: sender: connect to %s failed: %s", addr, err)
	}
	if conn == nil {
		log.Printf("ERROR: sender: could not connect to any IP for fmsg.%s", target.Domain)
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
		updateLocked(tx, table, locked, target.MsgID, now, int(initCode[0]), true)
		updateNotify(tx, notifyID, now, int(initCode[0]), true)
		deferRetry = false
		commitOrLog(tx, &committed, target.MsgID)
		return
	default:
		if initCode[0] >= 1 && initCode[0] <= 10 {
			// global rejection
			log.Printf("WARN: sender: msg %d rejected by %s: %s (%d)",
				target.MsgID, target.Domain, responseCodeName(initCode[0]), initCode[0])
			updateLocked(tx, table, locked, target.MsgID, now, int(initCode[0]), false)
			updateNotify(tx, notifyID, now, int(initCode[0]), false)
			deferRetry = false
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
		delivered := c == RejectCodeAccept
		if delivered {
			log.Printf("INFO: sender: delivered msg %d to %s", target.MsgID, dr.addr)
		} else {
			log.Printf("WARN: sender: msg %d to %s: %s (%d)", target.MsgID, dr.addr, responseCodeName(c), c)
		}
		updateRecipient(tx, table, dr.addr, target.MsgID, now, int(c), delivered)
	}

	// The exchange completed, so a locked notify row is satisfied too: the
	// domain was informed of the batch through this delivery.
	updateNotify(tx, notifyID, now, int(initCode[0]), true)

	deferRetry = false
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
