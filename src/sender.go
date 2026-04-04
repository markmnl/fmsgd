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

	// query both msg_to and msg_add_to for pending targets
	rows, err := db.Query(`
		SELECT mt.msg_id, mt.addr
		FROM msg_to mt
		INNER JOIN msg m ON m.id = mt.msg_id
		WHERE mt.time_delivered IS NULL
		  AND m.time_sent IS NOT NULL
		  AND (mt.response_code IS NULL OR mt.response_code IN (3, 5))
		  AND (mt.time_last_attempt IS NULL OR ($1 - mt.time_last_attempt) > $2)
		  AND ($1 - m.time_sent) < $3
		UNION ALL
		SELECT mat.msg_id, mat.addr
		FROM msg_add_to mat
		INNER JOIN msg m ON m.id = mat.msg_id
		WHERE mat.time_delivered IS NULL
		  AND m.time_sent IS NOT NULL
		  AND (mat.response_code IS NULL OR mat.response_code IN (3, 5))
		  AND (mat.time_last_attempt IS NULL OR ($1 - mat.time_last_attempt) > $2)
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
		  AND (mt.response_code IS NULL OR mt.response_code IN (3, 5))
		  AND (mt.time_last_attempt IS NULL OR ($2 - mt.time_last_attempt) > $3)
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
		  AND (mat.response_code IS NULL OR mat.response_code IN (3, 5))
		  AND (mat.time_last_attempt IS NULL OR ($2 - mat.time_last_attempt) > $3)
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

	// Register in outgoing map so challenge handler can look up this message
	hash := h.GetHeaderHash()
	hashArr := *(*[32]byte)(hash)
	log.Printf("INFO: sender: registering outgoing message %s", hex.EncodeToString(hash[:]))
	registerOutgoing(hashArr, h)
	defer deleteOutgoing(hashArr)

	// Build the list of recipients on the target domain (in order) and
	// note which ones we locked (i.e. are pending delivery this round). Per spec,
	// response codes arrive per-recipient in to then add-to order excluding other domains.
	// TODO check retry logic when some recipients on this domain are not locked
	// (e.g. already delivered or locked by another sender) — do we still get
	// per-recipient codes for the locked ones?
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

	// TODO [Spec]: DNSSEC validation SHOULD be performed on the DNS lookup.
	// If DNSSEC validation fails the connection MUST terminate (no retry).
	// lookupAuthorisedIPs does not currently perform or report DNSSEC validation.
	targetIPs, err := lookupAuthorisedIPs(target.Domain)
	if err != nil {
		log.Printf("ERROR: sender: DNS lookup for _fmsg.%s failed: %s", target.Domain, err)
		return // rollback, retry later
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
		return // rollback, retry later
	}
	defer conn.Close()

	// Send header — Encode() writes all fields through attachment headers per spec
	headerBytes := h.Encode()
	if _, err := conn.Write(headerBytes); err != nil {
		log.Printf("ERROR: sender: writing header: %s", err)
		return
	}

	// message data
	fd, err := os.Open(h.Filepath)
	if err != nil {
		log.Printf("ERROR: sender: opening file %s: %s", h.Filepath, err)
		return
	}
	defer fd.Close()

	conn.SetWriteDeadline(time.Now().Add(calcNetIODuration(int(h.Size), MinUploadRate)))
	n, err := io.CopyN(conn, fd, int64(h.Size))
	if n != int64(h.Size) {
		log.Printf("ERROR: sender: file size mismatch for msg %d: expected %d, got %d", target.MsgID, h.Size, n)
		return
	}
	if err != nil {
		log.Printf("ERROR: sender: sending data (%d/%d bytes): %s", n, h.Size, err)
		return
	}

	// attachment data — sequential byte sequences bounded by header sizes
	for _, att := range h.Attachments {
		af, err := os.Open(att.Filepath)
		if err != nil {
			log.Printf("ERROR: sender: opening attachment %s: %s", att.Filename, err)
			return
		}
		an, err := io.CopyN(conn, af, int64(att.Size))
		af.Close()
		if an != int64(att.Size) {
			log.Printf("ERROR: sender: attachment %s size mismatch: expected %d, got %d", att.Filename, att.Size, an)
			return
		}
		if err != nil {
			log.Printf("ERROR: sender: sending attachment %s: %s", att.Filename, err)
			return
		}
	}

	// --- read response ---
	// A code < 100 is a global rejection (single byte for all recipients).
	// Otherwise one code per recipient on this domain, in To-field order.
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))

	firstByte := make([]byte, 1)
	if _, err := io.ReadFull(conn, firstByte); err != nil {
		log.Printf("ERROR: sender: reading response: %s", err)
		return // rollback, retry later
	}

	code := firstByte[0]
	now = timeutil.TimestampNow().Float64()

	if code < 100 {
		// Code 11 (accept add to) — additional recipients received.
		if code == AcceptCodeAddTo {
			log.Printf("INFO: sender: msg %d additional recipients received by %s (code 11)", target.MsgID, target.Domain)
			for _, a := range lockedAddrs {
				if _, err := tx.Exec(`
					UPDATE msg_to SET time_delivered = $1, response_code = $2
					WHERE msg_id = $3 AND addr = $4
				`, now, int(code), target.MsgID, a); err != nil {
					log.Printf("ERROR: sender: update delivered for %s: %s", a, err)
				}
			}
			for _, a := range lockedAddToAddrs {
				if _, err := tx.Exec(`
					UPDATE msg_add_to SET time_delivered = $1, response_code = $2
					WHERE msg_id = $3 AND addr = $4
				`, now, int(code), target.MsgID, a); err != nil {
					log.Printf("ERROR: sender: update delivered add-to for %s: %s", a, err)
				}
			}
			if err := tx.Commit(); err != nil {
				log.Printf("ERROR: sender: commit tx for msg %d: %s", target.MsgID, err)
			} else {
				committed = true
			}
			return
		}

		// global rejection — update all locked recipients
		//
		// TODO [Spec]: Permanent failures (1 invalid, 2 unsupported version,
		// 4 too big, 10 duplicate) should NOT be retried. Currently all global
		// codes are stored identically; findPendingTargets only retries codes
		// 3 and 5, which is correct for global codes. But ensure code 10
		// (duplicate) is explicitly recognised as permanent and not retried.
		log.Printf("WARN: sender: msg %d rejected by %s: %s (%d)",
			target.MsgID, target.Domain, responseCodeName(code), code)
		for _, a := range lockedAddrs {
			if _, err := tx.Exec(`
				UPDATE msg_to SET time_last_attempt = $1, response_code = $2
				WHERE msg_id = $3 AND addr = $4
			`, now, int(code), target.MsgID, a); err != nil {
				log.Printf("ERROR: sender: update last attempt for %s: %s", a, err)
			}
		}
		for _, a := range lockedAddToAddrs {
			if _, err := tx.Exec(`
				UPDATE msg_add_to SET time_last_attempt = $1, response_code = $2
				WHERE msg_id = $3 AND addr = $4
			`, now, int(code), target.MsgID, a); err != nil {
				log.Printf("ERROR: sender: update last attempt add-to for %s: %s", a, err)
			}
		}
		if err := tx.Commit(); err != nil {
			log.Printf("ERROR: sender: commit tx for msg %d: %s", target.MsgID, err)
		} else {
			committed = true
		}
		return
	}

	// per-recipient codes
	codes := make([]byte, len(domainRecips))
	codes[0] = code
	if len(domainRecips) > 1 {
		rest := make([]byte, len(domainRecips)-1)
		if _, err := io.ReadFull(conn, rest); err != nil {
			log.Printf("ERROR: sender: reading remaining response codes: %s", err)
			return // rollback, retry later
		}
		copy(codes[1:], rest)
	}

	for i, dr := range domainRecips {
		if !dr.isLocked {
			continue // not our responsibility this round
			// TODO well receiving host still attempted delivery to this recipient — do we update response code for it? Spec is not clear on this scenario.
		}
		c := codes[i]
		table := "msg_to"
		if dr.isAddTo {
			table = "msg_add_to"
		}
		if c == RejectCodeAccept {
			log.Printf("INFO: sender: delivered msg %d to %s", target.MsgID, dr.addr)
			if _, err := tx.Exec(fmt.Sprintf(`
				UPDATE %s SET time_delivered = $1, response_code = 200
				WHERE msg_id = $2 AND addr = $3
			`, table), now, target.MsgID, dr.addr); err != nil {
				log.Printf("ERROR: sender: update delivered for %s: %s", dr.addr, err)
			}
		} else {
			log.Printf("WARN: sender: msg %d to %s rejected: %s (%d)",
				target.MsgID, dr.addr, responseCodeName(c), c)
			if _, err := tx.Exec(fmt.Sprintf(`
				UPDATE %s SET time_last_attempt = $1, response_code = $2
				WHERE msg_id = $3 AND addr = $4
			`, table), now, int(c), target.MsgID, dr.addr); err != nil {
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
