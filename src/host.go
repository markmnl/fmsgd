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

	// Flag bit assignments per SPEC.md:
	// bit 0 = has pid, bit 1 = has add to, bit 2 = common type, bit 3 = important,
	// bit 4 = no reply, bit 5 = deflate, bits 6-7 = reserved.
	FlagHasPid     uint8 = 1
	FlagHasAddTo   uint8 = 1 << 1
	FlagCommonType uint8 = 1 << 2
	FlagImportant  uint8 = 1 << 3
	FlagNoReply    uint8 = 1 << 4
	FlagDeflate    uint8 = 1 << 5

	// Attachment flag bit assignments per SPEC.md:
	// bit 0 = common type, bit 1 = deflate, bits 2-7 = reserved.
	AttachmentFlagCommonType uint8 = 1
	AttachmentFlagDeflate    uint8 = 1 << 1

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
	AcceptCodeAddTo                uint8 = 11

	RejectCodeUserUnknown      uint8 = 100
	RejectCodeUserFull         uint8 = 101
	RejectCodeUserNotAccepting uint8 = 102
	RejectCodeUserUndisclosed  uint8 = 103

	RejectCodeAccept uint8 = 200
)

// responseCodeName returns the human-friendly name for a response code.
func responseCodeName(code uint8) string {
	switch code {
	case RejectCodeInvalid:
		return "invalid"
	case RejectCodeUnsupportedVersion:
		return "unsupported version"
	case RejectCodeUndisclosed:
		return "undisclosed"
	case RejectCodeTooBig:
		return "too big"
	case RejectCodeInsufficentResources:
		return "insufficient resources"
	case RejectCodeParentNotFound:
		return "parent not found"
	case RejectCodePastTime:
		return "past time"
	case RejectCodeFutureTime:
		return "future time"
	case RejectCodeTimeTravel:
		return "time travel"
	case RejectCodeDuplicate:
		return "duplicate"
	case AcceptCodeAddTo:
		return "accept add to"
	case RejectCodeUserUnknown:
		return "user unknown"
	case RejectCodeUserFull:
		return "user full"
	case RejectCodeUserNotAccepting:
		return "user not accepting"
	case RejectCodeUserUndisclosed:
		return "user undisclosed"
	case RejectCodeAccept:
		return "accept"
	default:
		return fmt.Sprintf("unknown(%d)", code)
	}
}

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
var SkipAuthorisedIPs = false
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
	SkipAuthorisedIPs = os.Getenv("FMSG_SKIP_AUTHORISED_IPS") == "true"
}

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
	if s == "localhost" {
		return true
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
	hashSlice, err := io.ReadAll(io.LimitReader(r, 32))
	if err != nil {
		return err
	}
	hash := *(*[32]byte)(hashSlice) // get the underlying array (alternatively we could use hex strings..)
	log.Printf("INFO: CHALLENGE <-- %s", hex.EncodeToString(hashSlice))
	header, exists := lookupOutgoing(hash)
	if !exists {
		return fmt.Errorf("challenge for unknown message: %s, from: %s\n", hex.EncodeToString(hashSlice), c.RemoteAddr().String())
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

func readHeader(c net.Conn) (*FMsgHeader, *bufio.Reader, error) {
	r := bufio.NewReaderSize(c, ReadBufferSize)
	var h = &FMsgHeader{}

	d := calcNetIODuration(66000, MinDownloadRate) // max possible header size
	c.SetReadDeadline(time.Now().Add(d))

	// read version
	v, err := r.ReadByte()
	if err != nil {
		return h, r, err
	}
	// TODO [Spec 1.3]: Per spec step 1.3, the first byte determines the message type:
	//  - 1..127: fmsg version (currently only version 1 supported).
	//  - 128..255: incoming CHALLENGE; the fmsg version is (256 - value).
	//    E.g. 255 = challenge for version 1, 254 = challenge for version 2, etc.
	//  Currently only v==255 is handled as a challenge. This should be generalised
	//  to handle any value > 128 where (256 - value) is a supported version.
	//  For unsupported versions or invalid first-byte values, Host B MUST send
	//  REJECT code 2 (unsupported version) on the connection before closing.
	if v == 255 {
		return nil, r, handleChallenge(c, r)
	}
	if v != 1 {
		// TODO [Spec 1.3.iii]: Send REJECT code 2 (unsupported version) on the
		// connection before returning/closing, per spec step 1.3.iii.
		return h, r, fmt.Errorf("unsupported version: %d", v)
	}
	h.Version = v

	// read flags
	flags, err := r.ReadByte()
	if err != nil {
		return h, r, err
	}
	h.Flags = flags

	// read pid if any
	if flags&FlagHasPid == 1 {
		pid, err := io.ReadAll(io.LimitReader(r, 32))
		if err != nil {
			return h, r, err
		}
		h.Pid = make([]byte, 32)
		copy(h.Pid, pid)
		// TODO [Spec 1.4.v]: pid verification depends on the add-to field and must
		// be performed here (during header exchange), not deferred to downloadMessage.
		// See step 1.4.v for the full decision tree.
	}

	// read from address
	from, err := readAddress(r)
	if err != nil {
		return h, r, err
	}

	h.From = *from

	// read to addresses
	num, err := r.ReadByte()
	if err != nil {
		return h, r, err
	}
	// TODO [Spec 1.4.i.a]: Spec requires at least one address in "to". If num == 0,
	// reject with code 1 (invalid).
	seen := make(map[string]bool)
	for num > 0 {
		addr, err := readAddress(r)
		if err != nil {
			return h, r, err
		}
		key := strings.ToLower(addr.ToString())
		if seen[key] {
			return h, r, fmt.Errorf("duplicate recipient address: %s", addr.ToString())
		}
		seen[key] = true
		h.To = append(h.To, *addr)
		num--
	}

	// parse "add to" field when has-add-to flag (bit 1) is set
	if flags&FlagHasAddTo != 0 {
		// add to requires pid per spec 1.4.v.b.a
		if flags&FlagHasPid == 0 {
			codes := []byte{RejectCodeInvalid}
			if err := rejectAccept(c, codes); err != nil {
				return h, r, err
			}
			return h, r, fmt.Errorf("add to exists but pid does not")
		}

		addToCount, err := r.ReadByte()
		if err != nil {
			return h, r, err
		}
		if addToCount == 0 {
			codes := []byte{RejectCodeInvalid}
			if err := rejectAccept(c, codes); err != nil {
				return h, r, err
			}
			return h, r, fmt.Errorf("add to flag set but count is 0")
		}
		addToSeen := make(map[string]bool)
		for addToCount > 0 {
			addr, err := readAddress(r)
			if err != nil {
				return h, r, err
			}
			key := strings.ToLower(addr.ToString())
			if addToSeen[key] {
				codes := []byte{RejectCodeInvalid}
				if err := rejectAccept(c, codes); err != nil {
					return h, r, err
				}
				return h, r, fmt.Errorf("duplicate recipient address in add to: %s", addr.ToString())
			}
			addToSeen[key] = true
			h.AddTo = append(h.AddTo, *addr)
			addToCount--
		}
	}

	// read timestamp
	if err := binary.Read(r, binary.LittleEndian, &h.Timestamp); err != nil {
		return h, r, err
	}
	now := timeutil.TimestampNow().Float64()
	delta := now - h.Timestamp
	if PastTimeDelta > 0 && delta > PastTimeDelta {
		codes := []byte{RejectCodePastTime}
		if err := rejectAccept(c, codes); err != nil {
			return h, r, err
		}
		return h, r, fmt.Errorf("message timestamp: %f too far in past, delta: %fs", h.Timestamp, delta)
	}
	if FutureTimeDelta > 0 && delta < 0 && math.Abs(delta) > FutureTimeDelta {
		codes := []byte{RejectCodeFutureTime}
		if err := rejectAccept(c, codes); err != nil {
			return h, r, err
		}
		return h, r, fmt.Errorf("message timestamp: %f too far in future, delta: %fs", h.Timestamp, delta)
	}

	// read topic — only present when pid is NOT set (first message in a thread)
	if flags&FlagHasPid == 0 {
		topic, err := ReadUInt8Slice(r)
		if err != nil {
			return h, r, err
		}
		h.Topic = string(topic)
	}

	// read type: when common-type flag is set, the field is a single uint8
	// index into the Common Media Types table; otherwise uint8 length + ASCII string.
	if flags&FlagCommonType != 0 {
		typeNum, err := r.ReadByte()
		if err != nil {
			return h, r, err
		}
		typStr, ok := numberToMediaType[typeNum]
		if !ok {
			codes := []byte{RejectCodeInvalid}
			if err := rejectAccept(c, codes); err != nil {
				return h, r, err
			}
			return h, r, fmt.Errorf("unknown common type number: %d", typeNum)
		}
		h.Type = typStr
	} else {
		mime, err := ReadUInt8Slice(r)
		if err != nil {
			return h, r, err
		}
		h.Type = string(mime)
	}

	// read message size
	if err := binary.Read(r, binary.LittleEndian, &h.Size); err != nil {
		return h, r, err
	}
	// TODO [Spec 1.4.iii]: Size check must be total of data size PLUS all
	// attachment sizes, not just data size alone. Currently only h.Size (data)
	// is checked against MaxMessageSize.
	if h.Size > MaxMessageSize {
		codes := []byte{RejectCodeTooBig}
		if err := rejectAccept(c, codes); err != nil {
			return h, r, err
		}
		return h, r, fmt.Errorf("message size: %d exceeds max: %d", h.Size, MaxMessageSize)
	}

	// read attachment count
	var attachCount uint8
	if err := binary.Read(r, binary.LittleEndian, &attachCount); err != nil {
		return h, r, err
	}
	// TODO [Spec 1.4.i.c / attachments]: Parse attachment headers (flags, type,
	// filename, size) for each attachment. Validate:
	//  - Each attachment's type when its own common-type flag is set exists in
	//    Common Media Type mapping; else reject code 1 (invalid).
	//  - Filename is valid UTF-8 per spec (Unicode letters/numbers, limited
	//    special chars, unique case-insensitive, < 256 bytes).
	//  - Incorporate attachment sizes into the MAX_SIZE check above.
	//  - Store parsed attachment headers on FMsgHeader.
	if attachCount > 0 {
		return h, r, fmt.Errorf("attachments not yet supported (count: %d)", attachCount)
	}

	log.Printf("INFO: <-- MSG\n%s", h)

	// TODO [Spec 1.4.ii]: DNS verification of sender's IP address MUST be
	// performed here during header exchange (not deferred to the challenge
	// step). Host B must resolve _fmsg.<from-domain> and verify the incoming
	// connection's IP is in the authorised set. If not authorised, Host B MUST
	// TERMINATE the message exchange (abort connection, no reject code sent).
	// Currently this check only happens inside challenge() and is skipped
	// entirely when skip-challenge is allowed.

	// add-to pid decision tree per spec 1.4.v.b
	if len(h.AddTo) > 0 {
		// check if any add-to recipients are for our domain
		addToHasOurDomain := false
		for _, addr := range h.AddTo {
			if strings.EqualFold(addr.Domain, Domain) {
				addToHasOurDomain = true
				break
			}
		}
		if !addToHasOurDomain {
			// none of the add-to recipients are for our domain — record
			// the additional recipients and respond code 11.

			// pid must refer to a stored message per "Verifying Message Stored"
			parentID, err := lookupMsgIdByHash(h.Pid)
			if err != nil {
				return h, r, err
			}
			if parentID == 0 {
				codes := []byte{RejectCodeParentNotFound}
				if err := rejectAccept(c, codes); err != nil {
					return h, r, err
				}
				return h, r, fmt.Errorf("add-to notification: parent not found for pid %s", hex.EncodeToString(h.Pid))
			}

			// record add-to recipients for future hash reconstruction, respond code 11
			if err := storeMsgHeaderOnly(h); err != nil {
				codes := []byte{RejectCodeUndisclosed}
				if err2 := rejectAccept(c, codes); err2 != nil {
					return h, r, err2
				}
				return h, r, fmt.Errorf("add-to notification: storing header: %w", err)
			}
			codes := []byte{AcceptCodeAddTo}
			if err := rejectAccept(c, codes); err != nil {
				return h, r, err
			}
			log.Printf("INFO: additional recipients received (code 11) for pid %s", hex.EncodeToString(h.Pid))
			return nil, r, nil // nil header signals handled (like challenge)
		}
		// else: add-to recipients are for our domain, continue normally
		// (pid message does NOT have to be already stored per spec 1.4.v.b.b)
	}

	// TODO [Spec 1.4.v.c]: When pid exists and add-to does not:
	//  a. The message or message header pid refers to MUST be verified as stored
	//     per "Verifying Message Stored"; else reject code 6 (parent not found).
	//     "Verifying Message Stored" means: the digest matches a previously
	//     accepted message (code 200) or accepted header (code 11), AND the
	//     corresponding data currently exists on the host.
	//  b. The stored parent's time MUST be before the incoming message's time;
	//     else reject code 9 (time travel).
	//  Both checks should be here (step 1), not deferred to downloadMessage.

	return h, r, nil
}

// Sends CHALLENGE request to sender domain first checking if domain is indeed located
// at address in connection supplied by verifying the remote IP is in the
// sender's _fmsg.<sender-domain> authorised IP set.
// TODO [Spec step 2]: The spec defines challenge modes (NEVER, ALWAYS,
// HAS_NOT_PARTICIPATED, DIFFERENT_DOMAIN) as implementation choices.
// Currently defaults to ALWAYS. Implement configurable challenge mode.
func challenge(conn net.Conn, h *FMsgHeader) error {

	// verify remote IP is authorised by sender's _fmsg DNS record
	if SkipAuthorisedIPs {
		log.Println("WARN: skipping authorised IP check (FMSG_SKIP_AUTHORISED_IPS=true)")
	} else {
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
	}

	// Connection 2 MUST target the same IP as Connection 1 (spec 2.1).
	remoteHost, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return fmt.Errorf("failed to parse remote address for challenge: %w", err)
	}
	conn2, err := net.Dial("tcp", net.JoinHostPort(remoteHost, fmt.Sprintf("%d", RemotePort)))
	if err != nil {
		return err
	}
	version := uint8(255)
	if err := binary.Write(conn2, binary.LittleEndian, version); err != nil {
		return err
	}
	hash := h.GetHeaderHash()
	log.Printf("INFO: --> CHALLENGE\t%s\n", hex.EncodeToString(hash))
	if _, err := conn2.Write(hash); err != nil {
		return err
	}

	// read challenge response
	resp, err := io.ReadAll(io.LimitReader(conn2, 32))
	if err != nil {
		return err
	}
	copy(h.ChallengeHash[:], resp)
	log.Printf("INFO: <-- CHALLENGE RESP\t%s\n", hex.EncodeToString(resp))

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
		return RejectCodeUserNotAccepting, nil
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

func downloadMessage(c net.Conn, r io.Reader, h *FMsgHeader) error {

	// TODO [Spec 3.1]: Per spec step 3.1, BEFORE downloading the remaining
	// message data, if the CHALLENGE/CHALLENGE-RESP exchange was completed the
	// message hash from the CHALLENGE-RESP SHOULD be used to check if the
	// message is already stored (duplicate check). If found, respond REJECT
	// code 10 (duplicate) and close. Currently the duplicate check happens
	// AFTER downloading all data, wasting bandwidth on duplicates.

	// filter to our domain recipients in "to" then "add to" order per spec 3.4.i
	addrs := []FMsgAddress{}
	for _, addr := range h.To {
		if strings.EqualFold(addr.Domain, Domain) {
			addrs = append(addrs, addr)
		}
	}
	for _, addr := range h.AddTo {
		if strings.EqualFold(addr.Domain, Domain) {
			addrs = append(addrs, addr)
		}
	}
	if len(addrs) == 0 {
		return fmt.Errorf("%w our domain: %s, not in recipient list", ErrProtocolViolation, Domain)
	}
	codes := make([]byte, len(addrs))

	// download to temp file
	fd, err := os.CreateTemp("", "fmsg-download-*")
	if err != nil {
		return err
	}
	defer os.Remove(fd.Name())
	defer fd.Close()

	_, err = io.CopyN(fd, r, int64(h.Size))
	if err != nil {
		return err
	}

	// TODO [Spec 3.2]: Also download attachment data (sequential byte sequences
	// whose boundaries are defined by attachment header sizes). Currently only
	// message body data is downloaded; attachment bodies are not handled.

	// verify hash matches challenge response
	// TODO [Spec 3.3]: This verification MUST only be performed when the
	// CHALLENGE/CHALLENGE-RESP exchange was actually completed. If the challenge
	// was skipped (or not issued), ChallengeHash will be zero-valued and this
	// comparison will erroneously fail for every message. Guard this check behind
	// a flag or sentinel indicating whether the challenge exchange occurred.
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
	// TODO [Spec 1.4.v.c]: This pid verification should happen during header
	// exchange (readHeader / step 1.4.v), not after downloading the message.
	// Additionally, the spec requires checking the parent's timestamp is before
	// the current message's timestamp (reject code 9 = time travel), and the
	// verification must follow the "Verifying Message Stored" semantics:
	//  - The digest must match a previously accepted message (code 200) or
	//    accepted add-to (code 11).
	//  - The message/header must currently exist and be retrievable.
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
	// Build a set of add-to addresses for later classification
	addToSet := make(map[string]bool)
	for _, addr := range h.AddTo {
		addToSet[strings.ToLower(addr.ToString())] = true
	}
	acceptedTo := []FMsgAddress{}
	acceptedAddTo := []FMsgAddress{}
	var primaryFilepath string
	for i, addr := range addrs {
		code, err := validateMsgRecvForAddr(h, &addr)
		if err != nil {
			return err
		}
		if code != RejectCodeAccept {
			log.Printf("WARN: Rejected message to: %s: %s (%d)", addr.ToString(), responseCodeName(code), code)
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
		if addToSet[strings.ToLower(addr.ToString())] {
			acceptedAddTo = append(acceptedAddTo, addr)
		} else {
			acceptedTo = append(acceptedTo, addr)
		}
		if primaryFilepath == "" {
			primaryFilepath = fp
		}
	}

	// store message details once for all accepted recipients
	if len(acceptedTo) > 0 || len(acceptedAddTo) > 0 {
		origTo := h.To
		origAddTo := h.AddTo
		h.To = acceptedTo
		h.AddTo = acceptedAddTo
		h.Filepath = primaryFilepath
		if err := storeMsgDetail(h); err != nil {
			log.Printf("ERROR: storing message: %s", err)
			h.To = origTo
			h.AddTo = origAddTo
			for i := range codes {
				if codes[i] == RejectCodeAccept {
					codes[i] = RejectCodeUndisclosed
				}
			}
		} else {
			h.To = origTo
			h.AddTo = origAddTo
			allAccepted := append(acceptedTo, acceptedAddTo...)
			for i := range allAccepted {
				if err := postMsgStatRecv(&allAccepted[i], h.Timestamp, int(h.Size)); err != nil {
					log.Printf("WARN: Failed to post msg recv stat: %s", err)
				}
			}
		}
	}

	return rejectAccept(c, codes)
}

func abortConn(c net.Conn) {
	if tcp, ok := c.(*net.TCPConn); ok {
		tcp.SetLinger(0)
	}
	_ = c.Close()
}

func handleConn(c net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("ERROR: Recovered in handleConn: %v", r)
		}
	}()

	log.Printf("INFO: Connection from: %s\n", c.RemoteAddr().String())

	// read header
	header, r, err := readHeader(c)
	if err != nil {
		log.Printf("WARN: reading header from, %s: %s", c.RemoteAddr().String(), err)
		abortConn(c)
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
		log.Printf("ERROR: Challenge failed to, %s: %s", c.RemoteAddr().String(), err)
		abortConn(c)
		return
	}

	// store message
	d := calcNetIODuration(int(header.Size), MinDownloadRate)
	c.SetReadDeadline(time.Now().Add(d))
	err = downloadMessage(c, r, header)
	if err != nil {
		// if error was a protocal violation, abort; otherise let sender know there was an internal error
		log.Printf("ERROR: Download failed from, %s: %s", c.RemoteAddr().String(), err)
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

	initOutgoing()

	// load environment variables from .env file if present
	if err := godotenv.Load(); err != nil {
		log.Printf("INFO: Could not load .env file: %v", err)
	}

	// read env config (must be after godotenv.Load)
	loadEnvConfig()

	// determine listen address from args
	listenAddress := "127.0.0.1"
	for _, arg := range os.Args[1:] {
		listenAddress = arg
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

	// start sender in background (small delay so listener is ready first)
	go func() {
		time.Sleep(1 * time.Second)
		startSender()
	}()

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
