package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Tests for the terminal flag (SPEC v0.6.0 §3 bit 6): a terminal message is a
// leaf, so a reply to it or an add-to batch of it is rejected with code 1.

func TestValidateMessageFlagsAcceptsTerminal(t *testing.T) {
	c := &testConn{}
	if err := validateMessageFlags(c, FlagHasPid|FlagNoReply|FlagTerminal); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Len() != 0 {
		t.Fatalf("unexpected bytes written: %v", c.Bytes())
	}
}

func TestValidateMessageFlagsRejectsAddToWithTerminal(t *testing.T) {
	c := &testConn{}
	err := validateMessageFlags(c, FlagHasPid|FlagHasAddTo|FlagTerminal)
	if err == nil {
		t.Fatal("expected error for add-to message with terminal flag set")
	}
	if got := c.Bytes(); len(got) != 1 || got[0] != RejectCodeInvalid {
		t.Fatalf("wrote %v, want single code %d (invalid)", got, RejectCodeInvalid)
	}
}

var (
	testAlice = FMsgAddress{User: "alice", Domain: "example.com"}
	testBob   = FMsgAddress{User: "bob", Domain: "example.edu"}
	testCarol = FMsgAddress{User: "carol", Domain: "example.edu"}
)

// storedParentForTest returns a retrievable stored message from alice to bob
// with the given flags.
func storedParentForTest(t *testing.T, flags uint8) *FMsgHeader {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(path, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &FMsgHeader{
		Version:   1,
		Flags:     flags,
		From:      testAlice,
		To:        []FMsgAddress{testBob},
		Timestamp: 1000,
		Type:      "text/plain",
		Size:      2,
		Filepath:  path,
	}
}

// stubStoredParent makes every store lookup resolve to parent (message id 1)
// with no add-to batches recorded, and restores the real lookups afterwards.
func stubStoredParent(t *testing.T, parent *FMsgHeader) {
	t.Helper()
	origLookup, origGet, origBatch, origRecorded := lookupMsgIdByHashFn, getMsgByIDFn, getMsgByBatchHashFn, addToBatchRecordedFn
	origDomain := Domain
	t.Cleanup(func() {
		lookupMsgIdByHashFn, getMsgByIDFn, getMsgByBatchHashFn, addToBatchRecordedFn = origLookup, origGet, origBatch, origRecorded
		Domain = origDomain
	})
	lookupMsgIdByHashFn = func([]byte) (int64, error) { return 1, nil }
	getMsgByIDFn = func(int64) (*FMsgHeader, error) { return parent, nil }
	getMsgByBatchHashFn = func([]byte) (*FMsgHeader, error) { return nil, nil }
	addToBatchRecordedFn = func(int64, []byte) (bool, error) { return false, nil }
	Domain = testBob.Domain
}

func replyForTest(from FMsgAddress) *FMsgHeader {
	return &FMsgHeader{
		Version:   1,
		Flags:     FlagHasPid,
		Pid:       make([]byte, 32),
		From:      from,
		To:        []FMsgAddress{testAlice},
		Timestamp: 2000,
		Type:      "text/plain",
	}
}

func TestValidatePidReplyPathRejectsTerminalParent(t *testing.T) {
	stubStoredParent(t, storedParentForTest(t, FlagTerminal))
	c := &testConn{}
	err := validatePidReplyPath(c, replyForTest(testBob))
	if err == nil {
		t.Fatal("expected error for reply to terminal parent")
	}
	if got := c.Bytes(); len(got) != 1 || got[0] != RejectCodeInvalid {
		t.Fatalf("wrote %v, want single code %d (invalid)", got, RejectCodeInvalid)
	}
}

func TestValidatePidReplyPathAcceptsNonTerminalParent(t *testing.T) {
	stubStoredParent(t, storedParentForTest(t, FlagNoReply))
	c := &testConn{}
	if err := validatePidReplyPath(c, replyForTest(testBob)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Len() != 0 {
		t.Fatalf("unexpected bytes written: %v", c.Bytes())
	}
}

func addToForTest() *FMsgHeader {
	from := testAlice
	return &FMsgHeader{
		Version:   1,
		Flags:     FlagHasPid | FlagHasAddTo,
		Pid:       make([]byte, 32),
		From:      testAlice,
		To:        []FMsgAddress{testBob},
		AddToFrom: &from,
		AddTo:     []FMsgAddress{testCarol},
		Timestamp: 2000,
		Type:      "text/plain",
		Size:      2,
	}
}

// A sender that strips the terminal bit from an add-to copy of a terminal
// message is still caught by the stored-parent check.
func TestHandleAddToPathRejectsTerminalParent(t *testing.T) {
	stubStoredParent(t, storedParentForTest(t, FlagTerminal))
	c := &testConn{}
	_, err := handleAddToPath(c, addToForTest())
	if err == nil {
		t.Fatal("expected error for add-to of terminal parent")
	}
	if got := c.Bytes(); len(got) != 1 || got[0] != RejectCodeInvalid {
		t.Fatalf("wrote %v, want single code %d (invalid)", got, RejectCodeInvalid)
	}
}

func TestHandleAddToPathAcceptsNonTerminalParent(t *testing.T) {
	stubStoredParent(t, storedParentForTest(t, 0))
	c := &testConn{}
	h, err := handleAddToPath(c, addToForTest())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h.InitialResponseCode != AcceptCodeSkipData {
		t.Fatalf("InitialResponseCode = %d, want %d (skip data)", h.InitialResponseCode, AcceptCodeSkipData)
	}
	if c.Len() != 0 {
		t.Fatalf("unexpected bytes written: %v", c.Bytes())
	}
}
