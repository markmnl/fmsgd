package main

import (
	"bytes"
	"github.com/markmnl/fmsgd/pkg/fmsg"
	"os"
	"path/filepath"
	"testing"
)

// Outgoing headers encode Common Media Type IDs (SPEC §4) where the stored
// type string has one, as FMSG-005 requires for reactions (ID 56).

func commonTypeTestHeader(t *testing.T) *FMsgHeader {
	t.Helper()
	dir := t.TempDir()
	bodyPath := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(bodyPath, []byte("👍"), 0o600); err != nil {
		t.Fatal(err)
	}
	attPath := filepath.Join(dir, "pic.png")
	if err := os.WriteFile(attPath, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &FMsgHeader{
		Version:   1,
		Size:      4,
		From:      FMsgAddress{User: "alice", Domain: "example.com"},
		To:        []FMsgAddress{{User: "bob", Domain: "example.org"}},
		Timestamp: 1754280000,
		Topic:     "types",
		Type:      "text/plain;charset=UTF-8",
		Filepath:  bodyPath,
		Attachments: []FMsgAttachmentHeader{
			{Type: "image/png", Filename: "pic.png", Size: 3, Filepath: attPath},
			{Type: "application/x-custom", Filename: "custom.bin", Size: 3, Filepath: attPath},
		},
	}
}

func TestApplyCommonTypesEncodesIDs(t *testing.T) {
	h := commonTypeTestHeader(t)
	if !fmsg.ApplyCommonTypes(h) {
		t.Fatal("applyCommonTypes reported no change")
	}
	if h.Flags&FlagCommonType == 0 || h.TypeID != 56 {
		t.Errorf("message type: flags=%#08b id=%d, want common type ID 56", h.Flags, h.TypeID)
	}
	if h.Attachments[0].Flags&1 == 0 || h.Attachments[0].TypeID != 38 {
		t.Errorf("png attachment: flags=%#08b id=%d, want common type ID 38", h.Attachments[0].Flags, h.Attachments[0].TypeID)
	}
	if h.Attachments[1].Flags&1 != 0 {
		t.Errorf("unmapped attachment type must stay a string, flags=%#08b", h.Attachments[1].Flags)
	}
	wire := h.Encode()
	if bytes.Contains(wire, []byte("text/plain")) || bytes.Contains(wire, []byte("image/png")) {
		t.Error("common type strings must not appear on the wire")
	}
	if !bytes.Contains(wire, []byte("application/x-custom")) {
		t.Error("unmapped type string must appear on the wire")
	}
	if fmsg.ApplyCommonTypes(h) {
		t.Error("second application must be a no-op")
	}
}
