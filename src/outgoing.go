package main

import "sync"

// outgoing message headers keyed on header hash.
// All access is synchronised via outgoingMu.
var outgoingMap map[[32]byte]*FMsgHeader
var outgoingMu sync.RWMutex

func initOutgoing() {
	outgoingMap = make(map[[32]byte]*FMsgHeader)
}

// registerOutgoing stores a header in the outgoing map so challenge handlers
// running on other goroutines can look it up.
func registerOutgoing(hash [32]byte, h *FMsgHeader) {
	outgoingMu.Lock()
	outgoingMap[hash] = h
	outgoingMu.Unlock()
}

// lookupOutgoing retrieves an outgoing header by its hash.
func lookupOutgoing(hash [32]byte) (*FMsgHeader, bool) {
	outgoingMu.RLock()
	h, ok := outgoingMap[hash]
	outgoingMu.RUnlock()
	return h, ok
}

// deleteOutgoing removes an outgoing header after delivery completes.
func deleteOutgoing(hash [32]byte) {
	outgoingMu.Lock()
	delete(outgoingMap, hash)
	outgoingMu.Unlock()
}
