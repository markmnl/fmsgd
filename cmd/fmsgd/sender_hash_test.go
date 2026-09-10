package main

import (
	"bytes"
	"github.com/markmnl/fmsgd/pkg/fmsg"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The prepared identity must cover the transmitted header, after compression.
// Outgoing delivery restores this representation without choosing it again.
func TestSharedHashUsesTransmittedForm(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("compressible markdown body — the quick brown fox. ", 200)
	bodyPath := filepath.Join(dir, "data.md")
	if err := os.WriteFile(bodyPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	h := &FMsgHeader{
		Version:   1,
		Size:      uint32(len(body)),
		From:      FMsgAddress{User: "alice", Domain: "example.com"},
		To:        []FMsgAddress{{User: "bob", Domain: "example.org"}},
		Timestamp: 1754280000,
		Topic:     "hash form",
		Type:      "text/markdown",
		Filepath:  bodyPath,
	}

	undeflated, err := h.GetMessageHash()
	if err != nil {
		t.Fatal(err)
	}

	wire, dir, err := fmsg.Prepare(h)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
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
	data, err := fmsg.MarshalPrepared(wire)
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := fmsg.UnmarshalPrepared(data, transmitted)
	if err != nil {
		t.Fatal(err)
	}
	got, err := receiver.GetMessageHash()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, transmitted) {
		t.Fatal("transmitted-form hash not reproducible")
	}
}
