package fmsg_test

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"testing"

	"github.com/markmnl/fmsgd/pkg/fmsg"
)

// ── Flag constants ────────────────────────────────────────────────────────────

func TestFlagConstants(t *testing.T) {
	tests := []struct {
		name string
		got  uint8
		want uint8
	}{
		{"FlagHasPid", fmsg.FlagHasPid, 0x01},
		{"FlagHasAddTo", fmsg.FlagHasAddTo, 0x02},
		{"FlagCommonType", fmsg.FlagCommonType, 0x04},
		{"FlagImportant", fmsg.FlagImportant, 0x08},
		{"FlagNoReply", fmsg.FlagNoReply, 0x10},
		{"FlagDeflate", fmsg.FlagDeflate, 0x20},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = 0x%02X, want 0x%02X", tt.name, tt.got, tt.want)
		}
	}
}

// ── Address ───────────────────────────────────────────────────────────────────

func TestAddressToString(t *testing.T) {
	tests := []struct {
		addr fmsg.Address
		want string
	}{
		{fmsg.Address{User: "alice", Domain: "example.com"}, "@alice@example.com"},
		{fmsg.Address{User: "bob", Domain: "b.io"}, "@bob@b.io"},
		{fmsg.Address{User: "x", Domain: "y.z"}, "@x@y.z"},
	}
	for _, tt := range tests {
		if got := tt.addr.ToString(); got != tt.want {
			t.Errorf("ToString() = %q, want %q", got, tt.want)
		}
	}
}

// ── Encode ────────────────────────────────────────────────────────────────────

// buildExpectedWire manually constructs the expected wire bytes for a minimal
// header: version=1, flags=0, from=@alice@example.com, to=[@bob@example.com],
// timestamp=0, topic="hi", type="text/plain", size=12, no attachments.
// This is the ground-truth used by TestHeaderEncode.
func buildExpectedWire() []byte {
	var b bytes.Buffer
	b.WriteByte(1) // version
	b.WriteByte(0) // flags
	from := "@alice@example.com"
	b.WriteByte(byte(len(from)))
	b.WriteString(from)
	b.WriteByte(1) // to count
	to := "@bob@example.com"
	b.WriteByte(byte(len(to)))
	b.WriteString(to)
	_ = binary.Write(&b, binary.LittleEndian, float64(0)) // timestamp
	b.WriteByte(byte(len("hi")))
	b.WriteString("hi")
	b.WriteByte(byte(len("text/plain")))
	b.WriteString("text/plain")
	_ = binary.Write(&b, binary.LittleEndian, uint32(12)) // size
	b.WriteByte(0)                                        // attachment count
	return b.Bytes()
}

func TestHeaderEncode(t *testing.T) {
	h := &fmsg.Header{
		Version:   1,
		Flags:     0,
		From:      fmsg.Address{User: "alice", Domain: "example.com"},
		To:        []fmsg.Address{{User: "bob", Domain: "example.com"}},
		Timestamp: 0,
		Topic:     "hi",
		Type:      "text/plain",
		Size:      12,
	}
	got := h.Encode()
	want := buildExpectedWire()
	if !bytes.Equal(got, want) {
		t.Errorf("Encode():\n got  %x\n want %x", got, want)
	}
}

func TestHeaderEncodeHasPid(t *testing.T) {
	// When FlagHasPid is set: 32 pid bytes written at offset 2; topic absent.
	pid := make([]byte, 32)
	for i := range pid {
		pid[i] = byte(i)
	}
	h := &fmsg.Header{
		Version:   1,
		Flags:     fmsg.FlagHasPid,
		Pid:       pid,
		From:      fmsg.Address{User: "a", Domain: "b.io"},
		To:        []fmsg.Address{{User: "c", Domain: "d.io"}},
		Timestamp: 0,
		Topic:     "should-not-appear",
		Type:      "text/plain",
		Size:      0,
	}
	wire := h.Encode()
	if !bytes.Equal(wire[2:34], pid) {
		t.Error("Encode() with FlagHasPid: pid bytes not at wire[2:34]")
	}
	if bytes.Contains(wire, []byte("should-not-appear")) {
		t.Error("Encode() with FlagHasPid: topic must be absent from wire")
	}
}

func TestHeaderEncodeDeflate(t *testing.T) {
	// When FlagDeflate is set, ExpandedSize uint32 follows Size.
	h := &fmsg.Header{
		Version:      1,
		Flags:        fmsg.FlagDeflate,
		From:         fmsg.Address{User: "a", Domain: "b.io"},
		To:           []fmsg.Address{{User: "c", Domain: "d.io"}},
		Timestamp:    0,
		Topic:        "t",
		Type:         "application/octet-stream",
		Size:         100,
		ExpandedSize: 9999,
	}
	wire := h.Encode()
	// Locate size bytes: offset = 1+1 + 1+7 + 1 + 1+7 + 8 + 1+1 + 1+24 = 55
	// version+flags = 2
	// fromlen+"@a@b.io" = 1+7 = 8; total 10
	// tocount = 1; total 11
	// tolen+"@c@d.io" = 1+7 = 8; total 19
	// timestamp = 8; total 27
	// topiclen+"t" = 1+1 = 2; total 29
	// typelen+"application/octet-stream" = 1+24 = 25; total 54
	// size uint32 = 4 bytes at [54:58]
	// expanded size uint32 = 4 bytes at [58:62]
	var size, expanded uint32
	_ = binary.Read(bytes.NewReader(wire[54:58]), binary.LittleEndian, &size)
	_ = binary.Read(bytes.NewReader(wire[58:62]), binary.LittleEndian, &expanded)
	if size != 100 {
		t.Errorf("Encode() DeflateFlag: Size = %d, want 100", size)
	}
	if expanded != 9999 {
		t.Errorf("Encode() DeflateFlag: ExpandedSize = %d, want 9999", expanded)
	}
}

func TestHeaderEncodeCommonType(t *testing.T) {
	// When FlagCommonType is set, TypeID byte is written instead of type string.
	// "text/csv" is ID 50.
	h := &fmsg.Header{
		Version:   1,
		Flags:     fmsg.FlagCommonType,
		From:      fmsg.Address{User: "a", Domain: "b.io"},
		To:        []fmsg.Address{{User: "c", Domain: "d.io"}},
		Timestamp: 0,
		Topic:     "t",
		TypeID:    50,
		Type:      "text/csv",
		Size:      0,
	}
	wire := h.Encode()
	if bytes.Contains(wire, []byte("text/csv")) {
		t.Error("Encode() with FlagCommonType: type string must not appear in wire")
	}
	// TypeID byte is at offset: 2+8+1+8+8+2 = 29
	// (version+flags) + (fromlen+from) + tocount + (tolen+to) + timestamp + (topiclen+topic)
	if wire[29] != 50 {
		t.Errorf("Encode() with FlagCommonType: type byte at [29] = %d, want 50", wire[29])
	}
}

func TestHeaderEncodeAttachment(t *testing.T) {
	h := &fmsg.Header{
		Version:   1,
		Flags:     0,
		From:      fmsg.Address{User: "a", Domain: "b.io"},
		To:        []fmsg.Address{{User: "c", Domain: "d.io"}},
		Timestamp: 0,
		Topic:     "t",
		Type:      "text/plain",
		Size:      5,
		Attachments: []fmsg.AttachmentHeader{
			{
				Flags:    0,
				Type:     "image/png",
				Filename: "pic.png",
				Size:     1024,
			},
		},
	}
	wire := h.Encode()
	if !bytes.Contains(wire, []byte("pic.png")) {
		t.Error("Encode(): attachment filename not in wire")
	}
	if !bytes.Contains(wire, []byte("image/png")) {
		t.Error("Encode(): attachment type not in wire")
	}
}

// ── GetHeaderHash ─────────────────────────────────────────────────────────────

func TestGetHeaderHash(t *testing.T) {
	h := &fmsg.Header{
		Version:   1,
		Flags:     0,
		From:      fmsg.Address{User: "alice", Domain: "example.com"},
		To:        []fmsg.Address{{User: "bob", Domain: "example.com"}},
		Timestamp: 0,
		Topic:     "hi",
		Type:      "text/plain",
		Size:      12,
	}
	got := h.GetHeaderHash()
	want := sha256.Sum256(buildExpectedWire())
	if !bytes.Equal(got, want[:]) {
		t.Errorf("GetHeaderHash() = %x, want %x", got, want)
	}
}

func TestGetHeaderHashCached(t *testing.T) {
	h := &fmsg.Header{
		Version: 1, Flags: 0,
		From: fmsg.Address{User: "a", Domain: "b.io"},
		To:   []fmsg.Address{{User: "c", Domain: "d.io"}},
		Type: "text/plain", Size: 0,
	}
	h1 := h.GetHeaderHash()
	h2 := h.GetHeaderHash()
	if &h1[0] != &h2[0] {
		t.Error("GetHeaderHash() should return the same slice on repeated calls (cached)")
	}
}

// ── GetMessageHash ────────────────────────────────────────────────────────────

// TestGetMessageHashSmall verifies a complete small-message hash against an
// independently computed expected value: sha256(encoded_header || body).
func TestGetMessageHashSmall(t *testing.T) {
	const body = "Hello, fmsg!"

	f, err := os.CreateTemp(t.TempDir(), "fmsg-body-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
	f.Close()

	h := &fmsg.Header{
		Version:   1,
		Flags:     0,
		From:      fmsg.Address{User: "alice", Domain: "example.com"},
		To:        []fmsg.Address{{User: "bob", Domain: "example.com"}},
		Timestamp: 0,
		Topic:     "hi",
		Type:      "text/plain",
		Size:      uint32(len(body)),
		Filepath:  f.Name(),
	}

	got, err := h.GetMessageHash()
	if err != nil {
		t.Fatalf("GetMessageHash() error: %v", err)
	}

	// Expected: sha256(header_wire || body_bytes) computed independently.
	wantHash := sha256.New()
	wantHash.Write(buildExpectedWireWithSize(uint32(len(body))))
	wantHash.Write([]byte(body))
	want := wantHash.Sum(nil)

	if !bytes.Equal(got, want) {
		t.Errorf("GetMessageHash():\n got  %s\n want %s", hex.EncodeToString(got), hex.EncodeToString(want))
	}

	// Golden hash guards against regressions in Encode() or the hash algorithm.
	const wantGolden = "eaefdd1cf1868078ff38ba882cbd31a2297af3db33cec888bd3e088bafdbcc3b"
	if hex.EncodeToString(got) != wantGolden {
		t.Errorf("GetMessageHash() golden:\n got  %s\n want %s", hex.EncodeToString(got), wantGolden)
	}
}

// buildExpectedWireWithSize is like buildExpectedWire but uses a variable body size.
func buildExpectedWireWithSize(size uint32) []byte {
	var b bytes.Buffer
	b.WriteByte(1) // version
	b.WriteByte(0) // flags
	from := "@alice@example.com"
	b.WriteByte(byte(len(from)))
	b.WriteString(from)
	b.WriteByte(1)
	to := "@bob@example.com"
	b.WriteByte(byte(len(to)))
	b.WriteString(to)
	_ = binary.Write(&b, binary.LittleEndian, float64(0))
	b.WriteByte(byte(len("hi")))
	b.WriteString("hi")
	b.WriteByte(byte(len("text/plain")))
	b.WriteString("text/plain")
	_ = binary.Write(&b, binary.LittleEndian, size)
	b.WriteByte(0)
	return b.Bytes()
}

func TestGetMessageHashCached(t *testing.T) {
	const body = "cached"
	f, err := os.CreateTemp(t.TempDir(), "fmsg-body-*")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(body)
	f.Close()

	h := &fmsg.Header{
		Version:  1,
		From:     fmsg.Address{User: "a", Domain: "b.io"},
		To:       []fmsg.Address{{User: "c", Domain: "d.io"}},
		Type:     "text/plain",
		Size:     uint32(len(body)),
		Filepath: f.Name(),
	}
	h1, err := h.GetMessageHash()
	if err != nil {
		t.Fatal(err)
	}
	h2, err := h.GetMessageHash()
	if err != nil {
		t.Fatal(err)
	}
	if &h1[0] != &h2[0] {
		t.Error("GetMessageHash() should return the same slice on repeated calls (cached)")
	}
}

func TestGetMessageHashWithAttachment(t *testing.T) {
	const body = "body data"
	const attData = "attachment data"

	bodyFile, err := os.CreateTemp(t.TempDir(), "fmsg-body-*")
	if err != nil {
		t.Fatal(err)
	}
	bodyFile.WriteString(body)
	bodyFile.Close()

	attFile, err := os.CreateTemp(t.TempDir(), "fmsg-att-*")
	if err != nil {
		t.Fatal(err)
	}
	attFile.WriteString(attData)
	attFile.Close()

	h := &fmsg.Header{
		Version:  1,
		From:     fmsg.Address{User: "a", Domain: "b.io"},
		To:       []fmsg.Address{{User: "c", Domain: "d.io"}},
		Type:     "text/plain",
		Size:     uint32(len(body)),
		Filepath: bodyFile.Name(),
		Attachments: []fmsg.AttachmentHeader{
			{
				Flags:    0,
				Type:     "image/png",
				Filename: "pic.png",
				Size:     uint32(len(attData)),
				Filepath: attFile.Name(),
			},
		},
	}
	got, err := h.GetMessageHash()
	if err != nil {
		t.Fatalf("GetMessageHash() with attachment error: %v", err)
	}

	// Expected: sha256(header_wire || body || attachment_data)
	hw := sha256.New()
	hw.Write(h.Encode())
	hw.Write([]byte(body))
	hw.Write([]byte(attData))
	want := hw.Sum(nil)

	if !bytes.Equal(got, want) {
		t.Errorf("GetMessageHash() with attachment:\n got  %x\n want %x", got, want)
	}
}

// ── HashPayload ───────────────────────────────────────────────────────────────

func TestHashPayloadPlain(t *testing.T) {
	content := []byte("payload bytes")
	f, err := os.CreateTemp(t.TempDir(), "fmsg-payload-*")
	if err != nil {
		t.Fatal(err)
	}
	f.Write(content)
	f.Close()

	var dst bytes.Buffer
	if err := fmsg.HashPayload(&dst, f.Name(), int64(len(content)), false, 0); err != nil {
		t.Fatalf("HashPayload() error: %v", err)
	}
	if !bytes.Equal(dst.Bytes(), content) {
		t.Errorf("HashPayload() wrote %x, want %x", dst.Bytes(), content)
	}
}

func TestHashPayloadDeflated(t *testing.T) {
	plain := []byte("hello compressed world")

	// Write zlib-compressed content to a temp file.
	f, err := os.CreateTemp(t.TempDir(), "fmsg-deflated-*")
	if err != nil {
		t.Fatal(err)
	}
	zw := zlib.NewWriter(f)
	zw.Write(plain)
	zw.Close()
	wireSize, _ := f.Seek(0, 1) // current offset = compressed size
	f.Close()

	var dst bytes.Buffer
	if err := fmsg.HashPayload(&dst, f.Name(), wireSize, true, uint32(len(plain))); err != nil {
		t.Fatalf("HashPayload() deflated error: %v", err)
	}
	if !bytes.Equal(dst.Bytes(), plain) {
		t.Errorf("HashPayload() deflated wrote %q, want %q", dst.Bytes(), plain)
	}
}

func TestHashPayloadDeflatedSizeMismatch(t *testing.T) {
	plain := []byte("data")
	f, err := os.CreateTemp(t.TempDir(), "fmsg-deflated-*")
	if err != nil {
		t.Fatal(err)
	}
	zw := zlib.NewWriter(f)
	zw.Write(plain)
	zw.Close()
	wireSize, _ := f.Seek(0, 1)
	f.Close()

	var dst bytes.Buffer
	err = fmsg.HashPayload(&dst, f.Name(), wireSize, true, uint32(len(plain))+99)
	if err == nil {
		t.Error("HashPayload() should error when expanded size does not match")
	}
}

// ── Common media types ────────────────────────────────────────────────────────

func TestGetCommonMediaType(t *testing.T) {
	tests := []struct {
		id   uint8
		want string
	}{
		{3, "application/json"},
		{6, "application/pdf"},
		{38, "image/png"},
		{50, "text/csv"},
		{56, "text/plain;charset=UTF-8"},
		{64, "video/webm"},
	}
	for _, tt := range tests {
		got, ok := fmsg.GetCommonMediaType(tt.id)
		if !ok {
			t.Errorf("GetCommonMediaType(%d): not found", tt.id)
		}
		if got != tt.want {
			t.Errorf("GetCommonMediaType(%d) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

func TestGetCommonMediaTypeUnknown(t *testing.T) {
	_, ok := fmsg.GetCommonMediaType(0)
	if ok {
		t.Error("GetCommonMediaType(0) should return false")
	}
	_, ok = fmsg.GetCommonMediaType(65)
	if ok {
		t.Error("GetCommonMediaType(65) should return false")
	}
}

func TestGetCommonMediaTypeID(t *testing.T) {
	tests := []struct {
		mime string
		want uint8
	}{
		{"application/json", 3},
		{"application/pdf", 6},
		{"image/png", 38},
		{"text/csv", 50},
		{"text/plain;charset=UTF-8", 56},
		{"video/webm", 64},
	}
	for _, tt := range tests {
		got, ok := fmsg.GetCommonMediaTypeID(tt.mime)
		if !ok {
			t.Errorf("GetCommonMediaTypeID(%q): not found", tt.mime)
		}
		if got != tt.want {
			t.Errorf("GetCommonMediaTypeID(%q) = %d, want %d", tt.mime, got, tt.want)
		}
	}
}

func TestGetCommonMediaTypeIDUnknown(t *testing.T) {
	_, ok := fmsg.GetCommonMediaTypeID("application/unknown")
	if ok {
		t.Error("GetCommonMediaTypeID(unknown) should return false")
	}
}

func TestCommonMediaTypeRoundTrip(t *testing.T) {
	// Every ID in the valid range 1–64 must round-trip: ID → string → ID.
	for id := uint8(1); id <= 64; id++ {
		mime, ok := fmsg.GetCommonMediaType(id)
		if !ok {
			t.Errorf("GetCommonMediaType(%d): not found", id)
			continue
		}
		got, ok := fmsg.GetCommonMediaTypeID(mime)
		if !ok {
			t.Errorf("GetCommonMediaTypeID(%q): not found (from ID %d)", mime, id)
			continue
		}
		if got != id {
			t.Errorf("round-trip ID %d → %q → %d", id, mime, got)
		}
	}
}
