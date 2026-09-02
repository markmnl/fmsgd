package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Outgoing headers encode Common Media Type IDs (SPEC §4) where the stored
// type string has one, as FMSG-005 requires for reactions (ID 56).

func commonTypeTestFields(t *testing.T) *msgFields {
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
	return &msgFields{
		version:  1,
		size:     4,
		from:     FMsgAddress{User: "alice", Domain: "example.com"},
		to:       []FMsgAddress{{User: "bob", Domain: "example.org"}},
		timeSent: 1754280000,
		topic:    "types",
		typ:      "text/plain;charset=UTF-8",
		filepath: bodyPath,
		attachments: []FMsgAttachmentHeader{
			{Type: "image/png", Filename: "pic.png", Size: 3, Filepath: attPath},
			{Type: "application/x-custom", Filename: "custom.bin", Size: 3, Filepath: attPath},
		},
	}
}

func TestApplyCommonTypesEncodesIDs(t *testing.T) {
	h := commonTypeTestFields(t).originalHeader()
	if !applyCommonTypes(h) {
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
	if applyCommonTypes(h) {
		t.Error("second application must be a no-op")
	}
}

func TestEncodeForWireUsesCommonTypesForNewMessage(t *testing.T) {
	m := commonTypeTestFields(t)
	h, common, err := encodeForWire(m.originalHeader, deflateState{}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !common || h.Flags&FlagCommonType == 0 {
		t.Errorf("new message should use common type IDs (common=%v flags=%#08b)", common, h.Flags)
	}
	h, common, err = encodeForWire(m.originalHeader, deflateState{}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if common || h.Flags&FlagCommonType != 0 {
		t.Errorf("commonTypes=false must keep string types (common=%v flags=%#08b)", common, h.Flags)
	}
}

// A message whose hash was recorded before this host encoded common type IDs
// keeps its string types, so every delivery reproduces the stored hash.
func TestEncodeForWireKeepsRecordedForm(t *testing.T) {
	m := commonTypeTestFields(t)

	stringForm := m.originalHeader()
	stringHash, err := stringForm.GetMessageHash()
	if err != nil {
		t.Fatal(err)
	}
	h, common, err := encodeForWire(m.originalHeader, deflateState{}, true, stringHash)
	if err != nil {
		t.Fatal(err)
	}
	if common || h.Flags&FlagCommonType != 0 {
		t.Errorf("hash recorded in string form must keep string form (common=%v flags=%#08b)", common, h.Flags)
	}
	got, _ := h.GetMessageHash()
	if !bytes.Equal(got, stringHash) {
		t.Error("string form does not reproduce the recorded hash")
	}

	commonForm := m.originalHeader()
	applyCommonTypes(commonForm)
	commonHash, err := commonForm.GetMessageHash()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(commonHash, stringHash) {
		t.Fatal("forms hash identically; test no longer discriminates")
	}
	h, common, err = encodeForWire(m.originalHeader, deflateState{}, true, commonHash)
	if err != nil {
		t.Fatal(err)
	}
	if !common || h.Flags&FlagCommonType == 0 {
		t.Errorf("hash recorded in common form must keep common form (common=%v flags=%#08b)", common, h.Flags)
	}
}
