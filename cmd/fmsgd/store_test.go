package main

import (
	"bytes"
	"errors"
	"testing"
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
