package main

import (
	"bufio"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	env "github.com/caitlinelfring/go-env-default"
	"github.com/joho/godotenv"
	"github.com/levenlabs/golib/timeutil"
)

const (
	InboxDirName  = "in"
	OutboxDirName = "out"

	FlagHasPid        uint8 = 1
	FlagImportant     uint8 = 1 << 1
	FlagNoReply       uint8 = 1 << 3
	FlagSkipChallenge uint8 = 1 << 4
	FlagDeflate       uint8 = 1 << 5

	RejectCodeInvalid              uint8 = 1
	RejectCodeUnsupportedVersion   uint8 = 2
	RejectCodeUndisclosed          uint8 = 3
	RejectCodeTooBig               uint8 = 4
	RejectCodeInsufficentResources uint8 = 5
	RejectCodeParentNotFound       uint8 = 6
	RejectCodePastTime             uint8 = 7
	RejectCodeFutureTime           uint8 = 8
	RejectCodeTimeTravel           uint8 = 9
	RejectCodeDuplicate            uint8 = 10
	RejectCodeMustChallenge        uint8 = 11
	RejectCodeCannotChallenge      uint8 = 12

	RejectCodeUserUnknown uint8 = 100
	RejectCodeUserFull    uint8 = 101

	RejectCodeAccept uint8 = 200
)

var ErrProtocolViolation = errors.New("protocol violation")

var Port = 4930

// The only reason RemotePort would ever be different from Port is when running two fmsg hosts on the same machine so the same port is unavaliable.
var RemotePort = 4930
var PastTimeDelta float64 = 7 * 24 * 60 * 60
var FutureTimeDelta float64 = 300
var MinDownloadRate float64 = 5000
var MinUploadRate float64 = 5000
var ReadBufferSize = 1600
var MaxMessageSize = uint32(1024 * 10)
var AllowSkipChallenge = 0
var DataDir = "got on startup"
var Domain = "got on startup"
var IDURI = "got on startup"
var AtRune, _ = utf8.DecodeRuneInString("@")
var MinNetIODeadline = 6 * time.Second

// loadEnvConfig reads env vars (after godotenv.Load so .env is picked up).
func loadEnvConfig() {
	Port = env.GetIntDefault("FMSG_PORT", 4930)
	RemotePort = env.GetIntDefault("FMSG_REMOTE_PORT", 4930)
	PastTimeDelta = env.GetFloatDefault("FMSG_MAX_PAST_TIME_DELTA", 7*24*60*60)
	FutureTimeDelta = env.GetFloatDefault("FMSG_MAX_FUTURE_TIME_DELTA", 300)
	MinDownloadRate = env.GetFloatDefault("FMSG_MIN_DOWNLOAD_RATE", 5000)
	MinUploadRate = env.GetFloatDefault("FMSG_MIN_UPLOAD_RATE", 5000)
	ReadBufferSize = env.GetIntDefault("FMSG_READ_BUFFER_SIZE", 1600)
	MaxMessageSize = uint32(env.GetIntDefault("FMSG_MAX_MSG_SIZE", 1024*10))
	AllowSkipChallenge = env.GetIntDefault("FMSG_ALLOW_SKIP_CHALLENGE", 0)
}

// outgoing message headers keyed on header hash
var outgoing map[[32]byte]*FMsgHeader

// Updates DataDir from environment, panics if not a valid directory.
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

// Updates Domain from environment, panics if not a valid domain.
func setDomain() {
	domain, hasValue := os.LookupEnv("FMSG_DOMAIN")
	if !hasValue {
		log.Panicln("ERROR: FMSG_DOMAIN not set")
	}
	_, err := net.LookupHost(domain)
	if err != nil {
		log.Panicf("ERROR: FMSG_DOMAIN, %s: %s\n", domain, err)
	}
	Domain = domain

	// verify our external IP is in the _fmsg authorised IP set
	checkDomainIP(domain)
}

// Updates IDURL from environment, panics if not valid.
func setIDURL() {
	rawUrl, hasValue := os.LookupEnv("FMSG_ID_URL")
	if !hasValue {
		log.Panicln("ERROR: FMSG_ID_URL not set")
	}
	url, err := url.Parse(rawUrl)
	if err != nil {
		log.Panicf("ERROR: FMSG_ID_URL not valid, %s: %s", rawUrl, err)
	}
	_, err = net.LookupHost(url.Hostname())
	if err != nil {
		log.Panicf("ERROR: FMSG_ID_URL lookup failed, %s: %s", url, err)
	}
	IDURI = rawUrl
	log.Printf("INFO: ID URL: %s", IDURI)
}

func calcNetIODuration(sizeInBytes int, bytesPerSecond float64) time.Duration {
	rate := float64(sizeInBytes) / bytesPerSecond
	d := time.Duration(rate * float64(time.Second))
	if d < MinNetIODeadline {
		return MinNetIODeadline
	}
	return d
}

func isValidUser(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func isValidDomain(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	labels := strings.Split(s, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-') {
				return false
			}
		}
	}
	return true
}

func parseAddress(b []byte) (*FMsgAddress, error) {
	if len(b) < 4 {
		return nil, fmt.Errorf("invalid address: too short (%d bytes)", len(b))
	}
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
	if !isValidUser(addr.User) {
		return addr, fmt.Errorf("invalid user in address: %s", addr.User)
	}
	addr.Domain = addrStr[lastAt+1:]
	if !isValidDomain(addr.Domain) {
		return addr, fmt.Errorf("invalid domain in address: %s", addr.Domain)
	}
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

	d := calcNetIODuration(66000, MinDownloadRate) // max possible header size
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
	if flags&FlagHasPid == 1 {
		pid, err := io.ReadAll(io.LimitReader(c, 32))
		if err != nil {
			return h, err
		}
		h.Pid = make([]byte, 32)
		copy(h.Pid, pid)
		// pid existence is verified in downloadMessage after hash check
	}

	// read from address
	from, err := readAddress(r)
	if err != nil {
		return h, err
	}
	log.Printf("from: %s", from.ToString())
	h.From = *from

	// read to addresses
	num, err := r.ReadByte()
	if err != nil {
		return h, err
	}
	seen := make(map[string]bool)
	for num > 0 {
		addr, err := readAddress(r)
		if err != nil {
			return h, err
		}
		key := strings.ToLower(addr.ToString())
		if seen[key] {
			return h, fmt.Errorf("duplicate recipient address: %s", addr.ToString())
		}
		seen[key] = true
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
			codes := []byte{RejectCodePastTime}
			if err := rejectAccept(c, codes); err != nil {
				return h, err
			}
			return h, fmt.Errorf("message timestamp: %f too far in past, delta: %fs", h.Timestamp, delta)
		}
	}
	if FutureTimeDelta > 0 && delta > FutureTimeDelta {
		codes := []byte{RejectCodeFutureTime}
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
		codes := []byte{RejectCodeTooBig}
		if err := rejectAccept(c, codes); err != nil {
			return h, err
		}
		return h, fmt.Errorf("message size: %d exceeds max: %d", h.Size, MaxMessageSize)
	}

	// read attachment count
	var attachCount uint8
	if err := binary.Read(r, binary.LittleEndian, &attachCount); err != nil {
		return h, err
	}
	if attachCount > 0 {
		return h, fmt.Errorf("attachments not yet supported (count: %d)", attachCount)
	}

	return h, nil
}

// Sends CHALLENGE request to sender domain first checking if domain is indeed located
// at address in connection supplied by verifying the remote IP is in the
// sender's _fmsg.<sender-domain> authorised IP set.
func challenge(conn net.Conn, h *FMsgHeader) error {

	// skip challenge if sender requested it and we allow it
	if h.Flags&FlagSkipChallenge != 0 {
		if AllowSkipChallenge == 1 {
			log.Printf("INFO: skip challenge requested and allowed from %s", conn.RemoteAddr().String())
			return nil
		}
		log.Printf("WARN: skip challenge requested but not allowed from %s", conn.RemoteAddr().String())
		rejectAccept(conn, []byte{RejectCodeMustChallenge})
		return fmt.Errorf("skip challenge requested but not allowed")
	}

	// verify remote IP is authorised by sender's _fmsg DNS record
	remoteHost, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return fmt.Errorf("failed to parse remote address: %w", err)
	}
	remoteIP := net.ParseIP(remoteHost)
	if remoteIP == nil {
		return fmt.Errorf("failed to parse remote IP: %s", remoteHost)
	}

	authorisedIPs, err := lookupAuthorisedIPs(h.From.Domain)
	if err != nil {
		return err
	}

	found := false
	for _, ip := range authorisedIPs {
		if remoteIP.Equal(ip) {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("remote address %s not in _fmsg.%s authorised IPs", remoteIP.String(), h.From.Domain)
	}

	// okay lets give sender a call and confirm they are sending this message
	conn2, err := net.Dial("tcp", net.JoinHostPort(h.From.Domain, fmt.Sprintf("%d", RemotePort)))
	if err != nil {
		return err
	}
	version := uint8(255)
	if err := binary.Write(conn2, binary.LittleEndian, version); err != nil {
		return err
	}
	hash := h.GetHeaderHash()
	fmt.Printf("--> CHALLENGE\t%s\n", hex.EncodeToString(hash))
	if _, err := conn2.Write(hash); err != nil {
		return err
	}

	// read challenge response
	resp, err := io.ReadAll(io.LimitReader(conn2, 32))
	if err != nil {
		return err
	}
	copy(h.ChallengeHash[:], resp)
	fmt.Printf("--> RESP\t%s\n", hex.EncodeToString(resp))

	// gracefully close 2nd connection
	if err := conn2.Close(); err != nil {
		return err
	}

	return nil
}

func validateMsgRecvForAddr(h *FMsgHeader, addr *FMsgAddress) (code uint8, err error) {
	detail, err := getAddressDetail(addr)
	if err != nil {
		return RejectCodeUndisclosed, err
	}
	if detail == nil {
		return RejectCodeUserUnknown, nil
	}

	// check user accepting new
	if !detail.AcceptingNew {
		return RejectCodeUserUnknown, nil
	}

	// check user limits
	if detail.LimitRecvCountPer1d > -1 && detail.RecvCountPer1d+1 > detail.LimitRecvCountPer1d {
		log.Printf("WARN: Message rejected: RecvCountPer1d would exceed LimitRecvCountPer1d %d", detail.LimitRecvCountPer1d)
		return RejectCodeUserFull, nil
	}
	if detail.LimitRecvSizePer1d > -1 && detail.RecvSizePer1d+int64(h.Size) > detail.LimitRecvSizePer1d {
		log.Printf("WARN: Message rejected: RecvSizePer1d would exceed LimitRecvSizePer1d %d", detail.LimitRecvSizePer1d)
		return RejectCodeUserFull, nil
	}
	if detail.LimitRecvSizeTotal > -1 && detail.RecvSizeTotal+int64(h.Size) > detail.LimitRecvSizeTotal {
		log.Printf("WARN: Message rejected: RecvSizeTotal would exceed LimitRecvSizeTotal %d", detail.LimitRecvSizeTotal)
		return RejectCodeUserFull, nil
	}

	return RejectCodeAccept, nil
}

// uniqueFilepath generates a unique file path in the given directory,
// appending a counter suffix if the base name already exists.
func uniqueFilepath(dir string, timestamp uint32, ext string) string {
	base := fmt.Sprintf("%d", timestamp)
	fp := filepath.Join(dir, base+ext)
	if _, err := os.Stat(fp); os.IsNotExist(err) {
		return fp
	}
	for i := 1; ; i++ {
		fp = filepath.Join(dir, fmt.Sprintf("%s_%d%s", base, i, ext))
		if _, err := os.Stat(fp); os.IsNotExist(err) {
			return fp
		}
	}
}

func downloadMessage(c net.Conn, h *FMsgHeader) error {

	// filter to local domain recipients
	addrs := []FMsgAddress{}
	for _, addr := range h.To {
		if addr.Domain == Domain {
			addrs = append(addrs, addr)
		}
	}
	if len(addrs) == 0 {
		return fmt.Errorf("%w our domain: %s, not in recipient list: %s", ErrProtocolViolation, Domain, h.To)
	}
	codes := make([]byte, len(addrs))

	// download to temp file
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

	// verify hash matches challenge response
	h.Filepath = fd.Name()
	msgHash, err := h.GetMessageHash()
	if err != nil {
		return err
	}
	if !bytes.Equal(h.ChallengeHash[:], msgHash) {
		challengeHashStr := hex.EncodeToString(h.ChallengeHash[:])
		actualHashStr := hex.EncodeToString(msgHash)
		return fmt.Errorf("%w actual hash: %s mismatch challenge response: %s", ErrProtocolViolation, actualHashStr, challengeHashStr)
	}

	// check for duplicate message
	dupID, err := lookupMsgIdByHash(msgHash)
	if err != nil {
		return err
	}
	if dupID > 0 {
		for i := range codes {
			codes[i] = RejectCodeDuplicate
		}
		return rejectAccept(c, codes)
	}

	// verify parent exists if pid is set
	if h.Flags&FlagHasPid != 0 {
		parentID, err := lookupMsgIdByHash(h.Pid)
		if err != nil {
			return err
		}
		if parentID == 0 {
			for i := range codes {
				codes[i] = RejectCodeParentNotFound
			}
			return rejectAccept(c, codes)
		}
	}

	// determine file extension from MIME type
	exts, _ := mime.ExtensionsByType(h.Type)
	var ext string
	if exts == nil {
		ext = ".unknown"
	} else {
		ext = exts[0]
	}

	// validate each recipient and copy message for accepted ones
	acceptedAddrs := []FMsgAddress{}
	var primaryFilepath string
	for i, addr := range addrs {
		code, err := validateMsgRecvForAddr(h, &addr)
		if err != nil {
			return err
		}
		if code != RejectCodeAccept {
			log.Printf("WARN: Rejected message to: %s, code: %d", addr.ToString(), code)
			codes[i] = code
			continue
		}

		// copy to recipient's directory
		dirpath := filepath.Join(DataDir, addr.Domain, addr.User, InboxDirName)
		if err := os.MkdirAll(dirpath, 0750); err != nil {
			return err
		}

		fp := uniqueFilepath(dirpath, uint32(h.Timestamp), ext)
		if _, err := fd.Seek(0, io.SeekStart); err != nil {
			return err
		}

		fd2, err := os.Create(fp)
		if err != nil {
			return err
		}

		var copyErr error
		if h.Flags&FlagDeflate != 0 {
			fr := flate.NewReader(fd)
			_, copyErr = io.Copy(fd2, fr)
			fr.Close()
		} else {
			_, copyErr = io.Copy(fd2, fd)
		}
		fd2.Close()

		if copyErr != nil {
			log.Printf("ERROR: copying downloaded message from: %s, to: %s", fd.Name(), fp)
			os.Remove(fp)
			codes[i] = RejectCodeUndisclosed
			continue
		}

		codes[i] = RejectCodeAccept
		acceptedAddrs = append(acceptedAddrs, addr)
		if primaryFilepath == "" {
			primaryFilepath = fp
		}
	}

	// store message details once for all accepted recipients
	if len(acceptedAddrs) > 0 {
		origTo := h.To
		h.To = acceptedAddrs
		h.Filepath = primaryFilepath
		if err := storeMsgDetail(h); err != nil {
			log.Printf("ERROR: storing message: %s", err)
			h.To = origTo
			for i := range codes {
				if codes[i] == RejectCodeAccept {
					codes[i] = RejectCodeUndisclosed
				}
			}
		} else {
			h.To = origTo
			for i := range acceptedAddrs {
				if err := postMsgStatRecv(&acceptedAddrs[i], h.Timestamp, int(h.Size)); err != nil {
					log.Printf("WARN: Failed to post msg recv stat: %s", err)
				}
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
		log.Printf("Challenge failed to, %s: %s", c.RemoteAddr().String(), err)
		return
	}

	// store message
	d := calcNetIODuration(int(header.Size), MinDownloadRate)
	c.SetReadDeadline(time.Now().Add(d))
	err = downloadMessage(c, header)
	if err != nil {
		// if error was a protocal violation, abort; otherise let sender know there was an internal error
		log.Printf("Download failed from, %s: %s", c.RemoteAddr().String(), err)
		if errors.Is(err, ErrProtocolViolation) {
			return
		} else {
			rejectAccept(c, []byte{RejectCodeUndisclosed})
		}
	}

	// gracefully close 1st connection
	c.Close()
}

func main() {

	outgoing = make(map[[32]byte]*FMsgHeader)

	log.SetPrefix("fmsgd: ")

	// load environment variables from .env file if present
	if err := godotenv.Load(); err != nil {
		log.Printf("INFO: Could not load .env file: %v", err)
	}

	// read env config (must be after godotenv.Load)
	loadEnvConfig()

	// determine mode from args: "sender" or default receiver
	mode := "receiver"
	listenAddress := "127.0.0.1"
	for _, arg := range os.Args[1:] {
		if arg == "sender" {
			mode = "sender"
		} else {
			listenAddress = arg
		}
	}

	// initalize database
	err := testDb()
	if err != nil {
		log.Fatalf("ERROR: connecting to database: %s\n", err)
	}

	// set DataDir, Domain and IDURL from env
	setDataDir()
	setDomain()
	setIDURL()

	if mode == "sender" {
		startSender() // blocks forever
	} else {
		// start listening
		addr := fmt.Sprintf("%s:%d", listenAddress, Port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("INFO: Ready to receive on %s\n", addr)
		for {
			conn, err := ln.Accept()
			if err != nil {
				log.Printf("ERROR: Accept connection from %s returned: %s\n", ln.Addr().String(), err)
			} else {
				go handleConn(conn)
			}
		}
	}

}
