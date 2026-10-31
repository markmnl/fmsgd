package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/markmnl/fmsgd/pkg/fmsg"
)

const writingHand = "✍️" // E2 9C 8D EF B8 8F

func mustAddr(t *testing.T, s string) FMsgAddress {
	t.Helper()
	a, err := parseAddress([]byte(s))
	if err != nil {
		t.Fatalf("parseAddress(%q): %v", s, err)
	}
	return *a
}

func mustAddrs(t *testing.T, ss ...string) []FMsgAddress {
	t.Helper()
	var out []FMsgAddress
	for _, s := range ss {
		out = append(out, mustAddr(t, s))
	}
	return out
}

func randHash(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func hash32(b []byte) (h [32]byte) {
	copy(h[:], b)
	return
}

func addrStrings(as []FMsgAddress) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.ToString())
	}
	return out
}

// withIndicatorDomain sets this host's domain for the test.
func withIndicatorDomain(t *testing.T, d string) {
	t.Helper()
	orig := Domain
	Domain = d
	t.Cleanup(func() { Domain = orig })
}

// ---------------------------------------------------------------------------
// Datagram

func TestIndicatorDatagramRoundTrip(t *testing.T) {
	for _, d := range []indicatorDatagram{
		{Hash: hash32(randHash(t)), Index: 1, TTL: 6, Data: writingHand},
		{Hash: hash32(randHash(t)), Index: 0x1234, TTL: 120, Data: "🏴󠁧󠁢󠁳󠁣󠁴󠁿"},
		{Hash: hash32(randHash(t)), Index: 0, TTL: 0, Data: ""},
	} {
		b := d.marshal()
		got, err := parseIndicatorDatagram(b)
		if err != nil {
			t.Fatalf("parse(%x): %v", b, err)
		}
		if got != d {
			t.Fatalf("round trip = %+v, want %+v", got, d)
		}
	}
}

func TestIndicatorDatagramLayout(t *testing.T) {
	d := indicatorDatagram{Index: 0x0201, TTL: 6, Data: writingHand}
	for i := range d.Hash {
		d.Hash[i] = byte(i)
	}
	b := d.marshal()
	want := []byte{1}
	want = append(want, d.Hash[:]...)
	want = append(want, 0x01, 0x02, 6, 6, 0xE2, 0x9C, 0x8D, 0xEF, 0xB8, 0x8F)
	if !bytes.Equal(b, want) {
		t.Fatalf("marshal = %x, want %x", b, want)
	}
}

func TestIndicatorDatagramInvalid(t *testing.T) {
	valid := indicatorDatagram{Hash: hash32(randHash(t)), Index: 1, TTL: 6, Data: writingHand}.marshal()
	with := func(f func(b []byte) []byte) []byte { return f(bytes.Clone(valid)) }
	withData := func(data string) []byte {
		b := bytes.Clone(valid[:indicatorFixedLen])
		b[36] = byte(len(data))
		return append(b, data...)
	}
	long := strings.Repeat("‍👍", 10) // > 64 bytes
	cases := map[string][]byte{
		"empty":             nil,
		"version only":      {1},
		"version 0":         with(func(b []byte) []byte { b[0] = 0; return b }),
		"version 2":         with(func(b []byte) []byte { b[0] = 2; return b }),
		"truncated header":  valid[:indicatorFixedLen-1],
		"truncated data":    valid[:len(valid)-1],
		"trailing byte":     append(bytes.Clone(valid), 0),
		"length > 64":       withData(long),
		"length 65 claimed": with(func(b []byte) []byte { b[36] = 65; return append(b[:indicatorFixedLen], make([]byte, 65)...) }),
		"not emoji":         withData("a"),
		"two emoji":         withData("👍👍"),
		"emoji and space":   withData("👍 "),
		"invalid utf-8":     withData("\xff"),
		"text default":      withData("❤"),
	}
	if len(long) <= indicatorDataMax {
		t.Fatalf("test data %d bytes is not over the bound", len(long))
	}
	for name, b := range cases {
		if _, err := parseIndicatorDatagram(b); err == nil {
			t.Errorf("%s: parse(%x) accepted", name, b)
		}
	}
}

// ---------------------------------------------------------------------------
// Participants and subject selection

func TestIndicatorParticipants(t *testing.T) {
	h := &FMsgHeader{From: mustAddr(t, "@alice@a.test"), To: mustAddrs(t, "@bob@b.test", "@carol@c.test")}
	if got := addrStrings(indicatorParticipants(h)); strings.Join(got, ",") != "@alice@a.test,@bob@b.test,@carol@c.test" {
		t.Fatalf("original participants = %v", got)
	}
	bob := mustAddr(t, "@bob@b.test")
	h.Flags = FlagHasPid | FlagHasAddTo
	h.AddToFrom = &bob
	h.AddTo = mustAddrs(t, "@erin@e.test", "@carol@c.test")
	want := "@alice@a.test,@bob@b.test,@carol@c.test,@bob@b.test,@erin@e.test,@carol@c.test"
	if got := addrStrings(indicatorParticipants(h)); strings.Join(got, ",") != want {
		t.Fatalf("batch participants = %v, want %s", got, want)
	}
	// add to from defaults to from, as on the wire
	h.AddToFrom = nil
	if got := indicatorParticipants(h); got[3].ToString() != "@alice@a.test" {
		t.Fatalf("add to from without AddToFrom = %s", got[3].ToString())
	}
}

func TestPreparedHeaderParticipants(t *testing.T) {
	bob := mustAddr(t, "@bob@b.test")
	h := &FMsgHeader{Version: 1, Flags: FlagHasPid | FlagHasAddTo, Pid: randHash(t), From: mustAddr(t, "@alice@a.test"),
		To: mustAddrs(t, "@bob@b.test"), AddToFrom: &bob, AddTo: mustAddrs(t, "@erin@e.test"), Timestamp: 1, Type: "text/plain"}
	data, err := fmsg.MarshalPrepared(h)
	if err != nil {
		t.Fatal(err)
	}
	got, err := preparedHeader(data)
	if err != nil {
		t.Fatal(err)
	}
	if s := strings.Join(addrStrings(indicatorParticipants(got)), ","); s != "@alice@a.test,@bob@b.test,@bob@b.test,@erin@e.test" {
		t.Fatalf("participants = %s", s)
	}
	if _, err := preparedHeader([]byte(`{"Version":2,"Header":{}}`)); err == nil {
		t.Fatal("unsupported prepared version accepted")
	}
}

// testThread is a message from @alice@a.test to bob, carol and dave
// (dave hosted here), with two add-to batches:
//
//	batch 1: bob adds @erin@e.test
//	batch 2: dave adds @frank@here.test and @dave@here.test again
type testThread struct {
	original indicatorSubject
	batches  []indicatorSubject
	headers  []*FMsgHeader // original, batch 1, batch 2
}

func newTestThread(t *testing.T) testThread {
	t.Helper()
	orig := &FMsgHeader{Version: 1, From: mustAddr(t, "@alice@a.test"),
		To: mustAddrs(t, "@bob@b.test", "@carol@c.test", "@dave@here.test"), Timestamp: 1000, Topic: "hi", Type: "text/plain"}
	bob, dave := mustAddr(t, "@bob@b.test"), mustAddr(t, "@dave@here.test")
	b1 := orig.Clone()
	b1.Flags, b1.Pid, b1.Topic, b1.Timestamp = FlagHasPid|FlagHasAddTo, randHash(t), "", 1001
	b1.AddToFrom, b1.AddTo = &bob, mustAddrs(t, "@erin@e.test")
	b2 := orig.Clone()
	b2.Flags, b2.Pid, b2.Topic, b2.Timestamp = FlagHasPid|FlagHasAddTo, b1.Pid, "", 1002
	b2.AddToFrom, b2.AddTo = &dave, mustAddrs(t, "@frank@here.test", "@dave@here.test")
	return testThread{
		original: indicatorSubject{Hash: randHash(t), Participants: indicatorParticipants(orig)},
		batches: []indicatorSubject{
			{Hash: randHash(t), Participants: indicatorParticipants(b1)},
			{Hash: randHash(t), Participants: indicatorParticipants(b2)},
		},
		headers: []*FMsgHeader{orig, b1, b2},
	}
}

func targetsByDomain(ts []indicatorTarget) map[string]indicatorTarget {
	m := map[string]indicatorTarget{}
	for _, t := range ts {
		m[t.Domain] = t
	}
	return m
}

func TestSelectIndicatorTargets(t *testing.T) {
	withIndicatorDomain(t, "here.test")
	th := newTestThread(t)
	origHash, b1Hash, b2Hash := hash32(th.original.Hash), hash32(th.batches[0].Hash), hash32(th.batches[1].Hash)

	t.Run("original participant", func(t *testing.T) {
		got := targetsByDomain(selectIndicatorTargets(mustAddr(t, "@dave@here.test"), &th.original, th.batches, isIndicatorLocalDomain))
		want := map[string]indicatorTarget{
			"a.test": {"a.test", origHash, 3},
			"b.test": {"b.test", origHash, 3},
			"c.test": {"c.test", origHash, 3},
			// erin holds only batch 1, which includes dave at index 3
			"e.test": {"e.test", b1Hash, 3},
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("targets = %v\nwant %v", got, want)
		}
	})

	t.Run("added participant", func(t *testing.T) {
		// frank is only a participant of batch 2: a, b and c hold it; e does not
		got := targetsByDomain(selectIndicatorTargets(mustAddr(t, "@frank@here.test"), &th.original, th.batches, isIndicatorLocalDomain))
		want := map[string]indicatorTarget{
			"a.test": {"a.test", b2Hash, 5},
			"b.test": {"b.test", b2Hash, 5},
			"c.test": {"c.test", b2Hash, 5},
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("targets = %v\nwant %v", got, want)
		}
	})

	t.Run("lowest index", func(t *testing.T) {
		// dave is at 3, 4 and 6 in batch 2; the lowest is used
		got := targetsByDomain(selectIndicatorTargets(mustAddr(t, "@dave@here.test"), nil, th.batches[1:], isIndicatorLocalDomain))
		if g := got["a.test"]; g.Hash != b2Hash || g.Index != 3 {
			t.Fatalf("a.test target = %+v, want batch 2 index 3", g)
		}
	})

	t.Run("case insensitive", func(t *testing.T) {
		got := selectIndicatorTargets(mustAddr(t, "@Dave@HERE.test"), &th.original, nil, isIndicatorLocalDomain)
		if len(got) != 3 || got[0].Index != 3 {
			t.Fatalf("targets = %v", got)
		}
	})

	t.Run("never own domain", func(t *testing.T) {
		for _, tg := range selectIndicatorTargets(mustAddr(t, "@dave@here.test"), &th.original, th.batches, isIndicatorLocalDomain) {
			if tg.Domain == "here.test" {
				t.Fatal("targeted own domain")
			}
		}
	})

	t.Run("not a participant", func(t *testing.T) {
		if got := selectIndicatorTargets(mustAddr(t, "@zed@here.test"), &th.original, th.batches, isIndicatorLocalDomain); len(got) != 0 {
			t.Fatalf("targets = %v", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Rate limiting and caching

func TestSourceLimiter(t *testing.T) {
	l := newSourceLimiter(50, 100, 3)
	now := time.Unix(1000, 0)
	for i := 0; i < 100; i++ {
		if !l.allow("192.0.2.1", now) {
			t.Fatalf("datagram %d dropped within burst", i)
		}
	}
	if l.allow("192.0.2.1", now) {
		t.Fatal("datagram over burst allowed")
	}
	if !l.allow("192.0.2.2", now) {
		t.Fatal("other source limited")
	}
	// 50/s refill: a second later, 50 more
	now = now.Add(time.Second)
	for i := 0; i < 50; i++ {
		if !l.allow("192.0.2.1", now) {
			t.Fatalf("refilled datagram %d dropped", i)
		}
	}
	if l.allow("192.0.2.1", now) {
		t.Fatal("datagram over refill allowed")
	}
	// table full of active sources: a new source is dropped
	l.allow("192.0.2.3", now)
	if l.allow("192.0.2.4", now) {
		t.Fatal("new source allowed with table full")
	}
	// idle sources are pruned once their bucket would be full
	if !l.allow("192.0.2.4", now.Add(3*time.Second)) {
		t.Fatal("new source dropped after idle sources expired")
	}
	if len(l.buckets) != 1 {
		t.Fatalf("buckets = %d, want 1 after prune", len(l.buckets))
	}
}

func TestWindowLimiter(t *testing.T) {
	l := newWindowLimiter(20, time.Second)
	now := time.Unix(1000, 0)
	for i := 0; i < 20; i++ {
		if !l.allow("b.test", now.Add(time.Duration(i)*10*time.Millisecond)) {
			t.Fatalf("event %d dropped", i)
		}
	}
	if l.allow("b.test", now.Add(500*time.Millisecond)) {
		t.Fatal("21st event in a second allowed")
	}
	if !l.allow("c.test", now) {
		t.Fatal("other key limited")
	}
	if !l.allow("b.test", now.Add(time.Second)) {
		t.Fatal("event after window dropped")
	}
	if l.allow("b.test", now.Add(time.Second)) {
		t.Fatal("second event after window allowed while 19 in window")
	}
	l.prune(now.Add(3 * time.Second))
	if len(l.keys) != 0 {
		t.Fatalf("keys = %d after prune", len(l.keys))
	}
}

func TestIPCache(t *testing.T) {
	now := time.Unix(1000, 0)
	calls := 0
	fail := false
	c := newIPCache(func(d string) ([]net.IP, error) {
		calls++
		if fail {
			return nil, errors.New("nxdomain")
		}
		return []net.IP{net.ParseIP("192.0.2.1")}, nil
	}, func() time.Time { return now })
	c.get("b.test")
	c.get("B.test")
	if calls != 1 {
		t.Fatalf("lookups = %d, want 1 (cached)", calls)
	}
	now = now.Add(indicatorDNSCacheTTL)
	fail = true
	if _, err := c.get("b.test"); err == nil || calls != 2 {
		t.Fatalf("expired entry not looked up again: calls=%d err=%v", calls, err)
	}
	if _, err := c.get("b.test"); err == nil || calls != 2 {
		t.Fatal("failure not cached")
	}
	now = now.Add(indicatorDNSNegativeTTL)
	fail = false
	if ips, err := c.get("b.test"); err != nil || len(ips) != 1 || calls != 3 {
		t.Fatalf("negative entry not expired: calls=%d err=%v", calls, err)
	}
}

// ---------------------------------------------------------------------------
// Sending

type fakeIndicatorStore struct {
	original *indicatorSubject
	batches  []indicatorSubject
}

func (s *fakeIndicatorStore) subjectByHash(context.Context, []byte) (int64, []FMsgAddress, bool, error) {
	return 0, nil, false, nil
}
func (s *fakeIndicatorStore) subjectsOf(context.Context, int64) (*indicatorSubject, []indicatorSubject, error) {
	return s.original, s.batches, nil
}
func (s *fakeIndicatorStore) notify(context.Context, []byte) error { return nil }

type sentDatagram struct {
	to string
	d  indicatorDatagram
}

// fakePacketConn records datagrams written.
type fakePacketConn struct {
	mu   sync.Mutex
	sent []sentDatagram
}

func (c *fakePacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	d, err := parseIndicatorDatagram(b)
	if err != nil {
		panic(err)
	}
	c.mu.Lock()
	c.sent = append(c.sent, sentDatagram{addr.String(), d})
	c.mu.Unlock()
	return len(b), nil
}
func (c *fakePacketConn) take() []sentDatagram {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.sent
	c.sent = nil
	return s
}
func (c *fakePacketConn) ReadFrom([]byte) (int, net.Addr, error) { return 0, nil, net.ErrClosed }
func (c *fakePacketConn) Close() error                           { return nil }
func (c *fakePacketConn) LocalAddr() net.Addr                    { return &net.UDPAddr{} }
func (c *fakePacketConn) SetDeadline(time.Time) error            { return nil }
func (c *fakePacketConn) SetReadDeadline(time.Time) error        { return nil }
func (c *fakePacketConn) SetWriteDeadline(time.Time) error       { return nil }

// testResolver maps a.test → 192.0.2.1, b.test → 192.0.2.2 and so on;
// e.test has two addresses.
func testResolver(domain string) ([]net.IP, error) {
	switch domain {
	case "a.test":
		return []net.IP{net.ParseIP("192.0.2.1")}, nil
	case "b.test":
		return []net.IP{net.ParseIP("192.0.2.2")}, nil
	case "c.test":
		return []net.IP{net.ParseIP("192.0.2.3")}, nil
	case "e.test":
		return []net.IP{net.ParseIP("192.0.2.5"), net.ParseIP("2001:db8::5")}, nil
	case "here.test":
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	return nil, fmt.Errorf("no fmsg.%s", domain)
}

type testClock struct{ t time.Time }

func (c *testClock) now() time.Time      { return c.t }
func (c *testClock) add(d time.Duration) { c.t = c.t.Add(d) }
func newTestClock() *testClock           { return &testClock{time.Unix(1_000_000, 0)} }
func noticeJSON(from, e string, ttl int) string {
	b, _ := json.Marshal(indicatorNotice{ID: 7, From: from, Emoji: e, TTL: ttl})
	return string(b)
}

func newSendTestService(t *testing.T, store indicatorStore) (*indicatorService, *fakePacketConn, *testClock) {
	t.Helper()
	withIndicatorDomain(t, "here.test")
	clock := newTestClock()
	conn := &fakePacketConn{}
	s := newIndicatorService(store, conn, testResolver, clock.now)
	s.remotePort = 4930
	return s, conn, clock
}

func TestIndicatorSendFanOut(t *testing.T) {
	th := newTestThread(t)
	s, conn, _ := newSendTestService(t, &fakeIndicatorStore{&th.original, th.batches})
	if err := s.handleNotice(context.Background(), noticeJSON("@dave@here.test", writingHand, 6)); err != nil {
		t.Fatal(err)
	}
	got := map[string]indicatorDatagram{}
	for _, sd := range conn.take() {
		got[sd.to] = sd.d
	}
	want := map[string]indicatorDatagram{
		"192.0.2.1:4930": {hash32(th.original.Hash), 3, 6, writingHand},
		"192.0.2.2:4930": {hash32(th.original.Hash), 3, 6, writingHand},
		"192.0.2.3:4930": {hash32(th.original.Hash), 3, 6, writingHand},
		"192.0.2.5:4930": {hash32(th.batches[0].Hash), 3, 6, writingHand},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("sent %v\nwant %v", got, want)
	}
}

func TestIndicatorSendNormalises(t *testing.T) {
	th := newTestThread(t)
	s, conn, _ := newSendTestService(t, &fakeIndicatorStore{&th.original, nil})
	_ = s.handleNotice(context.Background(), noticeJSON("@dave@here.test", writingHand, 500))
	for _, sd := range conn.take() {
		if sd.d.TTL != indicatorTTLMax {
			t.Fatalf("ttl %d not clamped", sd.d.TTL)
		}
	}
	// ttl 0 with data is a clear: sent with empty data
	_ = s.handleNotice(context.Background(), noticeJSON("@dave@here.test", writingHand, 0))
	sent := conn.take()
	if len(sent) != 3 || sent[0].d.Data != "" || sent[0].d.TTL != 0 {
		t.Fatalf("clear sent as %+v", sent)
	}
}

func TestIndicatorSendIgnores(t *testing.T) {
	th := newTestThread(t)
	s, conn, _ := newSendTestService(t, &fakeIndicatorStore{&th.original, th.batches})
	for name, payload := range map[string]string{
		"remote address": noticeJSON("@bob@b.test", writingHand, 6), // e.g. one this host received
		"not emoji":      noticeJSON("@dave@here.test", "hi", 6),
		"negative ttl":   noticeJSON("@dave@here.test", writingHand, -1),
		"bad json":       "{",
	} {
		_ = s.handleNotice(context.Background(), payload)
		if sent := conn.take(); len(sent) != 0 {
			t.Errorf("%s: sent %v", name, sent)
		}
	}
}

func TestIndicatorSendMinIntervalCoalesces(t *testing.T) {
	th := newTestThread(t)
	s, conn, clock := newSendTestService(t, &fakeIndicatorStore{&th.original, nil})
	notice := func(e string) {
		t.Helper()
		if err := s.handleNotice(context.Background(), noticeJSON("@dave@here.test", e, 6)); err != nil {
			t.Fatal(err)
		}
	}
	dataSent := func() []string {
		var out []string
		for _, sd := range conn.take() {
			out = append(out, sd.d.Data)
		}
		return out
	}

	notice(writingHand)
	if got := dataSent(); len(got) != 3 {
		t.Fatalf("first indicator sent %d datagrams, want 3", len(got))
	}
	clock.add(500 * time.Millisecond)
	notice("⏳")
	clock.add(500 * time.Millisecond)
	notice("📎")
	if got := dataSent(); len(got) != 0 {
		t.Fatalf("sent within interval: %v", got)
	}
	clock.add(900 * time.Millisecond)
	s.flush()
	if got := dataSent(); len(got) != 0 {
		t.Fatalf("flushed before interval: %v", got)
	}
	clock.add(100 * time.Millisecond) // 2s since the first
	s.flush()
	if got := dataSent(); strings.Join(got, ",") != "📎,📎,📎" {
		t.Fatalf("flushed %v, want only the latest to each domain", got)
	}
	s.flush()
	if got := dataSent(); len(got) != 0 {
		t.Fatalf("flushed twice: %v", got)
	}
	// the flush restarted the interval
	clock.add(time.Second)
	notice(writingHand)
	if got := dataSent(); len(got) != 0 {
		t.Fatalf("sent within interval of flush: %v", got)
	}
	clock.add(time.Second)
	s.flush()
	if got := dataSent(); len(got) != 3 {
		t.Fatalf("flushed %v", got)
	}
	// idle state is forgotten
	clock.add(3 * time.Second)
	s.flush()
	if len(s.pending) != 0 {
		t.Fatalf("pending = %d after idle", len(s.pending))
	}
}

func TestIndicatorSendClearExempt(t *testing.T) {
	th := newTestThread(t)
	s, conn, clock := newSendTestService(t, &fakeIndicatorStore{&th.original, nil})
	_ = s.handleNotice(context.Background(), noticeJSON("@dave@here.test", writingHand, 6))
	conn.take()
	clock.add(100 * time.Millisecond)
	_ = s.handleNotice(context.Background(), noticeJSON("@dave@here.test", "👀", 30)) // held
	clock.add(100 * time.Millisecond)
	_ = s.handleNotice(context.Background(), noticeJSON("@dave@here.test", "", 0))
	sent := conn.take()
	if len(sent) != 3 || sent[0].d.Data != "" {
		t.Fatalf("clear not sent at once: %+v", sent)
	}
	clock.add(2 * time.Second)
	s.flush()
	if sent := conn.take(); len(sent) != 0 {
		t.Fatalf("held change sent after clear superseded it: %+v", sent)
	}
}

func TestIndicatorSendHostRate(t *testing.T) {
	s, conn, clock := newSendTestService(t, &fakeIndicatorStore{})
	for i := 0; i < 25; i++ {
		d := indicatorDatagram{Hash: hash32(randHash(t)), Index: 1, TTL: 6, Data: writingHand}
		s.offer(indicatorKey(d.Hash, d.Index), d, []string{"b.test", "c.test"})
	}
	perDest := map[string]int{}
	for _, sd := range conn.take() {
		perDest[sd.to]++
	}
	if perDest["192.0.2.2:4930"] != indicatorHostRate || perDest["192.0.2.3:4930"] != indicatorHostRate {
		t.Fatalf("sent per destination %v, want %d each", perDest, indicatorHostRate)
	}
	// clears are subject to the per-domain limit too
	d := indicatorDatagram{Hash: hash32(randHash(t)), Index: 1}
	s.offer(indicatorKey(d.Hash, d.Index), d, []string{"b.test"})
	if sent := conn.take(); len(sent) != 0 {
		t.Fatalf("over-rate clear sent: %v", sent)
	}
	clock.add(time.Second)
	s.offer(indicatorKey(d.Hash, d.Index), d, []string{"b.test"})
	if sent := conn.take(); len(sent) != 1 {
		t.Fatalf("sent %d after a second, want 1", len(sent))
	}
}

func TestIndicatorSendAlternatesAddresses(t *testing.T) {
	s, conn, _ := newSendTestService(t, &fakeIndicatorStore{})
	for i := 0; i < 4; i++ {
		d := indicatorDatagram{Hash: hash32(randHash(t)), Index: 1, TTL: 6, Data: writingHand}
		s.offer(indicatorKey(d.Hash, d.Index), d, []string{"e.test"})
	}
	var to []string
	for _, sd := range conn.take() {
		to = append(to, sd.to)
	}
	if strings.Join(to, ",") != "192.0.2.5:4930,[2001:db8::5]:4930,192.0.2.5:4930,[2001:db8::5]:4930" {
		t.Fatalf("sent to %v", to)
	}
}

func TestIndicatorSendLoopback(t *testing.T) {
	th := newTestThread(t)
	recv, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer recv.Close()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	withIndicatorDomain(t, "here.test")
	s := newIndicatorService(&fakeIndicatorStore{&th.original, nil}, conn, func(string) ([]net.IP, error) {
		return []net.IP{net.IPv4(127, 0, 0, 1)}, nil
	}, time.Now)
	s.remotePort = recv.LocalAddr().(*net.UDPAddr).Port
	if err := s.handleNotice(context.Background(), noticeJSON("@dave@here.test", writingHand, 6)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512)
	_ = recv.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, from, err := recv.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	if from.Port != conn.LocalAddr().(*net.UDPAddr).Port {
		t.Fatalf("datagram from %s, want the listening socket %s", from, conn.LocalAddr())
	}
	d, err := parseIndicatorDatagram(buf[:n])
	if err != nil {
		t.Fatal(err)
	}
	if d != (indicatorDatagram{hash32(th.original.Hash), 3, 6, writingHand}) {
		t.Fatalf("received %+v", d)
	}
}

// ---------------------------------------------------------------------------
// Receiving (PostgreSQL)

// testIndicatorDB returns a database with dd.sql applied in a fresh schema,
// and its DSN; set FMSG_TEST_DATABASE_URL to run.
func testIndicatorDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("FMSG_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set FMSG_TEST_DATABASE_URL to test indicators against PostgreSQL")
	}
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("indicators_%d", time.Now().UnixNano())
	if _, err = admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE"); admin.Close() })
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	dd, err := os.ReadFile("../../dd.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(dd)); err != nil {
		t.Fatal(err)
	}
	return db, u.String()
}

// storeTestThread stores the thread's original and batches; withOriginalWire
// false stores the original as received only through add-to.
func storeTestThread(t *testing.T, db *sql.DB, th testThread, withOriginalWire bool) int64 {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var wire any
	if withOriginalWire {
		b, err := fmsg.MarshalPrepared(th.headers[0])
		if err != nil {
			t.Fatal(err)
		}
		wire = string(b)
	}
	var id int64
	if err := tx.QueryRow(`INSERT INTO msg(version,time_sent,from_addr,topic,type,sha256,size,filepath,wire_message)
		VALUES(1,1000,'@alice@a.test','hi','text/plain',$1,0,'/dev/null',$2) RETURNING id`, th.original.Hash, wire).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for i, b := range th.batches {
		h := th.headers[i+1]
		data, err := fmsg.MarshalPrepared(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO msg_add_to_batch(msg_id,add_to_from,time_added,sha256,wire_message) VALUES($1,$2,$3,$4,$5)`,
			id, h.AddToFrom.ToString(), h.Timestamp, b.Hash, string(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return id
}

func listenIndicators(t *testing.T, dsn string) *pq.Listener {
	t.Helper()
	l := pq.NewListener(dsn, time.Second, time.Second, nil)
	if err := l.Listen(indicatorChannel); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func nextNotice(t *testing.T, l *pq.Listener, wait time.Duration) *indicatorNotice {
	t.Helper()
	select {
	case n := <-l.Notify:
		if n == nil {
			t.Fatal("listener reconnected")
		}
		var got indicatorNotice
		if err := json.Unmarshal([]byte(n.Extra), &got); err != nil {
			t.Fatal(err)
		}
		return &got
	case <-time.After(wait):
		return nil
	}
}

func TestIndicatorReceive(t *testing.T) {
	db, dsn := testIndicatorDB(t)
	withIndicatorDomain(t, "here.test")
	th := newTestThread(t)
	msgID := storeTestThread(t, db, th, true)
	l := listenIndicators(t, dsn)

	clock := newTestClock()
	s := newIndicatorService(sqlIndicatorStore{db}, nil, testResolver, clock.now)
	s.skipIPCheck = false
	ctx := context.Background()
	dg := func(hash []byte, index uint16, ttl uint8, data string) []byte {
		return indicatorDatagram{Hash: hash32(hash), Index: index, TTL: ttl, Data: data}.marshal()
	}
	bIP, eIP := net.ParseIP("192.0.2.2"), net.ParseIP("192.0.2.5")

	// dropped: none of these may publish
	for name, c := range map[string]struct {
		b    []byte
		src  net.IP
		want error
	}{
		"invalid":            {[]byte{1, 2, 3}, bIP, errIndicatorInvalid},
		"unknown hash":       {dg(randHash(t), 1, 6, writingHand), bIP, errIndicatorUnknown},
		"index out of range": {dg(th.original.Hash, 4, 6, writingHand), bIP, errIndicatorIndex},
		"batch index range":  {dg(th.batches[0].Hash, 6, 6, writingHand), eIP, errIndicatorIndex},
		"local participant":  {dg(th.original.Hash, 3, 6, writingHand), net.ParseIP("127.0.0.1"), errIndicatorLocal},
		"wrong source":       {dg(th.original.Hash, 1, 6, writingHand), net.ParseIP("192.0.2.3"), errIndicatorSource},
	} {
		if err := s.receive(ctx, c.b, c.src); !errors.Is(err, c.want) {
			t.Errorf("%s: receive = %v, want %v", name, err, c.want)
		}
	}
	if n := nextNotice(t, l, 300*time.Millisecond); n != nil {
		t.Fatalf("dropped datagram published %+v", n)
	}

	for name, c := range map[string]struct {
		b    []byte
		src  net.IP
		want indicatorNotice
	}{
		"original":      {dg(th.original.Hash, 1, 6, writingHand), bIP, indicatorNotice{msgID, "@bob@b.test", writingHand, 6}},
		"v4-mapped src": {dg(th.original.Hash, 1, 6, writingHand), net.ParseIP("::ffff:192.0.2.2"), indicatorNotice{msgID, "@bob@b.test", writingHand, 6}},
		"batch add to":  {dg(th.batches[0].Hash, 5, 6, "⏳"), eIP, indicatorNotice{msgID, "@erin@e.test", "⏳", 6}},
		"ttl clamped":   {dg(th.original.Hash, 0, 255, writingHand), net.ParseIP("192.0.2.1"), indicatorNotice{msgID, "@alice@a.test", writingHand, 120}},
		"ttl 0 clears":  {dg(th.original.Hash, 2, 0, writingHand), net.ParseIP("192.0.2.3"), indicatorNotice{msgID, "@carol@c.test", "", 0}},
		"empty clears":  {dg(th.original.Hash, 2, 9, ""), net.ParseIP("192.0.2.3"), indicatorNotice{msgID, "@carol@c.test", "", 0}},
	} {
		if err := s.receive(ctx, c.b, c.src); err != nil {
			t.Fatalf("%s: receive: %v", name, err)
		}
		n := nextNotice(t, l, 5*time.Second)
		if n == nil || *n != c.want {
			t.Fatalf("%s: published %+v, want %+v", name, n, c.want)
		}
	}

	// FMSG_SKIP_AUTHORISED_IPS skips the source check
	s.skipIPCheck = true
	if err := s.receive(ctx, dg(th.original.Hash, 1, 6, writingHand), net.ParseIP("198.51.100.1")); err != nil {
		t.Fatalf("skip authorised IPs: %v", err)
	}
	if n := nextNotice(t, l, 5*time.Second); n == nil || n.From != "@bob@b.test" {
		t.Fatalf("skip authorised IPs published %+v", n)
	}
	s.skipIPCheck = false

	// per-source limit: the burst is spent, then datagrams are dropped
	// before anything else is checked
	src := net.ParseIP("192.0.2.200")
	for i := 0; i < int(indicatorRecvBurst); i++ {
		if err := s.receive(ctx, nil, src); !errors.Is(err, errIndicatorInvalid) {
			t.Fatalf("datagram %d: %v", i, err)
		}
	}
	if err := s.receive(ctx, dg(th.original.Hash, 1, 6, writingHand), src); !errors.Is(err, errIndicatorRateLimited) {
		t.Fatalf("over limit: %v", err)
	}
	clock.add(time.Second)
	if err := s.receive(ctx, nil, src); !errors.Is(err, errIndicatorInvalid) {
		t.Fatalf("after refill: %v", err)
	}
}

func TestIndicatorReceiveOriginalViaAddTo(t *testing.T) {
	db, dsn := testIndicatorDB(t)
	withIndicatorDomain(t, "here.test")
	th := newTestThread(t)
	msgID := storeTestThread(t, db, th, false)
	l := listenIndicators(t, dsn)
	s := newIndicatorService(sqlIndicatorStore{db}, nil, testResolver, time.Now)
	s.skipIPCheck = false

	// the original's participants come from a batch's from and to
	d := indicatorDatagram{Hash: hash32(th.original.Hash), Index: 2, TTL: 6, Data: writingHand}.marshal()
	if err := s.receive(context.Background(), d, net.ParseIP("192.0.2.3")); err != nil {
		t.Fatal(err)
	}
	if n := nextNotice(t, l, 5*time.Second); n == nil || *n != (indicatorNotice{msgID, "@carol@c.test", writingHand, 6}) {
		t.Fatalf("published %+v", n)
	}
	// and only those: index 4 (a batch's add to from) is out of range
	d = indicatorDatagram{Hash: hash32(th.original.Hash), Index: 4, TTL: 6, Data: writingHand}.marshal()
	if err := s.receive(context.Background(), d, net.ParseIP("192.0.2.2")); !errors.Is(err, errIndicatorIndex) {
		t.Fatalf("receive = %v", err)
	}
}

func TestIndicatorSubjectsOf(t *testing.T) {
	db, _ := testIndicatorDB(t)
	th := newTestThread(t)
	for _, withWire := range []bool{true, false} {
		id := storeTestThread(t, db, th, withWire)
		orig, batches, err := sqlIndicatorStore{db}.subjectsOf(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if orig == nil || !bytes.Equal(orig.Hash, th.original.Hash) ||
			fmt.Sprint(orig.Participants) != fmt.Sprint(th.original.Participants) {
			t.Fatalf("wire=%v: original = %+v", withWire, orig)
		}
		if len(batches) != 2 || !bytes.Equal(batches[1].Hash, th.batches[1].Hash) ||
			fmt.Sprint(batches[1].Participants) != fmt.Sprint(th.batches[1].Participants) {
			t.Fatalf("wire=%v: batches = %+v", withWire, batches)
		}
		// a fresh thread for the second pass: hashes are unique
		th = newTestThread(t)
	}
	if orig, batches, err := (sqlIndicatorStore{db}).subjectsOf(context.Background(), 999999); err != nil || orig != nil || batches != nil {
		t.Fatalf("missing message: %v %v %v", orig, batches, err)
	}
}

// TestIndicatorEndToEnd sends a local participant's indicator over loopback
// UDP to a second service that verifies it against the database and
// publishes it.
func TestIndicatorEndToEnd(t *testing.T) {
	db, dsn := testIndicatorDB(t)
	th := newTestThread(t)
	msgID := storeTestThread(t, db, th, true)
	l := listenIndicators(t, dsn)

	loopback := func(string) ([]net.IP, error) { return []net.IP{net.IPv4(127, 0, 0, 1)}, nil }
	recvConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer recvConn.Close()
	receiver := newIndicatorService(sqlIndicatorStore{db}, recvConn, loopback, time.Now)
	receiver.skipIPCheck = false
	receiver.isLocal = func(d string) bool { return strings.EqualFold(d, "b.test") }
	go receiver.serve()

	sendConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer sendConn.Close()
	sender := newIndicatorService(sqlIndicatorStore{db}, sendConn, loopback, time.Now)
	sender.isLocal = func(d string) bool { return strings.EqualFold(d, "here.test") }
	sender.remotePort = recvConn.LocalAddr().(*net.UDPAddr).Port

	if err := sender.handleNotice(context.Background(), fmt.Sprintf(`{"id":%d,"from":"@dave@here.test","emoji":%q,"ttl":6}`, msgID, writingHand)); err != nil {
		t.Fatal(err)
	}
	// every remote domain resolves to the one loopback receiver; it
	// publishes each (they are the same indicator)
	n := nextNotice(t, l, 5*time.Second)
	if n == nil || *n != (indicatorNotice{msgID, "@dave@here.test", writingHand, 6}) {
		t.Fatalf("published %+v", n)
	}
}

func TestIndicatorsEnabled(t *testing.T) {
	for v, want := range map[string]bool{"": true, "true": true, "1": true, "false": false, "FALSE": false, "0": false} {
		t.Setenv("FMSG_INDICATORS", v)
		if got := indicatorsEnabled(); got != want {
			t.Errorf("FMSG_INDICATORS=%q: enabled = %v", v, got)
		}
	}
}
