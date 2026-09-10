package fmsg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Clone returns a header with independent slices and no cached hashes.
func (h *Header) Clone() *Header {
	c := *h
	c.Pid = append([]byte(nil), h.Pid...)
	c.To = append([]Address(nil), h.To...)
	c.AddTo = append([]Address(nil), h.AddTo...)
	c.Attachments = append([]AttachmentHeader(nil), h.Attachments...)
	c.HeaderHash, c.messageHash = nil, nil
	return &c
}

// ApplyCommonTypes chooses the compact encoding before a message is hashed.
func ApplyCommonTypes(h *Header) bool {
	changed := false
	if id, ok := GetCommonMediaTypeID(h.Type); ok && h.Flags&FlagCommonType == 0 {
		h.Flags |= FlagCommonType
		h.TypeID = id
		changed = true
	}
	for i := range h.Attachments {
		a := &h.Attachments[i]
		if id, ok := GetCommonMediaTypeID(a.Type); ok && a.Flags&1 == 0 {
			a.Flags |= 1
			a.TypeID = id
			changed = true
		}
	}
	return changed
}

// Prepare freezes a locally authored message's wire representation. Input
// payloads are expanded bytes; HTTP content types (including application/zip)
// do not imply protocol zlib compression. Files are streamed into a durable
// directory beside the body, synced before returning, and reused by retries
// and add-to batches. The caller removes directory if its DB transaction fails.
func Prepare(input *Header) (h *Header, directory string, err error) {
	h = input.Clone()
	if h.Version != 1 || len(h.To) == 0 || len(h.To) > 255 || len(h.Attachments) > 255 || len(h.Topic) > 255 || len(h.Type) == 0 || len(h.Type) > 255 {
		return nil, "", fmt.Errorf("invalid message header lengths or version")
	}
	if (h.Flags&FlagHasPid != 0) != (len(h.Pid) == 32) {
		return nil, "", fmt.Errorf("reply requires a 32-byte parent hash")
	}
	directory, err = os.MkdirTemp(filepath.Dir(h.Filepath), ".fmsg-wire-")
	if err != nil {
		return nil, "", err
	}
	if err = os.Chmod(directory, 0750); err != nil {
		_ = os.RemoveAll(directory)
		return nil, "", err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(directory)
		}
	}()
	h.Flags &^= FlagDeflate
	h.ExpandedSize = 0
	h.Filepath, h.Size, h.ExpandedSize, err = preparePart(h.Filepath, h.Size, h.Type, filepath.Join(directory, "body"))
	if err != nil {
		return nil, directory, err
	}
	if h.ExpandedSize > 0 {
		h.Flags |= FlagDeflate
	}
	for i := range h.Attachments {
		a := &h.Attachments[i]
		if len(a.Filename) == 0 || len(a.Filename) > 255 || len(a.Type) == 0 || len(a.Type) > 255 {
			return nil, directory, fmt.Errorf("invalid attachment header")
		}
		a.Flags &^= 2
		a.Filepath, a.Size, a.ExpandedSize, err = preparePart(a.Filepath, a.Size, a.Type, filepath.Join(directory, fmt.Sprintf("attachment-%d", i)))
		if err != nil {
			return nil, directory, err
		}
		if a.ExpandedSize > 0 {
			a.Flags |= 2
		}
	}
	ApplyCommonTypes(h)
	_, err = h.GetMessageHash()
	if err == nil {
		err = syncDirectory(directory)
	}
	if err == nil {
		err = syncDirectory(filepath.Dir(directory))
	}
	return h, directory, err
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Preserve retains an already verified incoming wire form without re-encoding
// its payloads. StoredWire is passed through the receiver's expansion step.
func Preserve(input *Header, parentDir string) (h *Header, directory string, err error) {
	h = input.Clone()
	directory, err = os.MkdirTemp(parentDir, ".fmsg-wire-")
	if err != nil {
		return nil, "", err
	}
	if err = os.Chmod(directory, 0750); err != nil {
		_ = os.RemoveAll(directory)
		return nil, "", err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(directory)
		}
	}()
	copyPart := func(src, name string, size uint32) (string, error) {
		in, e := os.Open(src)
		if e != nil {
			return "", e
		}
		defer in.Close()
		path := filepath.Join(directory, name)
		out, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
		if e != nil {
			return "", e
		}
		_, e = io.CopyN(out, in, int64(size))
		if e == nil {
			e = out.Sync()
		}
		ce := out.Close()
		if e == nil {
			e = ce
		}
		return path, e
	}
	h.Filepath, err = copyPart(h.Filepath, "body", h.Size)
	if err != nil {
		return nil, directory, err
	}
	for i := range h.Attachments {
		a := &h.Attachments[i]
		a.Filepath, err = copyPart(a.Filepath, fmt.Sprintf("attachment-%d", i), a.Size)
		if err != nil {
			return nil, directory, err
		}
	}
	if _, err = h.GetMessageHash(); err != nil {
		return nil, directory, err
	}
	if err = syncDirectory(directory); err != nil {
		return nil, directory, err
	}
	err = syncDirectory(parentDir)
	return h, directory, err
}

func preparePart(src string, size uint32, typ, dst string) (string, uint32, uint32, error) {
	info, err := os.Stat(src)
	if err != nil {
		return "", 0, 0, err
	}
	if !info.Mode().IsRegular() || info.Size() != int64(size) {
		return "", 0, 0, fmt.Errorf("payload length differs from metadata: %s", src)
	}
	expanded := uint32(0)
	if ShouldCompress(typ, size) {
		path, compressed, useful, err := TryCompress(src, size)
		if err != nil {
			return "", 0, 0, err
		}
		if useful {
			defer os.Remove(path)
			src, size, expanded = path, compressed, size
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return "", 0, 0, err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0640)
	if err != nil {
		return "", 0, 0, err
	}
	_, err = io.CopyN(out, in, int64(size))
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err == nil {
		err = closeErr
	}
	return dst, size, expanded, err
}

// MarshalPrepared stores an internal, versioned snapshot, including durable
// payload paths. It is not an HTTP representation of a message.
func MarshalPrepared(h *Header) ([]byte, error) {
	return json.Marshal(struct {
		Version int
		Header  *Header
	}{1, h.Clone()})
}

// UnmarshalPrepared verifies the snapshot against the persisted protocol hash.
// A missing or corrupt payload is an error, never a reason to re-encode it.
func UnmarshalPrepared(data, expected []byte) (*Header, error) {
	var s struct {
		Version int
		Header  *Header
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	if s.Version != 1 || s.Header == nil {
		return nil, fmt.Errorf("unsupported prepared message")
	}
	h := s.Header.Clone()
	hash, err := h.GetMessageHash()
	if err != nil {
		return nil, err
	}
	if len(expected) != 32 || !bytes.Equal(hash, expected) {
		return nil, fmt.Errorf("prepared message hash mismatch")
	}
	return h, nil
}
