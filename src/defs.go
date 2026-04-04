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
	Flags    uint8
	Type     string
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
	Size        uint32
	Attachments []FMsgAttachmentHeader

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

// Encode the message header to wire format as a []byte. This includes all
// fields up to and including the attachment headers per spec. This function
// will panic on error instead of returning one.
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
	// topic is only present when pid is NOT set
	if h.Flags&FlagHasPid == 0 {
		b.WriteByte(byte(len(h.Topic)))
		b.WriteString(h.Topic)
	}
	// type: when common-type flag is set, write a single uint8 index;
	// otherwise write uint8 length + ASCII string.
	if h.Flags&FlagCommonType != 0 {
		num, ok := mediaTypeToNumber[h.Type]
		if !ok {
			panic(fmt.Sprintf("common type flag set but %q has no mapping", h.Type))
		}
		b.WriteByte(num)
	} else {
		b.WriteByte(byte(len(h.Type)))
		b.WriteString(h.Type)
	}
	// size (uint32 LE)
	if err := binary.Write(&b, binary.LittleEndian, h.Size); err != nil {
		panic(err)
	}
	// attachment headers
	b.WriteByte(byte(len(h.Attachments)))
	for _, att := range h.Attachments {
		b.WriteByte(att.Flags)
		// attachment type: common-type flag is bit 0 of attachment flags
		if att.Flags&AttachmentFlagCommonType != 0 {
			num, ok := mediaTypeToNumber[att.Type]
			if !ok {
				panic(fmt.Sprintf("attachment common type flag set but %q has no mapping", att.Type))
			}
			b.WriteByte(num)
		} else {
			b.WriteByte(byte(len(att.Type)))
			b.WriteString(att.Type)
		}
		b.WriteByte(byte(len(att.Filename)))
		b.WriteString(att.Filename)
		if err := binary.Write(&b, binary.LittleEndian, att.Size); err != nil {
			panic(err)
		}
	}
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

		if _, err := io.Copy(hash, f); err != nil {
			return nil, err
		}

		// include attachment data (sequential byte sequences following
		// the message body, bounded by attachment header sizes)
		for _, att := range h.Attachments {
			af, err := os.Open(att.Filepath)
			if err != nil {
				return nil, fmt.Errorf("open attachment %s: %w", att.Filename, err)
			}
			if _, err := io.CopyN(hash, af, int64(att.Size)); err != nil {
				af.Close()
				return nil, fmt.Errorf("read attachment %s: %w", att.Filename, err)
			}
			af.Close()
		}

		h.messageHash = hash.Sum(nil)
	}
	return h.messageHash, nil
}
