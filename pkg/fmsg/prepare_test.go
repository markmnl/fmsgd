package fmsg

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreparedHashAndDurableRepresentation(t *testing.T) {
	for _, body := range []string{"hello", strings.Repeat("compressible body ", 300)} {
		t.Run(string(rune(len(body))), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "body")
			att := filepath.Join(dir, "attachment")
			attachment := strings.Repeat("attachment content ", 100)
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(att, []byte(attachment), 0600); err != nil {
				t.Fatal(err)
			}
			input := &Header{Version: 1, From: Address{User: "alice", Domain: "example.com"}, To: []Address{{User: "bob", Domain: "example.com"}}, Timestamp: 1234.125, Type: "text/plain;charset=UTF-8", Topic: "topic", Size: uint32(len(body)), Filepath: path, Attachments: []AttachmentHeader{{Filename: "note.txt", Type: "text/plain;charset=UTF-8", Size: uint32(len(attachment)), Filepath: att}}}
			h, _, err := Prepare(input)
			if err != nil {
				t.Fatal(err)
			}
			manual := sha256.New()
			manual.Write(h.Encode())
			manual.Write([]byte(body))
			manual.Write([]byte(attachment))
			got, err := h.GetMessageHash()
			if err != nil || !bytes.Equal(got, manual.Sum(nil)) {
				t.Fatal("wrong protocol hash", err)
			}
			if h.Flags&FlagCommonType == 0 || h.Attachments[0].Flags&2 == 0 {
				t.Fatal("wire encoding was not finalized")
			}
			snapshot, err := MarshalPrepared(h)
			if err != nil {
				t.Fatal(err)
			}
			// Replacing the source files cannot change the immutable wire copy.
			_ = os.WriteFile(path, []byte("edited draft"), 0600)
			_ = os.Remove(att)
			restored, err := UnmarshalPrepared(snapshot, got)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(restored.Encode(), h.Encode()) {
				t.Fatal("wire header changed")
			}
			preserved, _, err := Preserve(restored, dir)
			if err != nil {
				t.Fatal(err)
			}
			preservedHash, err := preserved.GetMessageHash()
			if err != nil || !bytes.Equal(preservedHash, got) {
				t.Fatal("received identity changed", err)
			}
			_ = os.Remove(restored.Filepath)
			if _, err = UnmarshalPrepared(snapshot, got); err == nil {
				t.Fatal("missing immutable content accepted")
			}
		})
	}
}
