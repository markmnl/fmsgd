package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const Port = 36900
const ReadBufferSize = 1600
const MaxMessageSize = 1024 * 10
const (
	HasPid uint8 = 1
	Important uint8 = 1 << 1

	
	RejectUndisclosed uint8 = 1
	RejectCodeTooBig uint8  = 2
	RejectAccept uint8 = 255
)

var IoDeadline, _ = time.ParseDuration("5s")
var AtRune, _ = utf8.DecodeRuneInString("@")
var DataDir = "/tmp"
var Domain = "localhost"

// outgoing message headers keyed on header hash
var outgoing map[[32]byte]*FMsgHeader

type FMsgAddress struct {
	User string
	Domain string
}

type FMsgAttachmentHeader struct {
	Filename string
	Size uint32

	Filepath string
}

type FMsgHeader struct {
	Version uint8
	Flags uint8
	Pid [32]byte
	From FMsgAddress
	To []FMsgAddress
	Timestamp float64
	Topic string
	Type string

	// Size in bytes of entire message
	Size uint32
	// Hash up to and including Type
	HeaderHash []byte
	// Hash of message from Challenge Response
	ChallengeHash [32]byte
	// Actual hash of message data including any attachments data
	MessageHash []byte
	// 
	Filepath string
}

func (addr *FMsgAddress) ToString() string {
	return fmt.Sprintf("@%s@%s", addr.User, addr.Domain)
}

// Encode the header up to and including type field to a []byte. This function will panic on error
// instead of returning one.
func (h *FMsgHeader) Encode() []byte {
	var b bytes.Buffer
	b.WriteByte(h.Version)
	b.WriteByte(h.Flags)
	if h.Flags & HasPid == 1 {
		b.Write(h.Pid[:])
	}
	b.WriteString(h.From.ToString())
	b.WriteByte(byte(len(h.To)))
	for _, addr := range h.To {
		b.WriteString(addr.ToString())
	}
	if err := binary.Write(&b, binary.LittleEndian, h.Timestamp); err != nil {
		panic(err)
	}
	b.WriteString(h.Topic)
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
	if h.MessageHash == nil {
		f, err := os.Open(h.Filepath)
		if err != nil {
		  return nil, err
		}
		defer f.Close()
	  
		hash := sha256.New()
		if _, err := io.Copy(hash, f); err != nil {
		  return nil, err
		}
		
		// TODO attachments

		h.MessageHash = hash.Sum(nil)
	}
	return h.MessageHash, nil
}

func parseAddress(b []byte) (*FMsgAddress, error) {
	var addr = &FMsgAddress{}
	addrStr := string(b)
	firstAt := strings.IndexRune(addrStr, AtRune)
	if firstAt == -1 || firstAt != 0 {
		return addr, fmt.Errorf("invalid address, must start with @ %s", addr)
	}
	lastAt := strings.LastIndex(addrStr, "@")
	if lastAt == firstAt {
		return addr, fmt.Errorf("invalid address, must have second @ %s", addr)
	}
	addr.User = addrStr[1:lastAt]
	addr.Domain = addrStr[lastAt+1:]
	return addr, nil
}

// Reads byte slice prefixed with uint8 size from reader supplied
func ReadUInt8Slice(r io.Reader) ([]byte, error) {
	var size byte
	err := binary.Read(r, binary.LittleEndian, &size)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(r, int64(size)))
}

func readAddress(r io.Reader) (*FMsgAddress, error) {
	slice, err := ReadUInt8Slice(r)
	if err != nil {
		return nil, err
	}
	return parseAddress(slice)
}

func handleChallenge(c net.Conn, r *bufio.Reader) error {
	hashSlice, err := io.ReadAll(io.LimitReader(c, 32))
	if err != nil {
		return err
	}
	hash := *(*[32]byte)(hashSlice) // get the underlying array (alternatively we could use hex strings..)
	header, exists := outgoing[hash]
	if !exists {
		return fmt.Errorf("challenge for unknown message: %s, from: %s", hex.EncodeToString(hashSlice), c.RemoteAddr().String())
	}
	msgHash, err := header.GetMessageHash()
	if err != nil {
		return err
	}
	if _, err := c.Write(msgHash); err != nil {
		return err
	}
	return nil
}

func rejectAccept(c net.Conn, codes []byte) error {
	_, err := c.Write(codes)
	return err
}

func readHeader(c net.Conn) (*FMsgHeader, error) {
	r := bufio.NewReaderSize(c, ReadBufferSize)
	var h = &FMsgHeader{}

	// read version
	v, err := r.ReadByte()
	if err != nil {
		return h, err
	}
	if v == 255 {
		return nil, handleChallenge(c, r)
	}
	if v != 1 {
		return h, fmt.Errorf("unsupported version: %d", v)
	}
	h.Version = v

	// read flags
	flags, err := r.ReadByte()
	if err != nil {
		return h, err
	}
	h.Flags = flags
	
	// read pid if any
	if flags & HasPid == 1 {  
		pid, err := io.ReadAll(io.LimitReader(c, 32))
		if err != nil {
			return h, err
		}
		copy(h.Pid[:], pid)
		// TODO verify pid exists
	}

	// read from address
	from, err := readAddress(r)
	if err != nil {
		return h, err
	}
	log.Printf("from: @%s@%s", from.User, from.Domain)
	h.From = *from
 
	// read to addresses TODO validate unique
	num, err := r.ReadByte()
	if err != nil {
		return h, err
	} 
	for num > 0 {
		addr, err := readAddress(r)
		if err != nil {
			return h, err
		}
		h.To = append(h.To, *addr)
		num--
		log.Printf("to: @%s@%s", addr.User, addr.Domain)
	}

	// read timestamp
	if err := binary.Read(r, binary.LittleEndian, &h.Timestamp); err != nil {
		return h, err
	}

	// read topic
	topic, err := ReadUInt8Slice(r)
	if err != nil {
		return h, err
	}
	h.Topic = string(topic)

	// read type
	mime, err := ReadUInt8Slice(r)
	if err != nil {
		return h, err
	}
	h.Topic = string(mime)

	// read message size
	if err := binary.Read(r, binary.LittleEndian, &h.Size); err != nil {
		return h, err
	}
	if h.Size > MaxMessageSize {
		codes := []byte{0, RejectCodeTooBig}
		if err := rejectAccept(c, codes); err != nil {
			return h, err
		}
		return h, fmt.Errorf("message size: %d exceeds max: %d", h.Size, MaxMessageSize)
	}
	
	// TODO attachments

	return h, nil
}

// Sends CHALLENGE request to sender domain first checking if domain is indeed located
// at address in connection supplied by doing a host lookup.
func challenge(conn net.Conn, h *FMsgHeader) error {

	// TODO check if no challenge requested and we allow it
	addr := strings.Split(conn.RemoteAddr().String(), ":")[0]
	addrs, err := net.LookupHost(h.From.Domain)
	if err != nil {
		return err
	}
	found := false
	for _, a := range addrs {
		if addr == a { // TODO do we need any normalization?
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("remote address: %s not found in lookup for host: %s", addr, h.From.Domain)
	}

	// okay lets give sender a call and confirm they are sending this message
	conn2, err := net.Dial("tcp", fmt.Sprintf("%s:%d", h.From.Domain, Port)) 
	if err != nil {
		return err
	}
	version := byte(255)
	if err := binary.Write(conn2, binary.LittleEndian, version); err != nil {
		return err
	}
	hash := h.GetHeaderHash()
	if _, err := conn2.Write(hash); err != nil {
		return err
	}

	// read challenge response
	msgHash, err := io.ReadAll(io.LimitReader(conn2, 32))
	if err != nil {
		return err
	}
	copy(h.ChallengeHash[:], msgHash)

	// gracefully close 2nd connection
	if err := conn2.Close(); err != nil {
		return err
	}

	return nil
}

func downloadMessage(c net.Conn, h *FMsgHeader) error {
	
	// first download to temp file
	addrs := []FMsgAddress{}
	for _, addr := range(h.To) {
		if addr.Domain == Domain {
			addrs = append(addrs, addr)
		}
	}
	codes := make([]byte, len(addrs))
	fd, err := os.CreateTemp("", "fmsg-download-*")
	if err != nil {
		return err
	}
	defer os.Remove(fd.Name())
	defer fd.Close()
	_, err = io.CopyN(fd, c, int64(h.Size))
	if err != nil  {
		return err
	}

	// TODO attachments
	// TODO check checksum

	// copy to each recipient's directory
	for i, addr := range addrs {
		// TODO check disk space and user quota
		fp := filepath.Join(DataDir, addr.User, h.Topic) // TODO better naming
		fd2, err := os.Create(fp)
		if err != nil  {
			return err
		}
		defer fd2.Close()
		_, err = io.Copy(fd2, fd)
		if err != nil  {
			codes[i] = RejectUndisclosed // TODO better error ?
		} else {
			codes[i] = RejectAccept
		}
	}
	return rejectAccept(c, codes)
}

func handleConn(c net.Conn) {
	log.Printf("Connection from: %s", c.RemoteAddr().String())

	// set read deadline for reading header
	c.SetReadDeadline(time.Now().Add(IoDeadline))

	// read header
	header, err := readHeader(c)
	if err != nil {
		log.Printf("Error reading header from, %s: %s", c.RemoteAddr().String(), err)
		return
	}

	// if no header AND no error this was a challenge thats been handeled
	if header == nil {
		c.Close()
		return
	}

	c.SetReadDeadline(time.UnixMilli(0))

	// challenge
	err = challenge(c, header)
	if err != nil {
		log.Printf("Challenge failed from, %s: %s", c.RemoteAddr().String(), err)
		return
	}

	// download message
	err = downloadMessage(c, header)
	if err != nil {
		log.Printf("Download failed form, %s: %s", c.RemoteAddr().String(), err)
		return
	}

	// TODO add details to index

	// gracefully close 1st connection
	c.Close()
}


func main() {
	outgoing = make(map[[32]byte]*FMsgHeader)

	ln, err := net.Listen("tcp", "localhost:36900")
	if err != nil {
		log.Fatal(err)
	}
	for {
		log.Println("Listening on localhost:36900")
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("Error accepting connection from, %s: %s\n", ln.Addr().String(), err)
		} else {
			go handleConn(conn)
		}
	}
}

