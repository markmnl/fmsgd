package main

import (
	"testing"
)

func TestOutgoingMapOperations(t *testing.T) {
	initOutgoing()

	h := &FMsgHeader{
		Version: 1,
		Flags:   0,
		From:    FMsgAddress{User: "alice", Domain: "a.com"},
		To:      []FMsgAddress{{User: "bob", Domain: "b.com"}},
		Topic:   "test",
		Type:    "text/plain",
	}

	var hash [32]byte
	copy(hash[:], h.GetHeaderHash())

	// Lookup before register should fail
	_, ok := lookupOutgoing(hash)
	if ok {
		t.Fatal("lookupOutgoing found entry before register")
	}

	// Register
	registerOutgoing(hash, h)

	// Lookup after register should succeed
	got, ok := lookupOutgoing(hash)
	if !ok {
		t.Fatal("lookupOutgoing failed after register")
	}
	if got != h {
		t.Fatal("lookupOutgoing returned different pointer")
	}

	// Delete
	deleteOutgoing(hash)

	_, ok = lookupOutgoing(hash)
	if ok {
		t.Fatal("lookupOutgoing found entry after delete")
	}
}

func TestOutgoingMapMultipleEntries(t *testing.T) {
	initOutgoing()

	h1 := &FMsgHeader{
		Version:   1,
		From:      FMsgAddress{User: "a", Domain: "b.com"},
		To:        []FMsgAddress{{User: "c", Domain: "d.com"}},
		Timestamp: 1.0,
		Type:      "text/plain",
	}
	h2 := &FMsgHeader{
		Version:   1,
		From:      FMsgAddress{User: "x", Domain: "y.com"},
		To:        []FMsgAddress{{User: "z", Domain: "w.com"}},
		Timestamp: 2.0,
		Type:      "text/plain",
	}

	var hash1, hash2 [32]byte
	copy(hash1[:], h1.GetHeaderHash())
	copy(hash2[:], h2.GetHeaderHash())

	registerOutgoing(hash1, h1)
	registerOutgoing(hash2, h2)

	got1, ok1 := lookupOutgoing(hash1)
	got2, ok2 := lookupOutgoing(hash2)

	if !ok1 || got1 != h1 {
		t.Error("failed to look up h1")
	}
	if !ok2 || got2 != h2 {
		t.Error("failed to look up h2")
	}

	// Delete one, other should remain
	deleteOutgoing(hash1)
	_, ok1 = lookupOutgoing(hash1)
	_, ok2 = lookupOutgoing(hash2)
	if ok1 {
		t.Error("h1 still present after delete")
	}
	if !ok2 {
		t.Error("h2 missing after deleting h1")
	}
}
