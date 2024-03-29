package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"os"
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
	Timestamp float64
	Topic     string
	Type      string

	// Size in bytes of entire message
	Size uint32
	// Hash up to and including Type
	HeaderHash []byte
	// Hash of message from challenge response
	ChallengeHash [32]byte
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
func (h *FMsgHeader) Encode() []byte {
	var b bytes.Buffer
	b.WriteByte(h.Version)
	b.WriteByte(h.Flags)
	if h.Flags & FlagHasPid == 1 {
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
	if err := binary.Write(&b, binary.LittleEndian, h.Timestamp); err != nil {
		panic(err)
	}
	b.WriteByte(byte(len(h.Topic)))
	b.WriteString(h.Topic)
	b.WriteByte(byte(len(h.Type)))
	b.WriteString(h.Type)
	return b.Bytes()
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

		// TODO attachments

		h.messageHash = hash.Sum(nil)
	}
	return h.messageHash, nil
}