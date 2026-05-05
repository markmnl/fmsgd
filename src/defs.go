package main

import (
	"bytes"
	"compress/zlib"
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
	Flags        uint8
	TypeID       uint8
	Type         string
	Filename     string
	Size         uint32
	ExpandedSize uint32

	Filepath string
}

type FMsgHeader struct {
	Version   uint8
	Flags     uint8
	Pid       []byte
	From      FMsgAddress
	To        []FMsgAddress
	AddToFrom *FMsgAddress // Present when has-add-to flag is set
	AddTo     []FMsgAddress
	Timestamp float64
	TypeID    uint8
	Topic     string
	Type      string

	// Size in bytes of entire message
	Size         uint32
	ExpandedSize uint32 // Decompressed size; present on wire iff FlagDeflate set
	Attachments  []FMsgAttachmentHeader

	HeaderHash          []byte
	ChallengeHash       [32]byte
	ChallengeCompleted  bool  // True if challenge was initiated and completed
	InitialResponseCode uint8 // Protocol response chosen after header validation (11/64/65)
	Filepath            string
	messageHash         []byte
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
		// add-to-from address (field 6)
		addToFrom := h.AddToFrom
		if addToFrom == nil {
			addToFrom = &h.From
		}
		str := addToFrom.ToString()
		b.WriteByte(byte(len(str)))
		b.WriteString(str)
		// add-to addresses (field 7)
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
	if h.Flags&FlagCommonType != 0 {
		typeID := h.TypeID
		if typeID == 0 {
			if id, ok := getCommonMediaTypeID(h.Type); ok {
				typeID = id
			}
		}
		b.WriteByte(typeID)
	} else {
		b.WriteByte(byte(len(h.Type)))
		b.WriteString(h.Type)
	}
	// size (uint32 LE)
	if err := binary.Write(&b, binary.LittleEndian, h.Size); err != nil {
		panic(err)
	}
	// expanded size (uint32 LE) — present iff zlib-deflate flag set
	if h.Flags&FlagDeflate != 0 {
		if err := binary.Write(&b, binary.LittleEndian, h.ExpandedSize); err != nil {
			panic(err)
		}
	}
	// attachment headers
	b.WriteByte(byte(len(h.Attachments)))
	for _, att := range h.Attachments {
		b.WriteByte(att.Flags)
		if att.Flags&1 != 0 {
			typeID := att.TypeID
			if typeID == 0 {
				if id, ok := getCommonMediaTypeID(att.Type); ok {
					typeID = id
				}
			}
			b.WriteByte(typeID)
		} else {
			b.WriteByte(byte(len(att.Type)))
			b.WriteString(att.Type)
		}
		b.WriteByte(byte(len(att.Filename)))
		b.WriteString(att.Filename)
		if err := binary.Write(&b, binary.LittleEndian, att.Size); err != nil {
			panic(err)
		}
		// attachment expanded size — present iff attachment zlib-deflate flag set
		if att.Flags&(1<<1) != 0 {
			if err := binary.Write(&b, binary.LittleEndian, att.ExpandedSize); err != nil {
				panic(err)
			}
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
		hash := sha256.New()

		headerBytes := h.Encode()
		if _, err := io.Copy(hash, bytes.NewBuffer(headerBytes)); err != nil {
			return nil, err
		}

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

func hashPayload(dst io.Writer, filepath string, wireSize int64, deflated bool, expandedSize uint32) error {
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
		written, err := io.Copy(dst, zr)
		_ = zr.Close()
		if err != nil {
			return err
		}
		if expandedSize > 0 && uint32(written) != expandedSize {
			return fmt.Errorf("decompressed size %d does not match declared expanded size %d", written, expandedSize)
		}
		return nil
	}

	_, err = io.CopyN(dst, f, wireSize)
	return err
}
