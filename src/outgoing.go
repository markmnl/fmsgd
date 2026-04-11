package main

import "sync"

// outgoingEntry tracks an in-flight outgoing message header together with
// the set of Host-B IPs currently being serviced for that message.
// The IP set is used to validate incoming challenges (§10.5 step 2).
type outgoingEntry struct {
	header *FMsgHeader
	ips    map[string]struct{}
}

// outgoingMap indexes in-flight message headers by their header hash.
// All access is synchronised via outgoingMu.
var outgoingMap map[[32]byte]*outgoingEntry
var outgoingMu sync.RWMutex

func initOutgoing() {
	outgoingMap = make(map[[32]byte]*outgoingEntry)
}

// registerOutgoing records hash → (header, ip) so challenge handlers can look
// it up. Multiple IPs may be registered for the same hash when the same message
// is being concurrently delivered to different domains (§10.2 step 2).
func registerOutgoing(hash [32]byte, h *FMsgHeader, ip string) {
	outgoingMu.Lock()
	e, ok := outgoingMap[hash]
	if !ok {
		e = &outgoingEntry{header: h, ips: make(map[string]struct{})}
		outgoingMap[hash] = e
	}
	e.ips[ip] = struct{}{}
	outgoingMu.Unlock()
}

// lookupOutgoing returns the header for hash iff ip is a registered Host-B IP
// for that entry. Returns (nil, false) if the hash is unknown or ip is not in
// the registered set (§10.5 step 2).
func lookupOutgoing(hash [32]byte, ip string) (*FMsgHeader, bool) {
	outgoingMu.RLock()
	e, ok := outgoingMap[hash]
	if !ok {
		outgoingMu.RUnlock()
		return nil, false
	}
	_, ipOK := e.ips[ip]
	h := e.header
	outgoingMu.RUnlock()
	if !ipOK {
		return nil, false
	}
	return h, true
}

// removeOutgoingIP removes ip from the entry's IP set. When the set becomes
// empty the map entry is deleted entirely (§10.2 step 7).
func removeOutgoingIP(hash [32]byte, ip string) {
	outgoingMu.Lock()
	if e, ok := outgoingMap[hash]; ok {
		delete(e.ips, ip)
		if len(e.ips) == 0 {
			delete(outgoingMap, hash)
		}
	}
	outgoingMu.Unlock()
}
