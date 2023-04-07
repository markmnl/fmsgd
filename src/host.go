package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"math"
	"mime"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	env "github.com/caitlinelfring/go-env-default"
	"github.com/levenlabs/golib/timeutil"
)

const (
	InboxDirName = "in"
	OutboxDirName = "out"

	HasPid    uint8 = 1
	Important uint8 = 1 << 1

	RejectUndisclosed uint8 = 1
	RejectCodeTooBig  uint8 = 2
	RejectCodeInsufficentResources  uint8 = 3
	RejectCodeParentNotFound  uint8 = 4
	RejectCodePastTime  uint8 = 5
	RejectCodeFutureTime  uint8 = 6
	RejectCodeTimeTravel  uint8 = 7
	RejectCodeDuplicate  uint8 = 8

	RejectCodeUserUnknown  uint8 = 100
	RejectCodeUserFull  uint8 = 101

	RejectAccept      uint8 = 255
)

var AtRune, _ = utf8.DecodeRuneInString("@")
var Port = env.GetIntDefault("FMSG_PORT", 36900)
var RemotePort = env.GetIntDefault("FMSG_REMOTE_PORT", 36901)
var PastTimeDelta float64 = env.GetFloatDefault("FMSG_MIN_PAST_TIME_DELTA", 7 * 24 * 60 * 60)
var FutureTimeDelta float64 = env.GetFloatDefault("FMSG_MAX_FUTURE_TIME_DELTA", 300)
var MinDownloadRate = env.GetFloatDefault("FMSG_MIN_DOWNLOAD_RATE", 5000)
var MinUploadRate = env.GetFloatDefault("FMSG_MIN_UPLOAD_RATE", 5000)
var ReadBufferSize = env.GetIntDefault("FMSG_READ_BUFFER_SIZE", 1600)
var MaxMessageSize = uint32(env.GetIntDefault("FMSG_MAX_MSG_SIZE", 1024 * 10))
var DataDir = "got on startup"
var Domain = "got on startup"

// outgoing message headers keyed on header hash
var outgoing map[[32]byte]*FMsgHeader

// Updates DataDir from environment, panics if not a valid directory
func setDataDir() {
    value, hasValue := os.LookupEnv("FMSG_DATA_DIR")
	if !hasValue {
        log.Panic("ERROR: FMSG_DATA_DIR not set")
    }
	_, err := os.Stat(value)
	if err != nil {
		log.Panicf("ERROR: FMSG_DATA_DIR, %s: %s", value, err)
	}
    DataDir = value
}

// Updates Domain from environment, panics if not a valid domain
func setDomain() {
    domain, hasValue := os.LookupEnv("FMSG_DOMAIN")
	if !hasValue {
        log.Panic("ERROR: FMSG_DOMAIN not set")
    }
    _, err := net.LookupHost(domain)
    if err != nil {
        log.Panicf("ERROR: FMSG_DOMAIN, %s: %s", domain, err)
    }
	Domain = domain
}

func calcDuration(sizeInBytes int, bytesPerSecond float64) (time.Duration) {
	rate := float64(sizeInBytes) / bytesPerSecond
	return time.Duration(rate * float64(time.Second))
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
	fmt.Printf("<-- %s", hex.EncodeToString(hashSlice))
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
	
	d := time.Duration(20 * time.Second) // TODO calc?
	c.SetReadDeadline(time.Now().Add(d))

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
	if flags&HasPid == 1 {
		pid, err := io.ReadAll(io.LimitReader(c, 32))
		if err != nil {
			return h, err
		}
		h.Pid = make([]byte, 32)
		copy(h.Pid, pid)
		// TODO verify pid exists
	}

	// read from address
	from, err := readAddress(r)
	if err != nil {
		return h, err
	}
	log.Printf("from: @%s@%s", from.User, from.Domain)
	h.From = *from

	// read to addresses TODO validate addresses are unique
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
	now := timeutil.TimestampNow().Float64()
	delta := now - h.Timestamp
	if PastTimeDelta > 0 && delta < 0 {
		if math.Abs(delta) > PastTimeDelta {
			codes := []byte{ RejectCodePastTime }
			if err := rejectAccept(c, codes); err != nil {
				return h, err
			}
			return h, fmt.Errorf("message timestamp: %f too far in past, delta: %fs", h.Timestamp, delta)
		}
	}
	if FutureTimeDelta > 0 && delta > FutureTimeDelta {
		codes := []byte{ RejectCodeFutureTime }
		if err := rejectAccept(c, codes); err != nil {
			return h, err
		}
		return h, fmt.Errorf("message timestamp: %f too far in future, delta: %fs", h.Timestamp, delta)
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
	h.Type = string(mime)

	// read message size
	if err := binary.Read(r, binary.LittleEndian, &h.Size); err != nil {
		return h, err
	}
	if h.Size > MaxMessageSize {
		codes := []byte{ RejectCodeTooBig }
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
	conn2, err := net.Dial("tcp", fmt.Sprintf("%s:%d", h.From.Domain, RemotePort))
	if err != nil {
		return err
	}
	version := uint8(255)
	if err := binary.Write(conn2, binary.LittleEndian, version); err != nil {
		return err
	}
	hash := h.GetHeaderHash()
	fmt.Printf("--> CHALLENGE %s\n", hex.EncodeToString(hash))
	if _, err := conn2.Write(hash); err != nil {
		return err
	}

	// read challenge response
	resp, err := io.ReadAll(io.LimitReader(conn2, 32))
	if err != nil {
		return err
	}
	copy(h.ChallengeHash[:], resp)

	// gracefully close 2nd connection
	if err := conn2.Close(); err != nil {
		return err
	}

	return nil
}

func downloadMessage(c net.Conn, h *FMsgHeader) error {

	// first download to temp file
	addrs := []FMsgAddress{}
	for _, addr := range h.To {
		if addr.Domain == Domain {
			addrs = append(addrs, addr)
		}
	}
	if len(addrs) == 0 {
		return fmt.Errorf("our domain: %s, not in recipient list: %s", Domain, h.To)
	}
	codes := make([]byte, len(addrs))
	fd, err := os.CreateTemp("", "fmsg-download-*")
	if err != nil {
		return err
	}
	defer os.Remove(fd.Name())
	defer fd.Close()

	_, err = io.CopyN(fd, c, int64(h.Size))
	if err != nil {
		return err
	}

	// TODO attachments

	// check checksum of downloaded message matches challenge resp.
	h.Filepath = fd.Name()
	msgHash, err := h.GetMessageHash()
	if err != nil {
		return err
	}
	if !bytes.Equal(h.ChallengeHash[:], msgHash) {
		return fmt.Errorf("actual hash doesn't match challenge response: %s %s", msgHash, h.ChallengeHash)
	}

	// calc file extenstion from mime type
	exts, _ := mime.ExtensionsByType(h.Type)
	var ext string
	if exts == nil {
		ext = ".unknown"
	} else {
		ext = exts[0]
	}

	// copy to each recipient's directory
	for i, addr := range addrs {
		// TODO check disk space and user quota
		dirpath := filepath.Join(DataDir, addr.Domain, addr.User, InboxDirName)
		fp := filepath.Join(dirpath, fmt.Sprintf("%d", uint32(h.Timestamp))+ext)
		err := os.MkdirAll(dirpath, 0750) // TODO review perm
		if err != nil {
			return err
		}
		fd2, err := os.Create(fp)
		if err != nil {
			return err
		}
		defer fd2.Close()
		_, err = io.Copy(fd2, fd)
		if err != nil {
			log.Printf("Error copying downloaded message from: %s, to: %s\n", fd.Name(), fd2.Name())
			codes[i] = RejectUndisclosed // TODO better error ?
		} else {
			h.Filepath = fp
			err = storeMsgDetail(h)
			if err != nil {
				log.Printf("Error storing message: %s\n", err)
				codes[i] = RejectUndisclosed // TODO better error ?
			} else {
				codes[i] = RejectAccept
			}
		}
	}

	return rejectAccept(c, codes)
}

func handleConn(c net.Conn) {
	defer func() {
        if r := recover(); r != nil {
            log.Println("Recovered in handleConn", r)
        }
    }()

	log.Printf("Connection from: %s\n", c.RemoteAddr().String())

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

	// challenge
	err = challenge(c, header)
	if err != nil {
		log.Printf("Challenge failed from, %s: %s", c.RemoteAddr().String(), err)
		return
	}

	// store message
	d := calcDuration(int(header.Size), MinDownloadRate)
	c.SetReadDeadline(time.Now().Add(d))
	err = downloadMessage(c, header)
	if err != nil {
		log.Printf("Download failed form, %s: %s", c.RemoteAddr().String(), err)
		return
	}

	// gracefully close 1st connection
	c.Close()
}

func main() {

	outgoing = make(map[[32]byte]*FMsgHeader)

	log.SetPrefix("fmsg-host: ")

	// get listen address from args
	if len(os.Args) < 2 {
		log.Fatalf("ERROR: Listen address is required as an argument, e.g.: \"0.0.0.0\"\n")
	}
	listenAddress := os.Args[1]

	// initalize database
	err := initDb()
	if err != nil {
		log.Fatalf("ERROR: initalizing database: %s\n", err)
	}
	log.Println("INFO: Initalized database")

	// set data dir and domain from env
	setDataDir()
	setDomain()

	// start listening TODO start sending
	addr := fmt.Sprintf("%s:%d", listenAddress, Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	for {
		log.Printf("Listening on %s\n", addr)
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("ERROR: Accept connection from %s returned: %s\n", ln.Addr().String(), err)
		} else {
			go handleConn(conn)
		}
	}
}
