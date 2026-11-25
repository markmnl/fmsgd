package main

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestLookupAuthorisedIPsResolverTimeoutAllowsFailover(t *testing.T) {
	t.Setenv("FMSG_REQUIRE_DNSSEC", "")

	// Per-server DNSSEC clients keep the 5s per-attempt timeout.
	if dnsClient.Timeout != dnsLookupTimeout || dnsTCPClient.Timeout != dnsLookupTimeout {
		t.Fatalf("DNSSEC client timeouts = (%v, %v), want %v", dnsClient.Timeout, dnsTCPClient.Timeout, dnsLookupTimeout)
	}

	origLookup := lookupIPWithContext
	t.Cleanup(func() {
		lookupIPWithContext = origLookup
	})

	wantIP := net.ParseIP("192.0.2.42")
	var attempts int
	lookupIPWithContext = func(ctx context.Context, network, host string) ([]net.IP, error) {
		if network != "ip" {
			t.Errorf("network = %q, want %q", network, "ip")
		}
		if host != "fmsg.example.com" {
			t.Errorf("host = %q, want %q", host, "fmsg.example.com")
		}

		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("expected context with deadline for non-DNSSEC lookup")
		}
		remaining := time.Until(deadline)
		if remaining < 15*time.Second {
			t.Fatalf("resolver context deadline remaining = %v, want >= 15s to allow nameserver failover", remaining)
		}
		if remaining > resolverLookupTimeout {
			t.Fatalf("resolver context deadline remaining = %v, want <= %v", remaining, resolverLookupTimeout)
		}

		// Simulate a multi-nameserver resolver: nameserver 1 times out after
		// one 5s per-attempt timeout, and nameserver 2 succeeds because the
		// overall context still has budget remaining.
		nameservers := []func() ([]net.IP, error){
			func() ([]net.IP, error) {
				remaining -= dnsLookupTimeout
				return nil, errors.New("i/o timeout on primary nameserver")
			},
			func() ([]net.IP, error) {
				if remaining <= 0 || ctx.Err() != nil {
					return nil, context.DeadlineExceeded
				}
				return []net.IP{wantIP}, nil
			},
		}

		var lastErr error
		for _, queryNS := range nameservers {
			attempts++
			ips, err := queryNS()
			if err == nil {
				return ips, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}

	ips, err := lookupAuthorisedIPs("example.com")
	if err != nil {
		t.Fatalf("lookupAuthorisedIPs failed across nameserver failover: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("nameserver attempts = %d, want 2 (failover)", attempts)
	}
	if len(ips) != 1 || !ips[0].Equal(wantIP) {
		t.Fatalf("lookupAuthorisedIPs = %v, want [%s]", ips, wantIP)
	}
}
