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

	const ip = "1.2.3.4"

	// Lookup before register should fail
	_, ok := lookupOutgoing(hash, ip)
	if ok {
		t.Fatal("lookupOutgoing found entry before register")
	}

	// Register
	registerOutgoing(hash, h, ip)

	// Lookup with correct IP should succeed
	got, ok := lookupOutgoing(hash, ip)
	if !ok {
		t.Fatal("lookupOutgoing failed after register")
	}
	if got != h {
		t.Fatal("lookupOutgoing returned different pointer")
	}

	// Lookup with wrong IP should fail
	_, ok = lookupOutgoing(hash, "9.9.9.9")
	if ok {
		t.Fatal("lookupOutgoing succeeded with wrong IP")
	}

	// Remove IP — entry should be gone
	removeOutgoingIP(hash, ip)

	_, ok = lookupOutgoing(hash, ip)
	if ok {
		t.Fatal("lookupOutgoing found entry after removeOutgoingIP")
	}
}

func TestOutgoingMapMultipleIPs(t *testing.T) {
	initOutgoing()

	h := &FMsgHeader{
		Version:   1,
		From:      FMsgAddress{User: "a", Domain: "b.com"},
		To:        []FMsgAddress{{User: "c", Domain: "d.com"}},
		Timestamp: 1.0,
		Type:      "text/plain",
	}

	var hash [32]byte
	copy(hash[:], h.GetHeaderHash())

	registerOutgoing(hash, h, "1.1.1.1")
	registerOutgoing(hash, h, "2.2.2.2")

	// Both IPs should resolve
	for _, ip := range []string{"1.1.1.1", "2.2.2.2"} {
		if _, ok := lookupOutgoing(hash, ip); !ok {
			t.Errorf("expected lookup to succeed for IP %s", ip)
		}
	}

	// Removing first IP still leaves entry for second
	removeOutgoingIP(hash, "1.1.1.1")
	if _, ok := lookupOutgoing(hash, "1.1.1.1"); ok {
		t.Error("1.1.1.1 still present after remove")
	}
	if _, ok := lookupOutgoing(hash, "2.2.2.2"); !ok {
		t.Error("2.2.2.2 missing after removing 1.1.1.1")
	}

	// Removing last IP deletes the entry
	removeOutgoingIP(hash, "2.2.2.2")
	if _, ok := lookupOutgoing(hash, "2.2.2.2"); ok {
		t.Error("entry still present after removing last IP")
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

	registerOutgoing(hash1, h1, "1.1.1.1")
	registerOutgoing(hash2, h2, "2.2.2.2")

	got1, ok1 := lookupOutgoing(hash1, "1.1.1.1")
	got2, ok2 := lookupOutgoing(hash2, "2.2.2.2")

	if !ok1 || got1 != h1 {
		t.Error("failed to look up h1")
	}
	if !ok2 || got2 != h2 {
		t.Error("failed to look up h2")
	}

	// Remove one, other should remain
	removeOutgoingIP(hash1, "1.1.1.1")
	_, ok1 = lookupOutgoing(hash1, "1.1.1.1")
	_, ok2 = lookupOutgoing(hash2, "2.2.2.2")
	if ok1 {
		t.Error("h1 still present after remove")
	}
	if !ok2 {
		t.Error("h2 missing after removing h1")
	}
}
