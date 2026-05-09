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
