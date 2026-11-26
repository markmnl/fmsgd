package main

import "testing"

func TestDeliverySlots(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		maxConcurrentSend, poolSize int
		want                        int
	}{
		{"the pool bounds the default", 1024, 32, 31},
		{"FMSG_MAX_CONCURRENT_SEND bounds a large pool", 8, 32, 8},
		{"a larger pool raises the limit", 1024, 200, 199},
		{"never below one delivery", 1024, 1, 1},
		{"a non-positive limit still allows one", 0, 32, 1},
	} {
		if got := deliverySlots(tc.maxConcurrentSend, tc.poolSize); got != tc.want {
			t.Errorf("%s: deliverySlots(%d, %d) = %d, want %d", tc.name, tc.maxConcurrentSend, tc.poolSize, got, tc.want)
		}
	}
}

func newTestInFlightTracker() *inFlightTracker {
	f := newInFlightTracker()
	f.domains = &domainReachability{unreachable: make(map[string]struct{})}
	return f
}

func TestInFlightTrackerDeduplicatesTargets(t *testing.T) {
	f := newTestInFlightTracker()
	target := pendingTarget{MsgID: 1, Domain: "Example.com"}

	if !f.acquire(target) {
		t.Fatal("first acquire refused")
	}
	if f.acquire(pendingTarget{MsgID: 1, Domain: "example.COM"}) {
		t.Fatal("the same target was acquired twice")
	}
	if !f.acquire(pendingTarget{MsgID: 2, Domain: "example.com"}) {
		t.Fatal("a reachable domain must take more than one delivery at a time")
	}
	f.release(target)
	if !f.acquire(target) {
		t.Fatal("a released target could not be acquired again")
	}
}

// A domain the sender could not connect to gets one delivery at a time, so
// its backlog cannot occupy every delivery slot for the length of a connect
// timeout while other domains wait.
func TestInFlightTrackerHoldsUnreachableDomainToOneDelivery(t *testing.T) {
	f := newTestInFlightTracker()
	down, up := "down.example.com", "up.example.com"

	// Nothing is known about the domain yet: it gets its full share.
	if !f.acquire(pendingTarget{MsgID: 1, Domain: down}) || !f.acquire(pendingTarget{MsgID: 2, Domain: down}) {
		t.Fatal("deliveries to a domain not known to be down were refused")
	}
	f.domains.setReachable("DOWN.example.com", false)

	if f.acquire(pendingTarget{MsgID: 3, Domain: down}) {
		t.Fatal("an unreachable domain with deliveries in flight took another slot")
	}
	if !f.acquire(pendingTarget{MsgID: 4, Domain: up}) {
		t.Fatal("another domain was held back by the unreachable one")
	}

	// Once nothing is in flight to it, one delivery goes out as the probe.
	f.release(pendingTarget{MsgID: 1, Domain: down})
	if f.acquire(pendingTarget{MsgID: 3, Domain: down}) {
		t.Fatal("a second delivery was allowed while one was still in flight")
	}
	f.release(pendingTarget{MsgID: 2, Domain: down})
	if !f.acquire(pendingTarget{MsgID: 3, Domain: down}) {
		t.Fatal("the probe delivery to an unreachable domain was refused")
	}
	if f.acquire(pendingTarget{MsgID: 5, Domain: down}) {
		t.Fatal("more than one probe delivery at a time")
	}

	// The probe connects: the domain gets its full share again.
	f.domains.setReachable(down, true)
	if !f.acquire(pendingTarget{MsgID: 5, Domain: down}) {
		t.Fatal("a domain that became reachable was still held back")
	}
}

func TestInFlightTrackerReleaseOfUnknownTargetIsHarmless(t *testing.T) {
	f := newTestInFlightTracker()
	f.release(pendingTarget{MsgID: 9, Domain: "example.com"})
	f.domains.setReachable("example.com", false)
	if !f.acquire(pendingTarget{MsgID: 9, Domain: "example.com"}) {
		t.Fatal("a stray release left the domain counted as busy")
	}
}
