package main

// FMSG-006 Indicators: short-lived, single-emoji states (such as composing a
// reply) that a participant shows on a message, carried host to host as
// unacknowledged UDP datagrams and never stored.
//
// This host and the client API (fmsg-webapi) share the PostgreSQL NOTIFY
// channel "indicator", carrying {"id","from","emoji","ttl"} JSON in both
// directions:
//   - the client API notifies when a local participant sets or clears an
//     indicator; this host sends it on to the subject's other participant
//     domains;
//   - this host notifies when it receives and verifies a datagram; the client
//     API delivers it to connected local participants.
// A received indicator is always from a remote address, and this host only
// sends for addresses it hosts, so nothing it notifies is sent again.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"

	"github.com/markmnl/fmsgd/internal/emoji"
)

const (
	indicatorChannel = "indicator"

	indicatorVersion     = 1
	indicatorDataMax     = 64                 // bytes
	indicatorFixedLen    = 1 + 32 + 2 + 1 + 1 // version, hash, index, ttl, data length
	indicatorMaxLen      = indicatorFixedLen + indicatorDataMax
	indicatorTTLMax      = 120             // INDICATOR_TTL_MAX, seconds
	indicatorHostRate    = 20              // INDICATOR_HOST_RATE, datagrams per second per receiving domain
	indicatorMinInterval = 2 * time.Second // INDICATOR_MIN_INTERVAL

	// Receiving: per-source-address token bucket.
	indicatorRecvRate  = 50.0
	indicatorRecvBurst = 100.0
	// indicatorRecvMaxSources bounds the per-source table; a datagram from a
	// new source is dropped while the table is full of active sources.
	indicatorRecvMaxSources = 100000
	// indicatorRecvWorkers bounds datagrams being verified concurrently; one
	// arriving while all are busy is dropped.
	indicatorRecvWorkers = 16

	indicatorDNSCacheTTL    = 60 * time.Second
	indicatorDNSNegativeTTL = 10 * time.Second
	indicatorDNSCacheMax    = 10000

	indicatorFlushInterval = 100 * time.Millisecond
	indicatorQueryTimeout  = 5 * time.Second
)

// isIndicatorLocalDomain reports whether domain is hosted by this host. A host
// sends indicators only for addresses it hosts, and drops a datagram claiming
// to come from one of them (FMSG-006 Sending, Receiving).
func isIndicatorLocalDomain(domain string) bool {
	return strings.EqualFold(domain, Domain)
}

// indicatorsEnabled reports whether FMSG_INDICATORS leaves indicators on
// (the default); "false" or "0" turns off both receiving and sending.
func indicatorsEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("FMSG_INDICATORS"))) {
	case "false", "0":
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Datagram

// indicatorDatagram is one FMSG-006 datagram (version 1).
type indicatorDatagram struct {
	Hash  [32]byte
	Index uint16
	TTL   uint8
	Data  string
}

// isClear reports whether the datagram clears the participant's state: empty
// data, or a ttl of 0.
func (d indicatorDatagram) isClear() bool { return d.Data == "" || d.TTL == 0 }

// marshal encodes the datagram, all integers little-endian.
func (d indicatorDatagram) marshal() []byte {
	b := make([]byte, 0, indicatorFixedLen+len(d.Data))
	b = append(b, indicatorVersion)
	b = append(b, d.Hash[:]...)
	b = binary.LittleEndian.AppendUint16(b, d.Index)
	b = append(b, d.TTL, byte(len(d.Data)))
	return append(b, d.Data...)
}

var errIndicatorInvalid = errors.New("invalid indicator datagram")

// isIndicatorEmoji reports whether s is valid indicator data: empty (clear) or
// exactly one emoji within the size bound, with FMSG-005's leniency.
func isIndicatorEmoji(s string) bool {
	return s == "" || (len(s) <= indicatorDataMax && emoji.IsSingle(s))
}

// parseIndicatorDatagram decodes and validates a datagram. It is exactly the
// fields; anything else, including trailing bytes, is invalid.
func parseIndicatorDatagram(b []byte) (indicatorDatagram, error) {
	var d indicatorDatagram
	if len(b) < indicatorFixedLen || b[0] != indicatorVersion {
		return d, errIndicatorInvalid
	}
	copy(d.Hash[:], b[1:33])
	d.Index = binary.LittleEndian.Uint16(b[33:35])
	d.TTL = b[35]
	n := int(b[36])
	if n > indicatorDataMax || len(b) != indicatorFixedLen+n {
		return d, errIndicatorInvalid
	}
	d.Data = string(b[indicatorFixedLen:])
	if !isIndicatorEmoji(d.Data) {
		return d, errIndicatorInvalid
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// Participants and subjects

// indicatorParticipants returns a message's participants in participant index
// order: from, each to, then — when the message has add to — add to from and
// each add to.
func indicatorParticipants(h *FMsgHeader) []FMsgAddress {
	parts := make([]FMsgAddress, 0, 1+len(h.To)+1+len(h.AddTo))
	parts = append(parts, h.From)
	parts = append(parts, h.To...)
	if h.Flags&FlagHasAddTo != 0 {
		if h.AddToFrom != nil {
			parts = append(parts, *h.AddToFrom)
		} else {
			parts = append(parts, h.From)
		}
		parts = append(parts, h.AddTo...)
	}
	return parts
}

// preparedHeader decodes a stored prepared message (msg.wire_message,
// msg_add_to_batch.wire_message) for its header fields only. Unlike
// fmsg.UnmarshalPrepared it does not re-hash the payload from disk: the row
// was found by its hash, and a datagram must not cost a read of the message.
func preparedHeader(data []byte) (*FMsgHeader, error) {
	var s struct {
		Version int
		Header  *FMsgHeader
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	if s.Version != 1 || s.Header == nil {
		return nil, fmt.Errorf("unsupported prepared message")
	}
	return s.Header, nil
}

// indicatorSubject is a message an indicator can refer to: the original
// message or one of its add-to batch messages, with its participants.
type indicatorSubject struct {
	Hash         []byte
	Participants []FMsgAddress
}

// participantIndex returns the lowest participant index of addr in subject,
// or -1.
func (s *indicatorSubject) participantIndex(addr FMsgAddress) int {
	for i, p := range s.Participants {
		if strings.EqualFold(p.ToString(), addr.ToString()) {
			return i
		}
	}
	return -1
}

func (s *indicatorSubject) hasDomain(domain string) bool {
	return hasDomainRecipient(s.Participants, domain)
}

// indicatorTarget is one datagram to send: the subject, by hash, and the
// indicating participant's index in it, to one receiving domain.
type indicatorTarget struct {
	Domain string
	Hash   [32]byte
	Index  uint16
}

// selectIndicatorTargets chooses, for each remote participant domain of a
// message, the subject that domain holds and that the indicating participant
// from belongs to, so the receiving host can resolve the index:
//   - the original, when one of the domain's participants is in the original's
//     from or to and from is too;
//   - otherwise the first add-to batch (by id) with a participant on the
//     domain and from among its participants.
//
// A domain with neither is skipped. The index is from's lowest index in the
// chosen subject. original may be nil; batches are in id order.
func selectIndicatorTargets(from FMsgAddress, original *indicatorSubject, batches []indicatorSubject, isLocal func(string) bool) []indicatorTarget {
	var domains []string
	seen := map[string]bool{}
	addDomains := func(s *indicatorSubject) {
		for _, p := range s.Participants {
			d := strings.ToLower(p.Domain)
			if !seen[d] && !isLocal(d) {
				seen[d] = true
				domains = append(domains, d)
			}
		}
	}
	if original != nil {
		addDomains(original)
	}
	for i := range batches {
		addDomains(&batches[i])
	}

	var targets []indicatorTarget
	for _, d := range domains {
		var subject *indicatorSubject
		idx := -1
		if original != nil && original.hasDomain(d) {
			if i := original.participantIndex(from); i >= 0 {
				subject, idx = original, i
			}
		}
		for b := 0; subject == nil && b < len(batches); b++ {
			if !batches[b].hasDomain(d) {
				continue
			}
			if i := batches[b].participantIndex(from); i >= 0 {
				subject, idx = &batches[b], i
			}
		}
		if subject == nil || len(subject.Hash) != 32 || idx > 0xFFFF {
			continue
		}
		t := indicatorTarget{Domain: d, Index: uint16(idx)}
		copy(t.Hash[:], subject.Hash)
		targets = append(targets, t)
	}
	return targets
}

// ---------------------------------------------------------------------------
// Store

// indicatorStore is the database seam: subject lookup for both directions and
// publishing received indicators to the client API.
type indicatorStore interface {
	// subjectByHash finds a stored message or add-to batch message by hash,
	// returning the local message id and the subject's participants, or
	// found false.
	subjectByHash(ctx context.Context, hash []byte) (msgID int64, participants []FMsgAddress, found bool, err error)
	// subjectsOf returns message msgID's original subject (nil when not
	// known) and its add-to batch subjects in id order.
	subjectsOf(ctx context.Context, msgID int64) (original *indicatorSubject, batches []indicatorSubject, err error)
	// notify publishes payload on the indicator channel.
	notify(ctx context.Context, payload []byte) error
}

type sqlIndicatorStore struct{ db *sql.DB }

// originalFromBatch returns the original message's participants from one of
// its add-to batch messages, for an original this host received only through
// add-to (msg.wire_message null): an add-to message repeats the original's
// from and to unchanged (SPEC §12), so those are the original's participants.
func (s sqlIndicatorStore) originalFromBatch(ctx context.Context, msgID int64) ([]FMsgAddress, bool, error) {
	var prepared []byte
	err := s.db.QueryRowContext(ctx, `SELECT wire_message FROM msg_add_to_batch
		WHERE msg_id = $1 AND sha256 IS NOT NULL AND wire_message IS NOT NULL
		ORDER BY id LIMIT 1`, msgID).Scan(&prepared)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	h, err := preparedHeader(prepared)
	if err != nil {
		return nil, false, err
	}
	return append([]FMsgAddress{h.From}, h.To...), true, nil
}

func (s sqlIndicatorStore) subjectByHash(ctx context.Context, hash []byte) (int64, []FMsgAddress, bool, error) {
	var id int64
	var prepared []byte
	err := s.db.QueryRowContext(ctx, `SELECT id, wire_message FROM msg WHERE sha256 = $1 AND time_sent IS NOT NULL`, hash).Scan(&id, &prepared)
	if err == nil {
		if len(prepared) == 0 {
			parts, ok, err := s.originalFromBatch(ctx, id)
			return id, parts, ok, err
		}
		h, err := preparedHeader(prepared)
		if err != nil {
			return 0, nil, false, err
		}
		return id, indicatorParticipants(h), true, nil
	}
	if err != sql.ErrNoRows {
		return 0, nil, false, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT msg_id, wire_message FROM msg_add_to_batch WHERE sha256 = $1 AND wire_message IS NOT NULL`, hash).Scan(&id, &prepared)
	if err == sql.ErrNoRows {
		return 0, nil, false, nil
	}
	if err != nil {
		return 0, nil, false, err
	}
	h, err := preparedHeader(prepared)
	if err != nil {
		return 0, nil, false, err
	}
	return id, indicatorParticipants(h), true, nil
}

func (s sqlIndicatorStore) subjectsOf(ctx context.Context, msgID int64) (*indicatorSubject, []indicatorSubject, error) {
	var hash, prepared []byte
	err := s.db.QueryRowContext(ctx, `SELECT sha256, wire_message FROM msg WHERE id = $1 AND time_sent IS NOT NULL`, msgID).Scan(&hash, &prepared)
	if err == sql.ErrNoRows {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}

	rows, err := s.db.QueryContext(ctx, `SELECT sha256, wire_message FROM msg_add_to_batch
		WHERE msg_id = $1 AND sha256 IS NOT NULL AND wire_message IS NOT NULL ORDER BY id`, msgID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var batches []indicatorSubject
	for rows.Next() {
		var bHash, bPrepared []byte
		if err := rows.Scan(&bHash, &bPrepared); err != nil {
			return nil, nil, err
		}
		h, err := preparedHeader(bPrepared)
		if err != nil {
			return nil, nil, err
		}
		batches = append(batches, indicatorSubject{Hash: bHash, Participants: indicatorParticipants(h)})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	var original *indicatorSubject
	if len(hash) == 32 {
		if len(prepared) > 0 {
			h, err := preparedHeader(prepared)
			if err != nil {
				return nil, nil, err
			}
			original = &indicatorSubject{Hash: hash, Participants: indicatorParticipants(h)}
		} else if parts, ok, err := s.originalFromBatch(ctx, msgID); err != nil {
			return nil, nil, err
		} else if ok {
			original = &indicatorSubject{Hash: hash, Participants: parts}
		}
	}
	return original, batches, nil
}

func (s sqlIndicatorStore) notify(ctx context.Context, payload []byte) error {
	_, err := s.db.ExecContext(ctx, `SELECT pg_notify($1, $2)`, indicatorChannel, string(payload))
	return err
}

// indicatorNotice is the indicator channel's JSON payload. ID is the local id
// of the subject message; From the indicating participant; an empty Emoji or a
// zero TTL clears the state.
type indicatorNotice struct {
	ID    int64  `json:"id"`
	From  string `json:"from"`
	Emoji string `json:"emoji"`
	TTL   int    `json:"ttl"`
}

// ---------------------------------------------------------------------------
// Rate limiting and caching

// tokenBucket refills at rate tokens per second up to burst.
type tokenBucket struct {
	tokens float64
	last   time.Time
}

func (b *tokenBucket) allow(now time.Time, rate, burst float64) bool {
	b.tokens = min(burst, b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sourceLimiter is the receiving host's per-source-address rate limit.
type sourceLimiter struct {
	mu        sync.Mutex
	rate      float64
	burst     float64
	max       int
	buckets   map[string]*tokenBucket
	lastPrune time.Time
}

func newSourceLimiter(rate, burst float64, max int) *sourceLimiter {
	return &sourceLimiter{rate: rate, burst: burst, max: max, buckets: map[string]*tokenBucket{}}
}

func (l *sourceLimiter) allow(src string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[src]
	if !ok {
		if len(l.buckets) >= l.max || now.Sub(l.lastPrune) >= 10*time.Second {
			l.prune(now)
		}
		if len(l.buckets) >= l.max {
			return false
		}
		b = &tokenBucket{tokens: l.burst, last: now}
		l.buckets[src] = b
	}
	return b.allow(now, l.rate, l.burst)
}

// prune forgets sources whose bucket has refilled: they are
// indistinguishable from a new source.
func (l *sourceLimiter) prune(now time.Time) {
	full := time.Duration(l.burst / l.rate * float64(time.Second))
	for k, b := range l.buckets {
		if now.Sub(b.last) >= full {
			delete(l.buckets, k)
		}
	}
	l.lastPrune = now
}

// windowLimiter allows at most n events per key in any sliding window, by
// remembering the time of each key's last n events.
type windowLimiter struct {
	mu     sync.Mutex
	n      int
	window time.Duration
	keys   map[string]*windowRing
}

type windowRing struct {
	times []time.Time
	next  int
}

func newWindowLimiter(n int, window time.Duration) *windowLimiter {
	return &windowLimiter{n: n, window: window, keys: map[string]*windowRing{}}
}

func (l *windowLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.keys[key]
	if !ok {
		r = &windowRing{times: make([]time.Time, 0, l.n)}
		l.keys[key] = r
	}
	if len(r.times) < l.n {
		r.times = append(r.times, now)
		return true
	}
	if now.Sub(r.times[r.next]) < l.window {
		return false
	}
	r.times[r.next] = now
	r.next = (r.next + 1) % l.n
	return true
}

// prune forgets keys with no event inside the window.
func (l *windowLimiter) prune(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, r := range l.keys {
		latest := r.times[(r.next+len(r.times)-1)%len(r.times)]
		if now.Sub(latest) >= l.window {
			delete(l.keys, k)
		}
	}
}

// ipCache caches fmsg.<domain> resolutions for indicators: positive results
// for ttl, failures for negTTL. The TCP exchange keeps resolving afresh.
type ipCache struct {
	mu      sync.Mutex
	lookup  func(domain string) ([]net.IP, error)
	now     func() time.Time
	ttl     time.Duration
	negTTL  time.Duration
	max     int
	entries map[string]ipCacheEntry
}

type ipCacheEntry struct {
	ips     []net.IP
	err     error
	expires time.Time
}

func newIPCache(lookup func(string) ([]net.IP, error), now func() time.Time) *ipCache {
	return &ipCache{lookup: lookup, now: now, ttl: indicatorDNSCacheTTL, negTTL: indicatorDNSNegativeTTL,
		max: indicatorDNSCacheMax, entries: map[string]ipCacheEntry{}}
}

func (c *ipCache) get(domain string) ([]net.IP, error) {
	domain = strings.ToLower(domain)
	c.mu.Lock()
	e, ok := c.entries[domain]
	c.mu.Unlock()
	now := c.now()
	if ok && now.Before(e.expires) {
		return e.ips, e.err
	}
	ips, err := c.lookup(domain)
	e = ipCacheEntry{ips: ips, err: err, expires: now.Add(c.ttl)}
	if err != nil {
		e.expires = now.Add(c.negTTL)
	}
	c.mu.Lock()
	if len(c.entries) >= c.max {
		for k, old := range c.entries {
			if !now.Before(old.expires) {
				delete(c.entries, k)
			}
		}
	}
	if len(c.entries) < c.max {
		c.entries[domain] = e
	}
	c.mu.Unlock()
	return ips, err
}

// ---------------------------------------------------------------------------
// Service

// indicatorService receives and sends FMSG-006 indicators.
type indicatorService struct {
	store       indicatorStore
	conn        net.PacketConn
	resolve     func(domain string) ([]net.IP, error) // fmsg.<domain>, cached
	now         func() time.Time
	isLocal     func(domain string) bool
	skipIPCheck bool
	remotePort  int

	recvLimit   *sourceLimiter
	domainLimit *windowLimiter
	recvSem     chan struct{}

	mu      sync.Mutex
	pending map[string]*indicatorPending // by subject hash and index
	rr      map[string]uint32            // per-domain counter for alternating addresses
}

// indicatorPending tracks one (subject, participant index): when its last
// non-clearing datagram went out and the latest coalesced change not yet sent.
type indicatorPending struct {
	lastSent time.Time
	latest   *indicatorDatagram
	domains  []string
}

func newIndicatorService(store indicatorStore, conn net.PacketConn, lookup func(string) ([]net.IP, error), now func() time.Time) *indicatorService {
	return &indicatorService{
		store:       store,
		conn:        conn,
		resolve:     newIPCache(lookup, now).get,
		now:         now,
		isLocal:     isIndicatorLocalDomain,
		skipIPCheck: SkipAuthorisedIPs,
		remotePort:  RemotePort,
		recvLimit:   newSourceLimiter(indicatorRecvRate, indicatorRecvBurst, indicatorRecvMaxSources),
		domainLimit: newWindowLimiter(indicatorHostRate, time.Second),
		recvSem:     make(chan struct{}, indicatorRecvWorkers),
		pending:     map[string]*indicatorPending{},
		rr:          map[string]uint32{},
	}
}

// Drop reasons, for tests; a dropped datagram is never answered or logged.
var (
	errIndicatorRateLimited = errors.New("indicator: source rate limited")
	errIndicatorUnknown     = errors.New("indicator: subject not found")
	errIndicatorIndex       = errors.New("indicator: index out of range")
	errIndicatorLocal       = errors.New("indicator: indicating participant is hosted here")
	errIndicatorSource      = errors.New("indicator: source address not authorised")
)

// receive runs the Receiving steps of FMSG-006 for one datagram from src,
// returning nil when the indicator was published to the client API, or why
// it was dropped.
func (s *indicatorService) receive(ctx context.Context, b []byte, src net.IP) error {
	if !s.recvLimit.allow(src.String(), s.now()) {
		return errIndicatorRateLimited
	}
	return s.verifyAndPublish(ctx, b, src)
}

// verifyAndPublish runs every Receiving step after the rate limit.
func (s *indicatorService) verifyAndPublish(ctx context.Context, b []byte, src net.IP) error {
	d, err := parseIndicatorDatagram(b)
	if err != nil {
		return err
	}
	msgID, parts, found, err := s.store.subjectByHash(ctx, d.Hash[:])
	if err != nil {
		return err
	}
	if !found {
		return errIndicatorUnknown
	}
	if int(d.Index) >= len(parts) {
		return errIndicatorIndex
	}
	from := parts[d.Index]
	if s.isLocal(from.Domain) {
		return errIndicatorLocal
	}
	if !s.skipIPCheck {
		ips, err := s.resolve(from.Domain)
		if err != nil || !slices.ContainsFunc(ips, src.Equal) {
			return errIndicatorSource
		}
	}
	n := indicatorNotice{ID: msgID, From: from.ToString(), Emoji: d.Data, TTL: int(min(d.TTL, indicatorTTLMax))}
	if d.isClear() {
		n.Emoji, n.TTL = "", 0
	}
	payload, err := json.Marshal(n)
	if err != nil {
		return err
	}
	return s.store.notify(ctx, payload)
}

// serve reads datagrams until the connection is closed, verifying each on a
// bounded set of workers; excess datagrams are dropped.
func (s *indicatorService) serve() {
	buf := make([]byte, indicatorMaxLen+1)
	for {
		n, addr, err := s.conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		ua, ok := addr.(*net.UDPAddr)
		if !ok || n > indicatorMaxLen {
			continue
		}
		if !s.recvLimit.allow(ua.IP.String(), s.now()) {
			continue
		}
		select {
		case s.recvSem <- struct{}{}:
		default:
			continue
		}
		b := bytes.Clone(buf[:n])
		go func() {
			defer func() { <-s.recvSem }()
			ctx, cancel := context.WithTimeout(context.Background(), indicatorQueryTimeout)
			defer cancel()
			_ = s.verifyAndPublish(ctx, b, ua.IP)
		}()
	}
}

// handleNotice sends a local participant's indicator, published by the
// client API on the indicator channel, to the subject's other participant
// domains. Notices for addresses this host does not host are ignored,
// including the ones this host publishes for received datagrams.
func (s *indicatorService) handleNotice(ctx context.Context, payload string) error {
	var n indicatorNotice
	if err := json.Unmarshal([]byte(payload), &n); err != nil {
		return err
	}
	from, err := parseAddress([]byte(n.From))
	if err != nil {
		return err
	}
	if !s.isLocal(from.Domain) {
		return nil
	}
	if n.ID <= 0 || n.TTL < 0 || !isIndicatorEmoji(n.Emoji) {
		return fmt.Errorf("invalid indicator notice")
	}
	data, ttl := n.Emoji, min(n.TTL, indicatorTTLMax)
	if data == "" || ttl == 0 {
		data, ttl = "", 0
	}
	original, batches, err := s.store.subjectsOf(ctx, n.ID)
	if err != nil {
		return err
	}
	targets := selectIndicatorTargets(*from, original, batches, s.isLocal)

	// group receiving domains by (subject, index): the interval and
	// coalescing apply to the participant on the subject, whoever receives
	type group struct {
		d       indicatorDatagram
		domains []string
	}
	var order []string
	groups := map[string]*group{}
	for _, t := range targets {
		k := indicatorKey(t.Hash, t.Index)
		g, ok := groups[k]
		if !ok {
			g = &group{d: indicatorDatagram{Hash: t.Hash, Index: t.Index, TTL: uint8(ttl), Data: data}}
			groups[k] = g
			order = append(order, k)
		}
		g.domains = append(g.domains, t.Domain)
	}
	for _, k := range order {
		s.offer(k, groups[k].d, groups[k].domains)
	}
	return nil
}

func indicatorKey(hash [32]byte, index uint16) string {
	return fmt.Sprintf("%s/%d", hex.EncodeToString(hash[:]), index)
}

// offer sends d to domains now, or holds it as the latest change for its
// (subject, index) until INDICATOR_MIN_INTERVAL has passed since the last
// datagram. A clearing datagram is sent at once and supersedes anything held.
func (s *indicatorService) offer(key string, d indicatorDatagram, domains []string) {
	now := s.now()
	s.mu.Lock()
	p, ok := s.pending[key]
	if !ok {
		p = &indicatorPending{}
		s.pending[key] = p
	}
	send := true
	if d.isClear() {
		p.latest, p.domains = nil, nil
	} else if !p.lastSent.IsZero() && now.Sub(p.lastSent) < indicatorMinInterval {
		dd := d
		p.latest, p.domains = &dd, domains
		send = false
	} else {
		p.lastSent = now
		p.latest, p.domains = nil, nil
	}
	s.mu.Unlock()
	if send {
		s.sendAll(d, domains)
	}
}

// flush sends every held change whose interval has passed and forgets idle
// state. It runs on a short ticker.
func (s *indicatorService) flush() {
	now := s.now()
	type due struct {
		d       indicatorDatagram
		domains []string
	}
	var out []due
	s.mu.Lock()
	for k, p := range s.pending {
		if p.latest != nil {
			if now.Sub(p.lastSent) >= indicatorMinInterval {
				out = append(out, due{*p.latest, p.domains})
				p.lastSent = now
				p.latest, p.domains = nil, nil
			}
		} else if now.Sub(p.lastSent) >= indicatorMinInterval {
			delete(s.pending, k)
		}
	}
	s.mu.Unlock()
	s.domainLimit.prune(now)
	for _, o := range out {
		s.sendAll(o.d, o.domains)
	}
}

func (s *indicatorService) sendAll(d indicatorDatagram, domains []string) {
	b := d.marshal()
	for _, domain := range domains {
		s.sendTo(domain, b)
	}
}

// sendTo sends one datagram to one address of fmsg.<domain>, alternating
// between addresses on successive datagrams, within the domain's rate limit.
// Errors are ignored: indicators are best effort.
func (s *indicatorService) sendTo(domain string, b []byte) {
	if !s.domainLimit.allow(domain, s.now()) {
		return
	}
	ips, err := s.resolve(domain)
	if err != nil || len(ips) == 0 {
		return
	}
	s.mu.Lock()
	i := s.rr[domain]
	s.rr[domain] = i + 1
	s.mu.Unlock()
	ip := ips[int(i%uint32(len(ips)))]
	_, _ = s.conn.WriteTo(b, &net.UDPAddr{IP: ip, Port: s.remotePort})
}

// listen LISTENs on the indicator channel and sends each local participant's
// indicator, in the order published so the latest state wins.
func (s *indicatorService) listen(dsn string) {
	listener := pq.NewListener(dsn, 10*time.Second, time.Minute, func(ev pq.ListenerEventType, err error) {
		if err != nil {
			log.Printf("ERROR: indicators: pg listener: %s", err)
		}
	})
	if err := listener.Listen(indicatorChannel); err != nil {
		log.Printf("ERROR: indicators: could not LISTEN on %s: %s", indicatorChannel, err)
		return
	}
	defer listener.Close()
	for {
		select {
		case n := <-listener.Notify:
			if n == nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), indicatorQueryTimeout)
			if err := s.handleNotice(ctx, n.Extra); err != nil {
				log.Printf("WARN: indicators: notice %q: %s", n.Extra, err)
			}
			cancel()
		case <-time.After(32 * time.Second):
			if err := listener.Ping(); err != nil {
				log.Printf("ERROR: indicators: pg listener ping: %s", err)
			}
		}
	}
}

func (s *indicatorService) runFlusher() {
	t := time.NewTicker(indicatorFlushInterval)
	defer t.Stop()
	for range t.C {
		s.flush()
	}
}

// startIndicators listens for indicator datagrams on UDP at the TCP listen
// address and port, and sends local participants' indicators from the same
// socket, so they leave from an address in this host's fmsg.<domain> set.
// Failing to start is logged; message exchange is unaffected.
func startIndicators(listenAddress string, port int) {
	if !indicatorsEnabled() {
		log.Println("INFO: indicators: disabled (FMSG_INDICATORS)")
		return
	}
	addr := net.JoinHostPort(listenAddress, fmt.Sprintf("%d", port))
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		log.Printf("ERROR: indicators: resolving %s: %s", addr, err)
		return
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		log.Printf("ERROR: indicators: listening on UDP %s: %s", addr, err)
		return
	}
	db, err := sql.Open("postgres", "")
	if err != nil {
		log.Printf("ERROR: indicators: opening database: %s", err)
		conn.Close()
		return
	}
	db.SetMaxOpenConns(indicatorRecvWorkers + 2)
	s := newIndicatorService(sqlIndicatorStore{db}, conn, lookupAuthorisedIPs, time.Now)
	go s.serve()
	go s.listen("")
	go s.runFlusher()
	log.Printf("INFO: indicators: ready on UDP %s", addr)
}
