package main

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeParentLinkStore struct {
	parentID  int64
	lookupErr error

	lookupHash        []byte
	setMsgID          int64
	setParentIDValue  int64
	setCalled         bool
	pendingParentID   int64
	pendingParentHash []byte
	pendingCalled     bool
}

func (s *fakeParentLinkStore) lookupParentID(parentHash []byte) (int64, error) {
	s.lookupHash = append([]byte(nil), parentHash...)
	return s.parentID, s.lookupErr
}

func (s *fakeParentLinkStore) setParentID(msgID int64, parentID int64) error {
	s.setCalled = true
	s.setMsgID = msgID
	s.setParentIDValue = parentID
	return nil
}

func (s *fakeParentLinkStore) setPendingChildrenParentID(parentID int64, parentHash []byte) error {
	s.pendingCalled = true
	s.pendingParentID = parentID
	s.pendingParentHash = append([]byte(nil), parentHash...)
	return nil
}

func TestResolveStoredParentRequiresExistingParent(t *testing.T) {
	store := &fakeParentLinkStore{}
	parentHash := []byte{1, 2, 3}

	err := resolveStoredParent(store, 10, parentHash, true)
	if err == nil {
		t.Fatal("resolveStoredParent returned nil error for required missing parent")
	}
	if !bytes.Equal(store.lookupHash, parentHash) {
		t.Fatalf("lookup hash = %v, want %v", store.lookupHash, parentHash)
	}
	if store.setCalled {
		t.Fatal("setParentID was called for missing parent")
	}
}

func TestResolveStoredParentAllowsOptionalMissingParent(t *testing.T) {
	store := &fakeParentLinkStore{}

	if err := resolveStoredParent(store, 10, []byte{1, 2, 3}, false); err != nil {
		t.Fatalf("resolveStoredParent returned error for optional missing parent: %v", err)
	}
	if store.setCalled {
		t.Fatal("setParentID was called for optional missing parent")
	}
}

func TestThreadHasFromDomainEmptyInputs(t *testing.T) {
	// No DB needed: empty hash/domain short-circuit.
	ok, err := threadHasFromDomain(nil, "example.com")
	if err != nil {
		t.Fatalf("nil hash: %v", err)
	}
	if ok {
		t.Fatal("nil hash: want false")
	}
	ok, err = threadHasFromDomain([]byte{1}, "")
	if err != nil {
		t.Fatalf("empty domain: %v", err)
	}
	if ok {
		t.Fatal("empty domain: want false")
	}
}

func TestResolveStoredParentSetsPidWhenParentExists(t *testing.T) {
	store := &fakeParentLinkStore{parentID: 42}

	if err := resolveStoredParent(store, 10, []byte{1, 2, 3}, true); err != nil {
		t.Fatalf("resolveStoredParent returned error: %v", err)
	}
	if !store.setCalled {
		t.Fatal("setParentID was not called")
	}
	if store.setMsgID != 10 || store.setParentIDValue != 42 {
		t.Fatalf("setParentID called with msgID=%d parentID=%d, want msgID=10 parentID=42", store.setMsgID, store.setParentIDValue)
	}
}

func TestResolveStoredParentPropagatesLookupError(t *testing.T) {
	lookupErr := errors.New("lookup failed")
	store := &fakeParentLinkStore{lookupErr: lookupErr}

	err := resolveStoredParent(store, 10, []byte{1, 2, 3}, true)
	if !errors.Is(err, lookupErr) {
		t.Fatalf("resolveStoredParent error = %v, want %v", err, lookupErr)
	}
	if store.setCalled {
		t.Fatal("setParentID was called after lookup error")
	}
}

func TestResolvePendingChildLinksBackfillsByParentHash(t *testing.T) {
	store := &fakeParentLinkStore{}
	parentHash := []byte{4, 5, 6}

	if err := resolvePendingChildLinks(store, 42, parentHash); err != nil {
		t.Fatalf("resolvePendingChildLinks returned error: %v", err)
	}
	if !store.pendingCalled {
		t.Fatal("setPendingChildrenParentID was not called")
	}
	if store.pendingParentID != 42 || !bytes.Equal(store.pendingParentHash, parentHash) {
		t.Fatalf("pending update got parentID=%d hash=%v, want parentID=42 hash=%v", store.pendingParentID, store.pendingParentHash, parentHash)
	}
}

func TestRequiresStoredParentUsesAddToFlag(t *testing.T) {
	parentHash := []byte{1, 2, 3}

	if !requiresStoredParent(&FMsgHeader{Flags: FlagHasPid, Pid: parentHash}) {
		t.Fatal("normal reply did not require stored parent")
	}
	if requiresStoredParent(&FMsgHeader{Flags: FlagHasPid | FlagHasAddTo, Pid: parentHash}) {
		t.Fatal("add-to message required stored parent")
	}
}

func TestWirePidForLoadedMessageAddToReferencesSharedMessage(t *testing.T) {
	parentHash := []byte{1, 2, 3}
	msgHash := []byte{4, 5, 6}

	got := wirePidForLoadedMessage(parentHash, msgHash, true)
	if !bytes.Equal(got, msgHash) {
		t.Fatalf("add-to wire pid = %v, want message hash %v", got, msgHash)
	}
}

func TestWirePidForLoadedMessageReplyKeepsParentHash(t *testing.T) {
	parentHash := []byte{1, 2, 3}
	msgHash := []byte{4, 5, 6}

	got := wirePidForLoadedMessage(parentHash, msgHash, false)
	if !bytes.Equal(got, parentHash) {
		t.Fatalf("reply wire pid = %v, want parent hash %v", got, parentHash)
	}
}

// An add-to message's row IS the shared message; its canonical hash is the
// original-form hash carried in Pid, not the add-to variant. Storing the
// variant would make replies reference a hash the origin host never knows.
func TestCanonicalMsgHashAddToUsesPid(t *testing.T) {
	origHash := []byte{9, 8, 7, 6}

	got, err := canonicalMsgHash(&FMsgHeader{Flags: FlagHasPid | FlagHasAddTo, Pid: origHash})
	if err != nil {
		t.Fatalf("canonicalMsgHash returned error: %v", err)
	}
	if !bytes.Equal(got, origHash) {
		t.Fatalf("add-to canonical hash = %v, want original-form hash %v", got, origHash)
	}
}

// A non-add-to message must never take the add-to attach path: a colliding
// sha256 there is a genuine duplicate, not the shared message being extended.
// existingMsgIDForAddTo short-circuits before touching the database for it.
func TestExistingMsgIDForAddToSkipsNonAddTo(t *testing.T) {
	id, err := existingMsgIDForAddTo(nil, &FMsgHeader{Flags: FlagHasPid}, []byte{1, 2, 3})
	if err != nil {
		t.Fatalf("existingMsgIDForAddTo returned error: %v", err)
	}
	if id != 0 {
		t.Fatalf("non-add-to message returned existing id %d, want 0", id)
	}
}

// An add-to message's Pid identifies the message itself, not a parent, so it
// must not be resolved as a relational parent. A plain reply's Pid is a parent.
func TestRelationalParentHashAddToHasNoParent(t *testing.T) {
	pid := []byte{1, 2, 3}

	if got := relationalParentHash(&FMsgHeader{Flags: FlagHasPid | FlagHasAddTo, Pid: pid}); got != nil {
		t.Fatalf("add-to relational parent = %v, want nil", got)
	}
	if got := relationalParentHash(&FMsgHeader{Flags: FlagHasPid, Pid: pid}); !bytes.Equal(got, pid) {
		t.Fatalf("reply relational parent = %v, want %v", got, pid)
	}
	if got := relationalParentHash(&FMsgHeader{}); got != nil {
		t.Fatalf("new-thread relational parent = %v, want nil", got)
	}
}

func TestInboundRecipientRow(t *testing.T) {
	now := 1234.5
	local := FMsgAddress{User: "alice", Domain: "here.example"}
	rejected := FMsgAddress{User: "carol", Domain: "here.example"}
	remote := FMsgAddress{User: "bob", Domain: "there.example"}
	outcome := map[string]uint8{
		"@alice@here.example": RejectCodeAccept,
		"@carol@here.example": RejectCodeUserFull,
	}

	delivered, code := inboundRecipientRow(local, outcome, now)
	if delivered != now || code != nil {
		t.Fatalf("accepted local: got (%v, %v), want (%v, nil)", delivered, code, now)
	}

	delivered, code = inboundRecipientRow(rejected, outcome, now)
	if delivered != nil || code != int16(RejectCodeUserFull) {
		t.Fatalf("rejected local: got (%v, %v), want (nil, %d)", delivered, code, RejectCodeUserFull)
	}

	delivered, code = inboundRecipientRow(remote, outcome, now)
	if delivered != nil || code != int16(localResponseCodeNotOurDelivery) {
		t.Fatalf("remote: got (%v, %v), want (nil, %d)", delivered, code, localResponseCodeNotOurDelivery)
	}
}

type fakeExecRecord struct {
	query string
	args  []driver.NamedValue
}

type fakeSQLConnector struct {
	mu          sync.Mutex
	execRecords []fakeExecRecord
	queryRowVal int64
}

func (c *fakeSQLConnector) Connect(context.Context) (driver.Conn, error) {
	return &fakeSQLConn{connector: c}, nil
}

func (c *fakeSQLConnector) Driver() driver.Driver { return fakeSQLDriver{} }

type fakeSQLDriver struct{}

func (fakeSQLDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("unexpected Open")
}

type fakeSQLConn struct {
	connector *fakeSQLConnector
}

func (c *fakeSQLConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("Prepare not implemented")
}

func (c *fakeSQLConn) Close() error { return nil }

func (c *fakeSQLConn) Begin() (driver.Tx, error) { return fakeSQLTx{}, nil }

func (c *fakeSQLConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return fakeSQLTx{}, nil
}

func (c *fakeSQLConn) QueryContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	return &fakeSQLRows{val: c.connector.queryRowVal}, nil
}

func (c *fakeSQLConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.connector.mu.Lock()
	copied := append([]driver.NamedValue(nil), args...)
	c.connector.execRecords = append(c.connector.execRecords, fakeExecRecord{query: query, args: copied})
	c.connector.mu.Unlock()
	return driver.RowsAffected(1), nil
}

type fakeSQLTx struct{}

func (fakeSQLTx) Commit() error   { return nil }
func (fakeSQLTx) Rollback() error { return nil }

type fakeSQLRows struct {
	val  int64
	done bool
}

func (r *fakeSQLRows) Columns() []string { return []string{"id"} }
func (r *fakeSQLRows) Close() error      { return nil }
func (r *fakeSQLRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = r.val
	return nil
}

func TestSenderPoolSaturationDoesNotStarveInboundPool(t *testing.T) {
	origOpen := openDBPool
	closeDB()
	closeSenderDB()
	t.Cleanup(func() {
		closeDB()
		closeSenderDB()
		openDBPool = origOpen
	})

	conn := &fakeSQLConnector{queryRowVal: 42}
	openDBPool = func() (*sql.DB, error) {
		return sql.OpenDB(conn), nil
	}

	sDB, err := getSenderDB()
	if err != nil {
		t.Fatalf("getSenderDB: %v", err)
	}
	inDB, err := getDB()
	if err != nil {
		t.Fatalf("getDB: %v", err)
	}
	if sDB == inDB {
		t.Fatal("sender pool and inbound pool must be distinct *sql.DB instances")
	}

	// Fill every single connection in the sender pool with open transactions.
	heldTxs := make([]*sql.Tx, 0, senderDBMaxOpenConns)
	defer func() {
		for _, tx := range heldTxs {
			_ = tx.Rollback()
		}
	}()
	for i := 0; i < senderDBMaxOpenConns; i++ {
		tx, err := sDB.Begin()
		if err != nil {
			t.Fatalf("sender Begin #%d: %v", i, err)
		}
		heldTxs = append(heldTxs, tx)
	}

	// Queue additional Begin calls beyond senderDBMaxOpenConns to confirm the
	// sender pool is genuinely saturated and blocking.
	const extraWaiters = 4
	extraDone := make(chan *sql.Tx, extraWaiters)
	var wg sync.WaitGroup
	for i := 0; i < extraWaiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := sDB.Begin()
			if err != nil {
				t.Errorf("extra sender Begin: %v", err)
				return
			}
			extraDone <- tx
		}()
	}

	// Wait briefly until at least one extra waiter is blocked on the sender pool.
	deadline := time.Now().Add(time.Second)
	for sDB.Stats().WaitCount == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if stats := sDB.Stats(); stats.InUse != senderDBMaxOpenConns || stats.WaitCount == 0 {
		t.Fatalf("expected saturated sender pool (inUse=%d/%d, waitCount=%d)", stats.InUse, senderDBMaxOpenConns, stats.WaitCount)
	}

	// While the sender pool is completely saturated, an inbound getDB() query
	// must complete immediately without blocking.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var got int64
	if err := inDB.QueryRowContext(ctx, "SELECT id FROM msg WHERE sha256 = $1", []byte{1}).Scan(&got); err != nil {
		t.Fatalf("inbound query blocked or failed while sender pool saturated: %v", err)
	}
	if got != 42 {
		t.Fatalf("inbound query returned %d, want 42", got)
	}
	id, err := lookupMsgIdByHash([]byte{1, 2, 3})
	if err != nil || id != 42 {
		t.Fatalf("lookupMsgIdByHash = (%d, %v), want (42, nil)", id, err)
	}

	// Release held sender transactions and drain the queued waiters cleanly.
	for _, tx := range heldTxs {
		_ = tx.Rollback()
	}
	heldTxs = nil
	wg.Wait()
	close(extraDone)
	for tx := range extraDone {
		_ = tx.Rollback()
	}
}

func TestUpdateRecipientSQL(t *testing.T) {
	conn := &fakeSQLConnector{}
	db := sql.OpenDB(conn)
	t.Cleanup(func() { _ = db.Close() })

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback()

	cases := []struct {
		name      string
		table     string
		delivered bool
		wantTable string
		wantScope string
		wantID    int64
		wantSet   string
	}{
		{"msg_to delivered", "msg_to", true, "UPDATE msg_to SET", "WHERE msg_id = $3 AND addr = $4", 10, "time_delivered = $1, response_code = $2"},
		{"msg_to failed", "msg_to", false, "UPDATE msg_to SET", "WHERE msg_id = $3 AND addr = $4", 10, "time_last_attempt = $1, response_code = $2"},
		{"msg_add_to delivered", "msg_add_to", true, "UPDATE msg_add_to SET", "WHERE batch_id = $3 AND addr = $4", 20, "time_delivered = $1, response_code = $2"},
		{"msg_add_to failed", "msg_add_to", false, "UPDATE msg_add_to SET", "WHERE batch_id = $3 AND addr = $4", 20, "time_last_attempt = $1, response_code = $2"},
	}

	for i, tc := range cases {
		if err := updateRecipient(tx, tc.table, "@bob@example.com", 10, 20, 1234.5, 200, tc.delivered); err != nil {
			t.Fatalf("%s: updateRecipient error: %v", tc.name, err)
		}
		rec := conn.execRecords[i]
		if !strings.Contains(rec.query, tc.wantTable) || !strings.Contains(rec.query, tc.wantScope) || !strings.Contains(rec.query, tc.wantSet) {
			t.Fatalf("%s: unexpected SQL:\n%s", tc.name, rec.query)
		}
		if !tc.delivered && !strings.Contains(rec.query, "attempt_count = attempt_count + 1") {
			t.Fatalf("%s: missing attempt_count increment in SQL:\n%s", tc.name, rec.query)
		}
		if len(rec.args) != 4 || rec.args[2].Value != tc.wantID || rec.args[3].Value != "@bob@example.com" {
			t.Fatalf("%s: unexpected args: %+v", tc.name, rec.args)
		}
	}
}
