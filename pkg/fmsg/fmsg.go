// Package fmsg defines the fmsg message types and implements wire-format
// encoding and hashing as specified in SPEC.md.
//
// To compute a message hash from database fields, populate a [Header]
// (including Filepath and per-attachment Filepath values), then call
// [Header.GetMessageHash].
package fmsg

import (
	"compress/zlib"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
)

// Flag bit assignments per SPEC.md §3.
// Bit 7 is reserved and must be zero on the wire.
const (
	FlagHasPid     uint8 = 1      // bit 0: pid field present; message is a reply
	FlagHasAddTo   uint8 = 1 << 1 // bit 1: add-to addresses present
	FlagCommonType uint8 = 1 << 2 // bit 2: type encoded as common type ID, not string
	FlagImportant  uint8 = 1 << 3 // bit 3: sender marks message as important
	FlagNoReply    uint8 = 1 << 4 // bit 4: sender will discard replies (advisory)
	FlagDeflate    uint8 = 1 << 5 // bit 5: message body is zlib-deflate compressed
	FlagTerminal   uint8 = 1 << 6 // bit 6: leaf message; no message may reference it via pid (enforced by hosts)
)

// Address is an fmsg address of the form @user@domain.
type Address struct {
	User   string
	Domain string
}

// ToString returns the address in @user@domain form.
func (addr *Address) ToString() string {
	return "@" + addr.User + "@" + addr.Domain
}

// AttachmentHeader holds the wire-level metadata for a single attachment.
type AttachmentHeader struct {
	Flags        uint8
	TypeID       uint8
	Type         string
	Filename     string
	Size         uint32 // wire (possibly compressed) size in bytes
	ExpandedSize uint32 // decompressed size; present on wire iff bit 1 of Flags is set

	Filepath string // path to attachment data on disk
}

// Header holds all fields of an fmsg message header.
//
// Fields ChallengeHash, ChallengeCompleted, and InitialResponseCode are
// fmsgd server-runtime state; they are not part of the wire format and can
// be ignored by other consumers of this package.
type Header struct {
	Version   uint8
	Flags     uint8
	Pid       []byte
	From      Address
	To        []Address
	AddToFrom *Address // present when FlagHasAddTo is set
	AddTo     []Address
	Timestamp float64
	TypeID    uint8
	Topic     string
	Type      string

	Size         uint32 // wire (possibly compressed) size of the message body
	ExpandedSize uint32 // decompressed size; present on wire iff FlagDeflate set
	Attachments  []AttachmentHeader

	HeaderHash          []byte
	ChallengeHash       [32]byte // fmsgd server field: challenge response hash
	ChallengeCompleted  bool     // fmsgd server field: true if challenge was completed
	InitialResponseCode uint8    // fmsgd server field: protocol response code (11/64/65)
	Filepath            string   // path to message body data on disk
	StoredWire          []byte   `json:"-"` // receive path: durable wire snapshot before expanding API payloads
	messageHash         []byte
}

// Encode serialises the message header to wire format as defined in SPEC.md.
// The returned bytes cover all fields up to and including the attachment
// headers (fields 1–13 per spec).
func (h *Header) Encode() []byte {
	addToFrom := h.AddToFrom
	if h.Flags&FlagHasAddTo != 0 && addToFrom == nil {
		addToFrom = &h.From
	}

	// Pre-size output buffer to avoid repeated reallocations.
	size := 2 + // version + flags
		1 + 2 + len(h.From.User) + len(h.From.Domain) + // from
		1 + // to count
		8 + // timestamp
		4 + // size
		1 // attachment count
	if h.Flags&FlagHasPid != 0 {
		size += len(h.Pid)
	} else {
		size += 1 + len(h.Topic)
	}
	for i := range h.To {
		size += 3 + len(h.To[i].User) + len(h.To[i].Domain)
	}
	if h.Flags&FlagHasAddTo != 0 {
		size += 3 + len(addToFrom.User) + len(addToFrom.Domain) + 1
		for i := range h.AddTo {
			size += 3 + len(h.AddTo[i].User) + len(h.AddTo[i].Domain)
		}
	}
	if h.Flags&FlagCommonType != 0 {
		size++
	} else {
		size += 1 + len(h.Type)
	}
	if h.Flags&FlagDeflate != 0 {
		size += 4
	}
	for i := range h.Attachments {
		att := &h.Attachments[i]
		size += 1 + 1 + len(att.Filename) + 4
		if att.Flags&1 != 0 {
			size++
		} else {
			size += 1 + len(att.Type)
		}
		if att.Flags&(1<<1) != 0 {
			size += 4
		}
	}

	appendAddr := func(b []byte, a *Address) []byte {
		b = append(b, byte(2+len(a.User)+len(a.Domain)), '@')
		b = append(b, a.User...)
		b = append(b, '@')
		return append(b, a.Domain...)
	}

	b := make([]byte, 0, size)
	b = append(b, h.Version, h.Flags)
	if h.Flags&FlagHasPid != 0 {
		b = append(b, h.Pid...)
	}
	b = appendAddr(b, &h.From)
	b = append(b, byte(len(h.To)))
	for i := range h.To {
		b = appendAddr(b, &h.To[i])
	}
	if h.Flags&FlagHasAddTo != 0 {
		// add-to-from address (field 6)
		b = appendAddr(b, addToFrom)
		// add-to addresses (field 7)
		b = append(b, byte(len(h.AddTo)))
		for i := range h.AddTo {
			b = appendAddr(b, &h.AddTo[i])
		}
	}
	b = binary.LittleEndian.AppendUint64(b, math.Float64bits(h.Timestamp))
	// topic is only present when pid is NOT set
	if h.Flags&FlagHasPid == 0 {
		b = append(b, byte(len(h.Topic)))
		b = append(b, h.Topic...)
	}
	if h.Flags&FlagCommonType != 0 {
		typeID := h.TypeID
		if typeID == 0 {
			if id, ok := getCommonMediaTypeID(h.Type); ok {
				typeID = id
			}
		}
		b = append(b, typeID)
	} else {
		b = append(b, byte(len(h.Type)))
		b = append(b, h.Type...)
	}
	// size (uint32 LE)
	b = binary.LittleEndian.AppendUint32(b, h.Size)
	// expanded size (uint32 LE) — present iff zlib-deflate flag set
	if h.Flags&FlagDeflate != 0 {
		b = binary.LittleEndian.AppendUint32(b, h.ExpandedSize)
	}
	// attachment headers
	b = append(b, byte(len(h.Attachments)))
	for i := range h.Attachments {
		att := &h.Attachments[i]
		b = append(b, att.Flags)
		if att.Flags&1 != 0 {
			typeID := att.TypeID
			if typeID == 0 {
				if id, ok := getCommonMediaTypeID(att.Type); ok {
					typeID = id
				}
			}
			b = append(b, typeID)
		} else {
			b = append(b, byte(len(att.Type)))
			b = append(b, att.Type...)
		}
		b = append(b, byte(len(att.Filename)))
		b = append(b, att.Filename...)
		b = binary.LittleEndian.AppendUint32(b, att.Size)
		// attachment expanded size — present iff attachment zlib-deflate flag set
		if att.Flags&(1<<1) != 0 {
			b = binary.LittleEndian.AppendUint32(b, att.ExpandedSize)
		}
	}
	return b
}

// String returns a human-readable summary of the header fields.
func (h *Header) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "v%d flags=%d", h.Version, h.Flags)
	if len(h.Pid) > 0 {
		fmt.Fprintf(&b, " pid=%s", hex.EncodeToString(h.Pid))
	}
	fmt.Fprintf(&b, "\nfrom:\t%s", h.From.ToString())
	for i, addr := range h.To {
		if i == 0 {
			fmt.Fprintf(&b, "\nto:\t%s", addr.ToString())
		} else {
			fmt.Fprintf(&b, "\n\t%s", addr.ToString())
		}
	}
	for i, addr := range h.AddTo {
		if i == 0 {
			fmt.Fprintf(&b, "\nadd to:\t%s", addr.ToString())
		} else {
			fmt.Fprintf(&b, "\n\t%s", addr.ToString())
		}
	}
	fmt.Fprintf(&b, "\ntopic:\t%s", h.Topic)
	fmt.Fprintf(&b, "\ntype:\t%s", h.Type)
	fmt.Fprintf(&b, "\nsize:\t%d", h.Size)
	return b.String()
}

// GetHeaderHash returns the SHA-256 hash of the encoded header (fields 1–12).
// The result is cached after the first call.
func (h *Header) GetHeaderHash() []byte {
	if h.HeaderHash == nil {
		b := sha256.Sum256(h.Encode())
		h.HeaderHash = b[:]
	}
	return h.HeaderHash
}

// GetMessageHash returns the SHA-256 hash of the full message:
// encoded header + decompressed message body + decompressed attachment data.
// The result is cached after the first call.
func (h *Header) GetMessageHash() ([]byte, error) {
	if h.messageHash == nil {
		hash := sha256.New()

		headerBytes := h.Encode()
		hash.Write(headerBytes)

		if err := hashPayload(hash, h.Filepath, int64(h.Size), h.Flags&FlagDeflate != 0, h.ExpandedSize); err != nil {
			return nil, err
		}

		// include attachment data (sequential byte sequences following
		// the message body, bounded by attachment header sizes)
		for _, att := range h.Attachments {
			compressed := att.Flags&(1<<1) != 0
			if err := hashPayload(hash, att.Filepath, int64(att.Size), compressed, att.ExpandedSize); err != nil {
				return nil, fmt.Errorf("hash attachment %s: %w", att.Filename, err)
			}
		}

		h.messageHash = hash.Sum(nil)
	}
	return h.messageHash, nil
}

// HashPayload reads wireSize bytes from the file at filepath, decompressing
// via zlib if deflated, and writes the (decompressed) bytes to dst.
// The message hash is always computed over decompressed data.
func HashPayload(dst io.Writer, filepath string, wireSize int64, deflated bool, expandedSize uint32) error {
	f, err := os.Open(filepath)
	if err != nil {
		return err
	}
	defer f.Close()

	if deflated {
		lr := io.LimitReader(f, wireSize)
		zr, err := zlib.NewReader(lr)
		if err != nil {
			return err
		}
		written, err := io.Copy(dst, io.LimitReader(zr, int64(expandedSize)+1))
		closeErr := zr.Close()
		if err != nil {
			return err
		}
		if written != int64(expandedSize) {
			return fmt.Errorf("decompressed size %d does not match declared expanded size %d", written, expandedSize)
		}
		if closeErr != nil {
			return closeErr
		}
		return nil
	}

	_, err = io.CopyN(dst, f, wireSize)
	return err
}

// commonMediaTypes maps common type IDs (1–64) to MIME strings per SPEC.md §4.
// Unmapped IDs must be rejected with response code 1 (invalid).
var commonMediaTypes = map[uint8]string{
	1: "application/epub+zip", 2: "application/gzip", 3: "application/json", 4: "application/msword",
	5: "application/octet-stream", 6: "application/pdf", 7: "application/rtf", 8: "application/vnd.amazon.ebook",
	9: "application/vnd.ms-excel", 10: "application/vnd.ms-powerpoint",
	11: "application/vnd.oasis.opendocument.presentation", 12: "application/vnd.oasis.opendocument.spreadsheet",
	13: "application/vnd.oasis.opendocument.text",
	14: "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	15: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	16: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	17: "application/x-tar", 18: "application/xhtml+xml", 19: "application/xml", 20: "application/zip",
	21: "audio/aac", 22: "audio/midi", 23: "audio/mpeg", 24: "audio/ogg", 25: "audio/opus", 26: "audio/vnd.wave", 27: "audio/webm",
	28: "font/otf", 29: "font/ttf", 30: "font/woff", 31: "font/woff2",
	32: "image/apng", 33: "image/avif", 34: "image/bmp", 35: "image/gif", 36: "image/heic", 37: "image/jpeg", 38: "image/png",
	39: "image/svg+xml", 40: "image/tiff", 41: "image/webp",
	42: "model/3mf", 43: "model/gltf-binary", 44: "model/obj", 45: "model/step", 46: "model/stl", 47: "model/vnd.usdz+zip",
	48: "text/calendar", 49: "text/css", 50: "text/csv", 51: "text/html", 52: "text/javascript", 53: "text/markdown",
	54: "text/plain;charset=US-ASCII", 55: "text/plain;charset=UTF-16", 56: "text/plain;charset=UTF-8", 57: "text/vcard",
	58: "video/H264", 59: "video/H265", 60: "video/H266", 61: "video/ogg", 62: "video/VP8", 63: "video/VP9", 64: "video/webm",
}

var commonMediaTypeIDs = func() map[string]uint8 {
	m := make(map[string]uint8, len(commonMediaTypes))
	for id, mime := range commonMediaTypes {
		m[mime] = id
	}
	return m
}()

// GetCommonMediaType returns the MIME type string for a common type ID, or
// ("", false) if the ID is not in the mapping (should be rejected per spec).
func GetCommonMediaType(id uint8) (string, bool) {
	s, ok := commonMediaTypes[id]
	return s, ok
}

// GetCommonMediaTypeID returns the common type ID for a MIME string, or
// (0, false) if the string has no assigned ID.
func GetCommonMediaTypeID(mediaType string) (uint8, bool) {
	id, ok := commonMediaTypeIDs[mediaType]
	return id, ok
}

// getCommonMediaTypeID is the unexported alias used internally by Encode.
func getCommonMediaTypeID(mediaType string) (uint8, bool) {
	return GetCommonMediaTypeID(mediaType)
}

// hashPayload is the unexported alias used internally by GetMessageHash.
func hashPayload(dst io.Writer, filepath string, wireSize int64, deflated bool, expandedSize uint32) error {
	return HashPayload(dst, filepath, wireSize, deflated, expandedSize)
}
