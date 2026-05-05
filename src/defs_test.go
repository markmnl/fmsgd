package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestAddressToString(t *testing.T) {
	tests := []struct {
		addr FMsgAddress
		want string
	}{
		{FMsgAddress{User: "alice", Domain: "example.com"}, "@alice@example.com"},
		{FMsgAddress{User: "Bob", Domain: "EXAMPLE.COM"}, "@Bob@EXAMPLE.COM"},
		{FMsgAddress{User: "a-b.c", Domain: "x.y.z"}, "@a-b.c@x.y.z"},
	}
	for _, tt := range tests {
		got := tt.addr.ToString()
		if got != tt.want {
			t.Errorf("FMsgAddress{%q, %q}.ToString() = %q, want %q", tt.addr.User, tt.addr.Domain, got, tt.want)
		}
	}
}

func TestEncodeMinimalHeader(t *testing.T) {
	h := &FMsgHeader{
		Version:   1,
		Flags:     0,
		From:      FMsgAddress{User: "alice", Domain: "a.com"},
		To:        []FMsgAddress{{User: "bob", Domain: "b.com"}},
		Timestamp: 1700000000.0,
		Topic:     "hello",
		Type:      "text/plain",
	}
	b := h.Encode()

	r := bytes.NewReader(b)

	// version
	ver, _ := r.ReadByte()
	if ver != 1 {
		t.Fatalf("version = %d, want 1", ver)
	}

	// flags
	flags, _ := r.ReadByte()
	if flags != 0 {
		t.Fatalf("flags = %d, want 0", flags)
	}

	// from address
	fromLen, _ := r.ReadByte()
	fromBytes := make([]byte, fromLen)
	r.Read(fromBytes)
	if string(fromBytes) != "@alice@a.com" {
		t.Fatalf("from = %q, want %q", string(fromBytes), "@alice@a.com")
	}

	// to count
	toCount, _ := r.ReadByte()
	if toCount != 1 {
		t.Fatalf("to count = %d, want 1", toCount)
	}

	// to[0]
	toLen, _ := r.ReadByte()
	toBytes := make([]byte, toLen)
	r.Read(toBytes)
	if string(toBytes) != "@bob@b.com" {
		t.Fatalf("to[0] = %q, want %q", string(toBytes), "@bob@b.com")
	}

	// timestamp
	var ts float64
	binary.Read(r, binary.LittleEndian, &ts)
	if ts != 1700000000.0 {
		t.Fatalf("timestamp = %f, want 1700000000.0", ts)
	}

	// topic
	topicLen, _ := r.ReadByte()
	topicBytes := make([]byte, topicLen)
	r.Read(topicBytes)
	if string(topicBytes) != "hello" {
		t.Fatalf("topic = %q, want %q", string(topicBytes), "hello")
	}

	// type
	typeLen, _ := r.ReadByte()
	typeBytes := make([]byte, typeLen)
	r.Read(typeBytes)
	if string(typeBytes) != "text/plain" {
		t.Fatalf("type = %q, want %q", string(typeBytes), "text/plain")
	}

	// size (uint32 LE)
	var size uint32
	binary.Read(r, binary.LittleEndian, &size)
	if size != 0 {
		t.Fatalf("size = %d, want 0", size)
	}

	// attachment count
	attachCount, _ := r.ReadByte()
	if attachCount != 0 {
		t.Fatalf("attach count = %d, want 0", attachCount)
	}

	// should have consumed entire buffer
	if r.Len() != 0 {
		t.Fatalf("unexpected %d trailing bytes", r.Len())
	}
}

func TestEncodeWithPid(t *testing.T) {
	pid := make([]byte, 32)
	for i := range pid {
		pid[i] = byte(i)
	}
	h := &FMsgHeader{
		Version:   1,
		Flags:     FlagHasPid,
		Pid:       pid,
		From:      FMsgAddress{User: "a", Domain: "b.com"},
		To:        []FMsgAddress{{User: "c", Domain: "d.com"}},
		Timestamp: 0,
		Topic:     "should be omitted",
		Type:      "text/plain",
	}
	b := h.Encode()
	r := bytes.NewReader(b)

	r.ReadByte() // version
	r.ReadByte() // flags

	// pid should be next 32 bytes
	pidOut := make([]byte, 32)
	n, _ := r.Read(pidOut)
	if n != 32 {
		t.Fatalf("pid bytes read = %d, want 32", n)
	}
	if !bytes.Equal(pidOut, pid) {
		t.Fatalf("pid mismatch")
	}

	// skip from
	fLen, _ := r.ReadByte()
	fBuf := make([]byte, fLen)
	r.Read(fBuf)

	// skip to count + to[0]
	toCount, _ := r.ReadByte()
	if toCount != 1 {
		t.Fatalf("to count = %d, want 1", toCount)
	}
	tLen, _ := r.ReadByte()
	tBuf := make([]byte, tLen)
	r.Read(tBuf)

	// skip timestamp
	var ts float64
	binary.Read(r, binary.LittleEndian, &ts)

	// topic must NOT be present when pid is set — next byte should be type length
	typeLen, _ := r.ReadByte()
	typeBytes := make([]byte, typeLen)
	r.Read(typeBytes)
	if string(typeBytes) != "text/plain" {
		t.Fatalf("expected type field directly after timestamp, got %q", string(typeBytes))
	}

	// size + attachment count
	var size uint32
	binary.Read(r, binary.LittleEndian, &size)
	attachCount, _ := r.ReadByte()
	if attachCount != 0 {
		t.Fatalf("attach count = %d, want 0", attachCount)
	}

	if r.Len() != 0 {
		t.Fatalf("unexpected %d trailing bytes", r.Len())
	}
}

func TestEncodeWithAddTo(t *testing.T) {
	pid := make([]byte, 32)
	h := &FMsgHeader{
		Version:   1,
		Flags:     FlagHasPid | FlagHasAddTo,
		Pid:       pid,
		From:      FMsgAddress{User: "a", Domain: "b.com"},
		To:        []FMsgAddress{{User: "c", Domain: "d.com"}},
		AddToFrom: &FMsgAddress{User: "a", Domain: "b.com"},
		AddTo:     []FMsgAddress{{User: "e", Domain: "f.com"}},
		Timestamp: 0,
		Topic:     "",
		Type:      "text/plain",
	}
	b := h.Encode()
	r := bytes.NewReader(b)

	r.ReadByte() // version
	r.ReadByte() // flags

	// skip pid (32 bytes)
	pidBuf := make([]byte, 32)
	r.Read(pidBuf)

	// skip from
	fLen, _ := r.ReadByte()
	fBuf := make([]byte, fLen)
	r.Read(fBuf)

	// skip to count + to[0]
	toCount, _ := r.ReadByte()
	if toCount != 1 {
		t.Fatalf("to count = %d, want 1", toCount)
	}
	tLen, _ := r.ReadByte()
	tBuf := make([]byte, tLen)
	r.Read(tBuf)

	// add-to-from
	addToFromLen, _ := r.ReadByte()
	addToFrom := make([]byte, addToFromLen)
	r.Read(addToFrom)
	if string(addToFrom) != "@a@b.com" {
		t.Fatalf("add-to-from = %q, want %q", string(addToFrom), "@a@b.com")
	}

	// add to count
	addToCount, _ := r.ReadByte()
	if addToCount != 1 {
		t.Fatalf("add to count = %d, want 1", addToCount)
	}

	// add to[0]
	atLen, _ := r.ReadByte()
	atBuf := make([]byte, atLen)
	r.Read(atBuf)
	if string(atBuf) != "@e@f.com" {
		t.Fatalf("add to[0] = %q, want %q", string(atBuf), "@e@f.com")
	}
}

func TestEncodeWithAddToDefaultsAddToFromToFromAddress(t *testing.T) {
	pid := make([]byte, 32)
	h := &FMsgHeader{
		Version:   1,
		Flags:     FlagHasPid | FlagHasAddTo,
		Pid:       pid,
		From:      FMsgAddress{User: "a", Domain: "b.com"},
		To:        []FMsgAddress{{User: "c", Domain: "d.com"}},
		AddTo:     []FMsgAddress{{User: "e", Domain: "f.com"}},
		Timestamp: 0,
		Type:      "text/plain",
	}
	b := h.Encode()
	r := bytes.NewReader(b)

	r.ReadByte() // version
	r.ReadByte() // flags
	r.Read(make([]byte, 32))
	fLen, _ := r.ReadByte()
	r.Read(make([]byte, fLen))
	r.ReadByte() // to count
	tLen, _ := r.ReadByte()
	r.Read(make([]byte, tLen))

	addToFromLen, _ := r.ReadByte()
	addToFrom := make([]byte, addToFromLen)
	r.Read(addToFrom)
	if string(addToFrom) != "@a@b.com" {
		t.Fatalf("default add-to-from = %q, want %q", string(addToFrom), "@a@b.com")
	}
}

func TestEncodeNoAddToWhenFlagUnset(t *testing.T) {
	// When FlagHasAddTo is NOT set, add-to addresses should not appear on the wire
	// even if the AddTo slice is populated.
	h := &FMsgHeader{
		Version:   1,
		Flags:     0,
		From:      FMsgAddress{User: "a", Domain: "b.com"},
		To:        []FMsgAddress{{User: "c", Domain: "d.com"}},
		AddTo:     []FMsgAddress{{User: "e", Domain: "f.com"}}, // should be ignored
		Timestamp: 0,
		Topic:     "",
		Type:      "text/plain",
	}
	withAddTo := h.Encode()

	h2 := &FMsgHeader{
		Version:   1,
		Flags:     0,
		From:      FMsgAddress{User: "a", Domain: "b.com"},
		To:        []FMsgAddress{{User: "c", Domain: "d.com"}},
		Timestamp: 0,
		Topic:     "",
		Type:      "text/plain",
	}
	withoutAddTo := h2.Encode()

	if !bytes.Equal(withAddTo, withoutAddTo) {
		t.Fatalf("encoded bytes differ when AddTo populated but flag unset")
	}
}

func TestGetHeaderHash(t *testing.T) {
	h := &FMsgHeader{
		Version:   1,
		Flags:     0,
		From:      FMsgAddress{User: "alice", Domain: "a.com"},
		To:        []FMsgAddress{{User: "bob", Domain: "b.com"}},
		Timestamp: 1700000000.0,
		Topic:     "test",
		Type:      "text/plain",
	}
	hash := h.GetHeaderHash()
	if len(hash) != 32 {
		t.Fatalf("hash length = %d, want 32", len(hash))
	}

	// Must be deterministic
	hash2 := h.GetHeaderHash()
	if !bytes.Equal(hash, hash2) {
		t.Fatal("GetHeaderHash not deterministic")
	}

	// Must match manual SHA-256 of Encode()
	expected := sha256.Sum256(h.Encode())
	if !bytes.Equal(hash, expected[:]) {
		t.Fatal("GetHeaderHash does not match sha256(Encode())")
	}
}

func TestGetHeaderHashCommonTypeMatchesWireIDEncoding(t *testing.T) {
	h := &FMsgHeader{
		Version:   1,
		Flags:     FlagCommonType,
		From:      FMsgAddress{User: "alice", Domain: "a.com"},
		To:        []FMsgAddress{{User: "bob", Domain: "b.com"}},
		Timestamp: 1700000000,
		Topic:     "x",
		TypeID:    3,
		Type:      "application/json",
	}
	expected := sha256.Sum256(h.Encode())
	got := h.GetHeaderHash()
	if !bytes.Equal(got, expected[:]) {
		t.Fatalf("GetHeaderHash mismatch for common type ID")
	}
}

func TestStringOutput(t *testing.T) {
	h := &FMsgHeader{
		Version:   1,
		Flags:     0,
		From:      FMsgAddress{User: "alice", Domain: "a.com"},
		To:        []FMsgAddress{{User: "bob", Domain: "b.com"}, {User: "carol", Domain: "c.com"}},
		Timestamp: 0,
		Topic:     "greetings",
		Type:      "text/plain",
		Size:      42,
	}
	s := h.String()

	// Check key substrings are present
	for _, want := range []string{
		"v1",
		"@alice@a.com",
		"@bob@b.com",
		"@carol@c.com",
		"greetings",
		"text/plain",
		"42",
	} {
		if !bytes.Contains([]byte(s), []byte(want)) {
			t.Errorf("String() missing %q", want)
		}
	}
}

func TestStringWithAddTo(t *testing.T) {
	h := &FMsgHeader{
		Version: 1,
		Flags:   FlagHasAddTo,
		From:    FMsgAddress{User: "alice", Domain: "a.com"},
		To:      []FMsgAddress{{User: "bob", Domain: "b.com"}},
		AddTo:   []FMsgAddress{{User: "dave", Domain: "d.com"}},
		Topic:   "t",
		Type:    "text/plain",
	}
	s := h.String()
	if !bytes.Contains([]byte(s), []byte("add to:")) {
		t.Error("String() missing 'add to:' label")
	}
	if !bytes.Contains([]byte(s), []byte("@dave@d.com")) {
		t.Error("String() missing add-to address")
	}
}

func TestEncodeTimestampEncoding(t *testing.T) {
	// Verify the timestamp is encoded as little-endian float64
	h := &FMsgHeader{
		Version:   1,
		Flags:     0,
		From:      FMsgAddress{User: "a", Domain: "b.com"},
		To:        []FMsgAddress{{User: "c", Domain: "d.com"}},
		Timestamp: 1700000000.5,
		Topic:     "",
		Type:      "",
	}
	b := h.Encode()

	// Find timestamp position: version(1) + flags(1) + from(1+len) + to_count(1) + to[0](1+len)
	fromStr := "@a@b.com"
	toStr := "@c@d.com"
	offset := 1 + 1 + 1 + len(fromStr) + 1 + 1 + len(toStr) // = 2 + 9 + 10 = 21
	tsBytes := b[offset : offset+8]

	bits := binary.LittleEndian.Uint64(tsBytes)
	ts := math.Float64frombits(bits)
	if ts != 1700000000.5 {
		t.Fatalf("timestamp = %f, want 1700000000.5", ts)
	}

	// After timestamp: topic(1+0) + type(1+0) + size(4) + attach_count(1) = 7 bytes
	if r := bytes.NewReader(b[offset+8:]); r.Len() != 7 {
		t.Fatalf("trailing bytes after timestamp = %d, want 7", r.Len())
	}
}

func TestEncodeWithAttachments(t *testing.T) {
	h := &FMsgHeader{
		Version:   1,
		Flags:     0,
		From:      FMsgAddress{User: "a", Domain: "b.com"},
		To:        []FMsgAddress{{User: "c", Domain: "d.com"}},
		Timestamp: 0,
		Topic:     "",
		Type:      "text/plain",
		Size:      100,
		Attachments: []FMsgAttachmentHeader{
			{Flags: 0, Type: "image/png", Filename: "pic.png", Size: 2048},
			{Flags: 1, TypeID: 38, Type: "image/png", Filename: "doc.txt", Size: 512},
		},
	}
	b := h.Encode()
	r := bytes.NewReader(b)

	r.ReadByte() // version
	r.ReadByte() // flags

	// skip from
	fLen, _ := r.ReadByte()
	r.Read(make([]byte, fLen))
	// skip to count + to[0]
	r.ReadByte()
	tLen, _ := r.ReadByte()
	r.Read(make([]byte, tLen))
	// skip timestamp
	var ts float64
	binary.Read(r, binary.LittleEndian, &ts)
	// skip topic
	topicLen, _ := r.ReadByte()
	r.Read(make([]byte, topicLen))
	// skip type
	typeLen, _ := r.ReadByte()
	r.Read(make([]byte, typeLen))

	// size
	var size uint32
	binary.Read(r, binary.LittleEndian, &size)
	if size != 100 {
		t.Fatalf("size = %d, want 100", size)
	}

	// attachment count
	attachCount, _ := r.ReadByte()
	if attachCount != 2 {
		t.Fatalf("attach count = %d, want 2", attachCount)
	}

	// attachment 0
	att0Flags, _ := r.ReadByte()
	if att0Flags != 0 {
		t.Fatalf("att[0] flags = %d, want 0", att0Flags)
	}
	att0TypeLen, _ := r.ReadByte()
	att0Type := make([]byte, att0TypeLen)
	r.Read(att0Type)
	if string(att0Type) != "image/png" {
		t.Fatalf("att[0] type = %q, want %q", string(att0Type), "image/png")
	}
	att0FnLen, _ := r.ReadByte()
	att0Fn := make([]byte, att0FnLen)
	r.Read(att0Fn)
	if string(att0Fn) != "pic.png" {
		t.Fatalf("att[0] filename = %q, want %q", string(att0Fn), "pic.png")
	}
	var att0Size uint32
	binary.Read(r, binary.LittleEndian, &att0Size)
	if att0Size != 2048 {
		t.Fatalf("att[0] size = %d, want 2048", att0Size)
	}

	// attachment 1
	att1Flags, _ := r.ReadByte()
	if att1Flags != 1 {
		t.Fatalf("att[1] flags = %d, want 1", att1Flags)
	}
	att1TypeID, _ := r.ReadByte()
	if att1TypeID != 38 {
		t.Fatalf("att[1] type ID = %d, want 38", att1TypeID)
	}
	att1FnLen, _ := r.ReadByte()
	att1Fn := make([]byte, att1FnLen)
	r.Read(att1Fn)
	if string(att1Fn) != "doc.txt" {
		t.Fatalf("att[1] filename = %q, want %q", string(att1Fn), "doc.txt")
	}
	var att1Size uint32
	binary.Read(r, binary.LittleEndian, &att1Size)
	if att1Size != 512 {
		t.Fatalf("att[1] size = %d, want 512", att1Size)
	}

	if r.Len() != 0 {
		t.Fatalf("unexpected %d trailing bytes", r.Len())
	}
}

func TestEncodeWithCommonMessageType(t *testing.T) {
	h := &FMsgHeader{
		Version:   1,
		Flags:     FlagCommonType,
		From:      FMsgAddress{User: "a", Domain: "b.com"},
		To:        []FMsgAddress{{User: "c", Domain: "d.com"}},
		Timestamp: 0,
		Topic:     "",
		TypeID:    3,
		Type:      "application/json",
	}
	b := h.Encode()
	r := bytes.NewReader(b)

	r.ReadByte() // version
	r.ReadByte() // flags

	// skip from
	fLen, _ := r.ReadByte()
	r.Read(make([]byte, fLen))
	// skip to count + to[0]
	r.ReadByte()
	tLen, _ := r.ReadByte()
	r.Read(make([]byte, tLen))
	// skip timestamp
	var ts float64
	binary.Read(r, binary.LittleEndian, &ts)
	// skip topic
	topicLen, _ := r.ReadByte()
	r.Read(make([]byte, topicLen))

	typeID, _ := r.ReadByte()
	if typeID != 3 {
		t.Fatalf("type ID = %d, want 3", typeID)
	}
}

func TestGetMessageHashUsesDecompressedPayloads(t *testing.T) {
	compress := func(data []byte) []byte {
		var b bytes.Buffer
		w := zlib.NewWriter(&b)
		if _, err := w.Write(data); err != nil {
			t.Fatalf("zlib write: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("zlib close: %v", err)
		}
		return b.Bytes()
	}

	msgPlain := []byte("hello compressed body")
	attPlain := []byte("hello compressed attachment")
	msgWire := compress(msgPlain)
	attWire := compress(attPlain)

	tmpDir := t.TempDir()
	msgPath := filepath.Join(tmpDir, "msg.bin")
	if err := os.WriteFile(msgPath, msgWire, 0600); err != nil {
		t.Fatalf("write msg file: %v", err)
	}
	attPath := filepath.Join(tmpDir, "att.bin")
	if err := os.WriteFile(attPath, attWire, 0600); err != nil {
		t.Fatalf("write attachment file: %v", err)
	}

	h := &FMsgHeader{
		Version:      1,
		Flags:        FlagDeflate,
		From:         FMsgAddress{User: "alice", Domain: "a.com"},
		To:           []FMsgAddress{{User: "bob", Domain: "b.com"}},
		Timestamp:    1700000000,
		Topic:        "t",
		Type:         "text/plain",
		Size:         uint32(len(msgWire)),
		ExpandedSize: uint32(len(msgPlain)),
		Attachments: []FMsgAttachmentHeader{
			{Flags: 1 << 1, Type: "application/octet-stream", Filename: "a.bin", Size: uint32(len(attWire)), ExpandedSize: uint32(len(attPlain)), Filepath: attPath},
		},
		Filepath: msgPath,
	}

	got, err := h.GetMessageHash()
	if err != nil {
		t.Fatalf("GetMessageHash() error: %v", err)
	}

	manual := sha256.New()
	if _, err := io.Copy(manual, bytes.NewReader(h.Encode())); err != nil {
		t.Fatalf("manual header copy: %v", err)
	}
	if _, err := manual.Write(msgPlain); err != nil {
		t.Fatalf("manual msg write: %v", err)
	}
	if _, err := manual.Write(attPlain); err != nil {
		t.Fatalf("manual att write: %v", err)
	}
	want := manual.Sum(nil)

	if !bytes.Equal(got, want) {
		t.Fatalf("message hash mismatch: got %x want %x", got, want)
	}
}

func TestEncodeExpandedSizePresentWhenDeflateSet(t *testing.T) {
	h := &FMsgHeader{
		Version:      1,
		Flags:        FlagDeflate,
		From:         FMsgAddress{User: "a", Domain: "b.com"},
		To:           []FMsgAddress{{User: "c", Domain: "d.com"}},
		Timestamp:    0,
		Topic:        "",
		Type:         "text/plain",
		Size:         50,
		ExpandedSize: 200,
	}
	b := h.Encode()
	r := bytes.NewReader(b)

	r.ReadByte() // version
	r.ReadByte() // flags

	// skip from
	fLen, _ := r.ReadByte()
	r.Read(make([]byte, fLen))
	// skip to count + to[0]
	r.ReadByte()
	tLen, _ := r.ReadByte()
	r.Read(make([]byte, tLen))
	// skip timestamp
	var ts float64
	binary.Read(r, binary.LittleEndian, &ts)
	// skip topic
	topicLen, _ := r.ReadByte()
	r.Read(make([]byte, topicLen))
	// skip type
	typeLen, _ := r.ReadByte()
	r.Read(make([]byte, typeLen))

	// size
	var size uint32
	binary.Read(r, binary.LittleEndian, &size)
	if size != 50 {
		t.Fatalf("size = %d, want 50", size)
	}

	// expanded size must be present because FlagDeflate is set
	var expandedSize uint32
	if err := binary.Read(r, binary.LittleEndian, &expandedSize); err != nil {
		t.Fatalf("reading expanded size: %v", err)
	}
	if expandedSize != 200 {
		t.Fatalf("expanded size = %d, want 200", expandedSize)
	}

	// attachment count
	attachCount, _ := r.ReadByte()
	if attachCount != 0 {
		t.Fatalf("attach count = %d, want 0", attachCount)
	}

	if r.Len() != 0 {
		t.Fatalf("unexpected %d trailing bytes", r.Len())
	}
}

func TestEncodeNoExpandedSizeWhenDeflateUnset(t *testing.T) {
	h := &FMsgHeader{
		Version:      1,
		Flags:        0,
		From:         FMsgAddress{User: "a", Domain: "b.com"},
		To:           []FMsgAddress{{User: "c", Domain: "d.com"}},
		Timestamp:    0,
		Topic:        "",
		Type:         "text/plain",
		Size:         100,
		ExpandedSize: 999, // must NOT appear on wire
	}
	b := h.Encode()
	r := bytes.NewReader(b)

	r.ReadByte() // version
	r.ReadByte() // flags

	fLen, _ := r.ReadByte()
	r.Read(make([]byte, fLen))
	r.ReadByte()
	tLen, _ := r.ReadByte()
	r.Read(make([]byte, tLen))
	var ts float64
	binary.Read(r, binary.LittleEndian, &ts)
	topicLen, _ := r.ReadByte()
	r.Read(make([]byte, topicLen))
	typeLen, _ := r.ReadByte()
	r.Read(make([]byte, typeLen))

	var size uint32
	binary.Read(r, binary.LittleEndian, &size)
	if size != 100 {
		t.Fatalf("size = %d, want 100", size)
	}

	// No expanded size field; next byte should be attachment count = 0
	attachCount, _ := r.ReadByte()
	if attachCount != 0 {
		t.Fatalf("attach count = %d, want 0", attachCount)
	}

	if r.Len() != 0 {
		t.Fatalf("unexpected %d trailing bytes (expanded size should not be present when deflate unset)", r.Len())
	}
}

func TestEncodeAttachmentExpandedSizePresentWhenDeflateSet(t *testing.T) {
	h := &FMsgHeader{
		Version:   1,
		Flags:     0,
		From:      FMsgAddress{User: "a", Domain: "b.com"},
		To:        []FMsgAddress{{User: "c", Domain: "d.com"}},
		Timestamp: 0,
		Topic:     "",
		Type:      "text/plain",
		Size:      0,
		Attachments: []FMsgAttachmentHeader{
			// attachment with zlib-deflate flag (bit 1 = 0b00000010)
			{Flags: 1 << 1, Type: "text/plain", Filename: "doc.txt", Size: 60, ExpandedSize: 300},
		},
	}
	b := h.Encode()
	r := bytes.NewReader(b)

	r.ReadByte() // version
	r.ReadByte() // flags
	fLen, _ := r.ReadByte()
	r.Read(make([]byte, fLen))
	r.ReadByte()
	tLen, _ := r.ReadByte()
	r.Read(make([]byte, tLen))
	var ts float64
	binary.Read(r, binary.LittleEndian, &ts)
	topicLen, _ := r.ReadByte()
	r.Read(make([]byte, topicLen))
	typeLen, _ := r.ReadByte()
	r.Read(make([]byte, typeLen))
	var msgSize uint32
	binary.Read(r, binary.LittleEndian, &msgSize)

	// attachment count
	attachCount, _ := r.ReadByte()
	if attachCount != 1 {
		t.Fatalf("attach count = %d, want 1", attachCount)
	}

	// attachment flags
	attFlags, _ := r.ReadByte()
	if attFlags != 1<<1 {
		t.Fatalf("att flags = %d, want %d", attFlags, 1<<1)
	}
	// type (length-prefixed)
	attTypeLen, _ := r.ReadByte()
	r.Read(make([]byte, attTypeLen))
	// filename
	attFnLen, _ := r.ReadByte()
	r.Read(make([]byte, attFnLen))
	// wire size
	var attSize uint32
	binary.Read(r, binary.LittleEndian, &attSize)
	if attSize != 60 {
		t.Fatalf("att size = %d, want 60", attSize)
	}
	// expanded size must be present
	var attExpandedSize uint32
	if err := binary.Read(r, binary.LittleEndian, &attExpandedSize); err != nil {
		t.Fatalf("reading att expanded size: %v", err)
	}
	if attExpandedSize != 300 {
		t.Fatalf("att expanded size = %d, want 300", attExpandedSize)
	}

	if r.Len() != 0 {
		t.Fatalf("unexpected %d trailing bytes", r.Len())
	}
}

func TestHashPayloadRejectsExpandedSizeMismatch(t *testing.T) {
	compress := func(data []byte) []byte {
		var b bytes.Buffer
		w := zlib.NewWriter(&b)
		w.Write(data)
		w.Close()
		return b.Bytes()
	}

	plain := []byte("hello world this is test data")
	wire := compress(plain)

	tmpDir := t.TempDir()
	p := filepath.Join(tmpDir, "data.bin")
	if err := os.WriteFile(p, wire, 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// Correct expanded size should succeed
	var dst bytes.Buffer
	if err := hashPayload(&dst, p, int64(len(wire)), true, uint32(len(plain))); err != nil {
		t.Fatalf("hashPayload with correct expanded size: %v", err)
	}

	// Wrong expanded size should fail
	dst.Reset()
	err := hashPayload(&dst, p, int64(len(wire)), true, uint32(len(plain))+1)
	if err == nil {
		t.Fatal("hashPayload with wrong expanded size: expected error, got nil")
	}
}
