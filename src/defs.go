package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

type FMsgAddress struct {
	User   string
	Domain string
}

type FMsgAttachmentHeader struct {
	Filename string
	Size     uint32

	Filepath string
}

type FMsgHeader struct {
	Version   uint8
	Flags     uint8
	Pid       []byte
	From      FMsgAddress
	To        []FMsgAddress
	AddTo     []FMsgAddress
	Timestamp float64
	Topic     string
	Type      string

	// Size in bytes of entire message
	Size uint32
	// TODO [Spec]: Add Attachments []FMsgAttachmentHeader field to store parsed
	// attachment headers (flags, type, filename, size) from the wire format.

	// Hash up to and including Type
	HeaderHash []byte
	// Hash of message from challenge response
	ChallengeHash [32]byte
	// TODO [Spec]: Add a ChallengeCompleted bool (or similar sentinel) to
	// distinguish "challenge was completed and ChallengeHash is valid" from
	// "challenge was not performed and ChallengeHash is zero-valued".
	// Absolute filepath set when downloaded
	Filepath string

	// Actual hash of message data including any attachments
	messageHash []byte
}

// Returns a string representation of an address in the form @user@example.com
func (addr *FMsgAddress) ToString() string {
	return fmt.Sprintf("@%s@%s", addr.User, addr.Domain)
}

// Encode the header up to and including type field to a []byte. This function will panic on error
// instead of returning one.
// TODO [Spec]: The spec defines "message header" as all fields up to and
// including the attachment headers field. This Encode() is missing:
//   - The "size" field (uint32).
//   - The "attachment headers" field (uint8 count + list of attachment headers).
//
// The header hash (SHA-256 of the encoded header) will be incorrect without
// these fields, breaking challenge verification and pid references.
// Additionally, the topic field should only be encoded when pid is NOT set.
func (h *FMsgHeader) Encode() []byte {
	var b bytes.Buffer
	b.WriteByte(h.Version)
	b.WriteByte(h.Flags)
	if h.Flags&FlagHasPid == 1 {
		b.Write(h.Pid[:])
	}
	str := h.From.ToString()
	b.WriteByte(byte(len(str)))
	b.WriteString(str)
	b.WriteByte(byte(len(h.To)))
	for _, addr := range h.To {
		str = addr.ToString()
		b.WriteByte(byte(len(str)))
		b.WriteString(str)
	}
	if h.Flags&FlagHasAddTo != 0 {
		b.WriteByte(byte(len(h.AddTo)))
		for _, addr := range h.AddTo {
			str = addr.ToString()
			b.WriteByte(byte(len(str)))
			b.WriteString(str)
		}
	}
	if err := binary.Write(&b, binary.LittleEndian, h.Timestamp); err != nil {
		panic(err)
	}
	b.WriteByte(byte(len(h.Topic)))
	b.WriteString(h.Topic)
	b.WriteByte(byte(len(h.Type)))
	b.WriteString(h.Type)
	return b.Bytes()
}

// String returns a human-readable summary of the header fields.
func (h *FMsgHeader) String() string {
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

func (h *FMsgHeader) GetHeaderHash() []byte {
	if h.HeaderHash == nil {
		b := sha256.Sum256(h.Encode())
		h.HeaderHash = b[:]
	}
	return h.HeaderHash
}

func (h *FMsgHeader) GetMessageHash() ([]byte, error) {
	if h.messageHash == nil {
		f, err := os.Open(h.Filepath)
		if err != nil {
			return nil, err
		}
		defer f.Close()

		hash := sha256.New()

		headerBytes := h.Encode()
		if _, err := io.Copy(hash, bytes.NewBuffer(headerBytes)); err != nil {
			return nil, err
		}

		// TODO [Spec]: Encode() is missing size, attachment count, and
		// attachment headers. Once Encode() includes the full message header
		// per spec, the size and attachment header bytes will automatically be
		// included in this hash.

		if _, err := io.Copy(hash, f); err != nil {
			return nil, err
		}

		// TODO: include attachment data (sequential byte sequences following
		// the message body, bounded by attachment header sizes) in the hash.

		h.messageHash = hash.Sum(nil)
	}
	return h.messageHash, nil
}
