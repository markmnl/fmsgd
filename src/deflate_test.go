package main

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"os"
	"strings"
	"testing"
)

// --- shouldDeflate tests ---

func TestShouldDeflate_TextTypes(t *testing.T) {
	compressible := []string{
		"text/plain;charset=UTF-8",
		"text/html",
		"text/markdown",
		"text/csv",
		"text/css",
		"text/javascript",
		"text/calendar",
		"text/vcard",
		"text/plain;charset=US-ASCII",
		"text/plain;charset=UTF-16",
		"application/json",
		"application/xml",
		"application/xhtml+xml",
		"application/rtf",
		"application/x-tar",
		"application/msword",
		"application/vnd.ms-excel",
		"application/vnd.ms-powerpoint",
		"image/svg+xml",
		"audio/midi",
		"model/obj",
		"model/step",
		"model/stl",
	}
	for _, mt := range compressible {
		if !shouldDeflate(mt, 1024) {
			t.Errorf("shouldDeflate(%q, 1024) = false, want true", mt)
		}
	}
}

func TestShouldDeflate_IncompressibleTypes(t *testing.T) {
	skip := []string{
		"image/jpeg",
		"image/png",
		"image/gif",
		"image/webp",
		"image/heic",
		"image/avif",
		"image/apng",
		"audio/aac",
		"audio/mpeg",
		"audio/ogg",
		"audio/opus",
		"audio/webm",
		"video/H264",
		"video/H265",
		"video/H266",
		"video/ogg",
		"video/VP8",
		"video/VP9",
		"video/webm",
		"application/gzip",
		"application/zip",
		"application/epub+zip",
		"application/octet-stream",
		"application/pdf",
		"application/vnd.oasis.opendocument.presentation",
		"application/vnd.oasis.opendocument.spreadsheet",
		"application/vnd.oasis.opendocument.text",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.amazon.ebook",
		"font/woff",
		"font/woff2",
		"model/3mf",
		"model/gltf-binary",
		"model/vnd.usdz+zip",
	}
	for _, mt := range skip {
		if shouldDeflate(mt, 1024) {
			t.Errorf("shouldDeflate(%q, 1024) = true, want false", mt)
		}
	}
}

func TestShouldDeflate_SmallPayload(t *testing.T) {
	sizes := []uint32{0, 1, 100, 511}
	for _, sz := range sizes {
		if shouldDeflate("text/plain;charset=UTF-8", sz) {
			t.Errorf("shouldDeflate(text/plain, %d) = true, want false", sz)
		}
	}
}

func TestShouldDeflate_EdgeCases(t *testing.T) {
	// Exactly at threshold: should attempt
	if !shouldDeflate("text/plain;charset=UTF-8", 512) {
		t.Error("shouldDeflate at threshold 512 should return true")
	}
	// Unknown type: default to try compression
	if !shouldDeflate("application/x-custom", 1024) {
		t.Error("shouldDeflate for unknown type should return true")
	}
	// Type with parameters should match base type
	if shouldDeflate("application/pdf; charset=utf-8", 1024) {
		t.Error("shouldDeflate should strip parameters and match application/pdf")
	}
	// Case insensitive
	if shouldDeflate("VIDEO/H264", 1024) {
		t.Error("shouldDeflate should be case-insensitive")
	}
}

// --- tryDeflate tests ---

func writeTempFile(t *testing.T, data []byte) string {
	t.Helper()
	f, err := os.CreateTemp("", "deflate-test-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

func TestTryDeflate_CompressibleData(t *testing.T) {
	original := []byte(strings.Repeat("hello world, this is compressible text data! ", 100))
	srcPath := writeTempFile(t, original)
	defer os.Remove(srcPath)

	dstPath, cSize, worthwhile, err := tryDeflate(srcPath, uint32(len(original)))
	if err != nil {
		t.Fatal(err)
	}
	if !worthwhile {
		t.Fatal("expected compression to be worthwhile for repetitive text")
	}
	defer os.Remove(dstPath)

	if cSize >= uint32(len(original))*9/10 {
		t.Errorf("compressed size %d not < 90%% of original %d", cSize, len(original))
	}

	// Verify the compressed file decompresses to the original data
	f, err := os.Open(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	zr, err := zlib.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	decompressed, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decompressed, original) {
		t.Error("decompressed data does not match original")
	}
}

func TestTryDeflate_IncompressibleData(t *testing.T) {
	// Random bytes are effectively incompressible
	data := make([]byte, 2048)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	srcPath := writeTempFile(t, data)
	defer os.Remove(srcPath)

	_, _, worthwhile, err := tryDeflate(srcPath, uint32(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if worthwhile {
		t.Error("expected compression of random data to not be worthwhile")
	}
}

func TestTryDeflate_RoundTrip(t *testing.T) {
	original := []byte(strings.Repeat("Round-trip test data with enough repetition to compress well. ", 50))
	srcPath := writeTempFile(t, original)
	defer os.Remove(srcPath)

	dstPath, cSize, worthwhile, err := tryDeflate(srcPath, uint32(len(original)))
	if err != nil {
		t.Fatal(err)
	}
	if !worthwhile {
		t.Fatal("expected compression to be worthwhile")
	}
	defer os.Remove(dstPath)

	// Read compressed file
	compressed, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if uint32(len(compressed)) != cSize {
		t.Errorf("compressed file size %d != reported size %d", len(compressed), cSize)
	}

	// Decompress and verify
	zr, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	decompressed, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decompressed, original) {
		t.Errorf("round-trip mismatch: got %d bytes, want %d bytes", len(decompressed), len(original))
	}
}

func TestTryDeflate_CleanupOnNotWorthwhile(t *testing.T) {
	// Random data won't compress well — the temp file should be removed
	data := make([]byte, 2048)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	srcPath := writeTempFile(t, data)
	defer os.Remove(srcPath)

	dstPath, _, worthwhile, err := tryDeflate(srcPath, uint32(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if worthwhile {
		defer os.Remove(dstPath)
		t.Fatal("expected not worthwhile for random data")
	}
	// dstPath should be empty and no leaked temp file
	if dstPath != "" {
		t.Errorf("expected empty dstPath when not worthwhile, got %q", dstPath)
	}
}

// --- Hash determinism tests ---

func TestGetMessageHash_WithDeflate(t *testing.T) {
	// Create repetitive data that compresses well
	original := []byte(strings.Repeat("deflate hash test data ", 100))
	srcPath := writeTempFile(t, original)
	defer os.Remove(srcPath)

	// Compress it
	dstPath, cSize, worthwhile, err := tryDeflate(srcPath, uint32(len(original)))
	if err != nil {
		t.Fatal(err)
	}
	if !worthwhile {
		t.Fatal("expected compression to be worthwhile")
	}
	defer os.Remove(dstPath)

	// Build header with deflate flag pointing at compressed file
	h := &FMsgHeader{
		Version:  1,
		Flags:    FlagDeflate,
		From:     FMsgAddress{User: "alice", Domain: "example.com"},
		To:       []FMsgAddress{{User: "bob", Domain: "other.com"}},
		Topic:    "test",
		Type:     "text/plain;charset=UTF-8",
		Size:     cSize,
		Filepath: dstPath,
	}

	msgHash, err := h.GetMessageHash()
	if err != nil {
		t.Fatal(err)
	}

	// Manually compute expected: SHA-256(encoded header + decompressed data)
	expected := sha256.New()
	expected.Write(h.Encode())
	expected.Write(original)
	expectedHash := expected.Sum(nil)

	if !bytes.Equal(msgHash, expectedHash) {
		t.Errorf("hash mismatch:\n  got  %x\n  want %x", msgHash, expectedHash)
	}
}

func TestGetMessageHash_WithoutDeflate(t *testing.T) {
	original := []byte(strings.Repeat("no deflate hash test ", 100))
	srcPath := writeTempFile(t, original)
	defer os.Remove(srcPath)

	h := &FMsgHeader{
		Version:  1,
		Flags:    0,
		From:     FMsgAddress{User: "alice", Domain: "example.com"},
		To:       []FMsgAddress{{User: "bob", Domain: "other.com"}},
		Topic:    "test",
		Type:     "text/plain;charset=UTF-8",
		Size:     uint32(len(original)),
		Filepath: srcPath,
	}

	msgHash, err := h.GetMessageHash()
	if err != nil {
		t.Fatal(err)
	}

	expected := sha256.New()
	expected.Write(h.Encode())
	expected.Write(original)
	expectedHash := expected.Sum(nil)

	if !bytes.Equal(msgHash, expectedHash) {
		t.Errorf("hash mismatch:\n  got  %x\n  want %x", msgHash, expectedHash)
	}
}

func TestGetMessageHash_DeflateChangesHash(t *testing.T) {
	// The same data produces different message hashes depending on whether
	// it is deflated, because the header bytes differ (flags and size fields).
	original := []byte(strings.Repeat("deflate vs plain ", 100))
	srcPath := writeTempFile(t, original)
	defer os.Remove(srcPath)

	dstPath, cSize, worthwhile, err := tryDeflate(srcPath, uint32(len(original)))
	if err != nil {
		t.Fatal(err)
	}
	if !worthwhile {
		t.Fatal("expected compression to be worthwhile")
	}
	defer os.Remove(dstPath)

	base := FMsgHeader{
		Version: 1,
		From:    FMsgAddress{User: "alice", Domain: "example.com"},
		To:      []FMsgAddress{{User: "bob", Domain: "other.com"}},
		Topic:   "test",
		Type:    "text/plain;charset=UTF-8",
	}

	// Hash without deflate
	plain := base
	plain.Flags = 0
	plain.Size = uint32(len(original))
	plain.Filepath = srcPath
	hashPlain, err := plain.GetMessageHash()
	if err != nil {
		t.Fatal(err)
	}

	// Hash with deflate
	deflated := base
	deflated.Flags = FlagDeflate
	deflated.Size = cSize
	deflated.Filepath = dstPath
	hashDeflated, err := deflated.GetMessageHash()
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(hashPlain, hashDeflated) {
		t.Error("expected different hashes for deflated vs non-deflated wire representations")
	}
}

func TestGetMessageHash_AttachmentDeflate(t *testing.T) {
	msgData := []byte("short message body that fits in a file")
	msgPath := writeTempFile(t, msgData)
	defer os.Remove(msgPath)

	attOriginal := []byte(strings.Repeat("attachment data for compression test ", 100))
	attSrcPath := writeTempFile(t, attOriginal)
	defer os.Remove(attSrcPath)

	attDstPath, attCSize, worthwhile, err := tryDeflate(attSrcPath, uint32(len(attOriginal)))
	if err != nil {
		t.Fatal(err)
	}
	if !worthwhile {
		t.Fatal("expected attachment compression to be worthwhile")
	}
	defer os.Remove(attDstPath)

	h := &FMsgHeader{
		Version:  1,
		Flags:    0,
		From:     FMsgAddress{User: "alice", Domain: "example.com"},
		To:       []FMsgAddress{{User: "bob", Domain: "other.com"}},
		Topic:    "test",
		Type:     "text/plain;charset=UTF-8",
		Size:     uint32(len(msgData)),
		Filepath: msgPath,
		Attachments: []FMsgAttachmentHeader{
			{
				Flags:    1 << 1, // attachment deflate bit
				Type:     "text/csv",
				Filename: "data.csv",
				Size:     attCSize,
				Filepath: attDstPath,
			},
		},
	}

	msgHash, err := h.GetMessageHash()
	if err != nil {
		t.Fatal(err)
	}

	// Manually compute: SHA-256(header + msg data + decompressed attachment)
	expected := sha256.New()
	expected.Write(h.Encode())
	expected.Write(msgData)
	expected.Write(attOriginal)
	expectedHash := expected.Sum(nil)

	if !bytes.Equal(msgHash, expectedHash) {
		t.Errorf("attachment hash mismatch:\n  got  %x\n  want %x", msgHash, expectedHash)
	}
}

// --- Encode flag tests ---

func TestEncode_DeflateFlag(t *testing.T) {
	h := &FMsgHeader{
		Version: 1,
		Flags:   FlagDeflate,
		From:    FMsgAddress{User: "alice", Domain: "example.com"},
		To:      []FMsgAddress{{User: "bob", Domain: "other.com"}},
		Topic:   "test",
		Type:    "text/plain;charset=UTF-8",
	}
	b := h.Encode()
	if b[1]&FlagDeflate == 0 {
		t.Error("deflate flag bit (5) not set in encoded header flags byte")
	}
}

func TestEncode_AttachmentDeflateFlag(t *testing.T) {
	h := &FMsgHeader{
		Version: 1,
		Flags:   0,
		From:    FMsgAddress{User: "alice", Domain: "example.com"},
		To:      []FMsgAddress{{User: "bob", Domain: "other.com"}},
		Topic:   "test",
		Type:    "text/plain;charset=UTF-8",
		Attachments: []FMsgAttachmentHeader{
			{Flags: 1 << 1, Type: "text/plain", Filename: "test.txt", Size: 100},
		},
	}
	b := h.Encode()
	// The encoded header ends with attachment headers. Find the attachment
	// flags byte: it's the first byte after the attachment count byte.
	// The attachment count is at len(b) - (1 + 1 + len("text/plain") + 1 + len("test.txt") + 4) - 1
	// Simpler: just verify the flags byte value appears in the output.
	// The attachment count byte (1) followed by attachment flags byte (0x02).
	found := false
	for i := 0; i < len(b)-1; i++ {
		if b[i] == 1 && b[i+1] == (1<<1) { // count=1, flags=0x02
			found = true
			break
		}
	}
	if !found {
		t.Error("attachment deflate flag bit (1) not found in encoded header")
	}
}
