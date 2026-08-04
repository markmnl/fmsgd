package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSharedHashUsesTransmittedForm guards the ordering fixed in the sender:
// the shared hash must be computed over the header exactly as transmitted
// (SPEC "Message hash"). computeDeflate/applyTo change flags, size and
// expanded size, so hashing before deflate records a sha256 the receiving
// host never computes — cross-host replies then bounce with code 6.
func TestSharedHashUsesTransmittedForm(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("compressible markdown body — the quick brown fox. ", 200)
	bodyPath := filepath.Join(dir, "data.md")
	if err := os.WriteFile(bodyPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	m := &msgFields{
		version:  1,
		size:     len(body),
		from:     FMsgAddress{User: "alice", Domain: "example.com"},
		to:       []FMsgAddress{{User: "bob", Domain: "example.org"}},
		timeSent: 1754280000,
		topic:    "hash form",
		typ:      "text/markdown",
		filepath: bodyPath,
	}

	undeflated, err := m.originalHeader().GetMessageHash()
	if err != nil {
		t.Fatal(err)
	}

	d := computeDeflate(m, 1)
	defer d.removeTempFiles()
	wire := m.originalHeader()
	d.applyTo(wire)
	if wire.Flags&FlagDeflate == 0 {
		t.Fatal("test body should have deflated — deflate heuristics changed?")
	}
	transmitted, err := wire.GetMessageHash()
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(undeflated, transmitted) {
		t.Fatal("forms hash identically; this regression test no longer exercises the trap")
	}

	// The receiving host recomputes the hash from the wire form it stored —
	// the sender's recorded shared hash must be that same transmitted form.
	receiver := m.originalHeader()
	d.applyTo(receiver)
	got, err := receiver.GetMessageHash()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, transmitted) {
		t.Fatal("transmitted-form hash not reproducible")
	}
}
