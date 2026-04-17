package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/tls"
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
	"unicode"
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
	AcceptCodeContinue             uint8 = 64
	AcceptCodeSkipData             uint8 = 65

	RejectCodeUserUnknown      uint8 = 100
	RejectCodeUserFull         uint8 = 101
	RejectCodeUserNotAccepting uint8 = 102
	RejectCodeUserDuplicate    uint8 = 103
	RejectCodeUserUndisclosed  uint8 = 105

	RejectCodeAccept uint8 = 200

	messageReservedBitsMask    uint8 = 0b11000000
	attachmentReservedBitsMask uint8 = 0b11111100
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
	case RejectCodeUserDuplicate:
		return "user duplicate"
	case RejectCodeUserUndisclosed:
		return "user undisclosed"
	case RejectCodeAccept:
		return "accept"
	default:
		return fmt.Sprintf("unknown(%d)", code)
	}
}

var ErrProtocolViolation = errors.New("protocol violation")

// commonMediaTypes maps common type IDs to their MIME strings per SPEC.md §4.
// IDs 1–64; unmapped IDs must be rejected with code 1 (invalid).
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

// getCommonMediaType returns the MIME type string for a common type ID, or
// empty string + false if the ID is not mapped (should be rejected per spec).
func getCommonMediaType(id uint8) (string, bool) {
	s, ok := commonMediaTypes[id]
	return s, ok
}

// getCommonMediaTypeID returns the common type ID for a MIME string.
func getCommonMediaTypeID(mediaType string) (uint8, bool) {
	for id, mime := range commonMediaTypes {
		if mime == mediaType {
			return id, true
		}
	}
	return 0, false
}

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
var TLSInsecureSkipVerify = false
var DataDir = "got on startup"
var Domain = "got on startup"
var IDURI = "got on startup"
var AtRune, _ = utf8.DecodeRuneInString("@")
var MinNetIODeadline = 6 * time.Second

var serverTLSConfig *tls.Config

func buildServerTLSConfig() *tls.Config {
	certFile := os.Getenv("FMSG_TLS_CERT")
	keyFile := os.Getenv("FMSG_TLS_KEY")
	if certFile == "" || keyFile == "" {
		log.Fatalf("ERROR: FMSG_TLS_CERT and FMSG_TLS_KEY must be set")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		log.Fatalf("ERROR: loading TLS certificate: %s", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		},
		NextProtos: []string{"fmsg/1"},
	}
}

func buildClientTLSConfig(serverName string) *tls.Config {
	return &tls.Config{
		ServerName:         serverName,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: TLSInsecureSkipVerify,
		NextProtos:         []string{"fmsg/1"},
	}
}

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
	TLSInsecureSkipVerify = os.Getenv("FMSG_TLS_INSECURE_SKIP_VERIFY") == "true"
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

	// verify our external IP is in the fmsg authorised IP set
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
	// TODO ping URL to verify its up and responding in a timely manner
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
	if !utf8.ValidString(s) || len(s) == 0 || len(s) > 64 {
		return false
	}

	isSpecial := func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	}

	runes := []rune(s)
	if isSpecial(runes[0]) || isSpecial(runes[len(runes)-1]) {
		return false
	}

	lastWasSpecial := false
	for _, c := range runes {
		if unicode.IsLetter(c) || unicode.IsNumber(c) {
			lastWasSpecial = false
			continue
		}
		if !isSpecial(c) {
			return false
		}
		if lastWasSpecial {
			return false
		}
		lastWasSpecial = true
	}
	return true
}

func isASCIIBytes(b []byte) bool {
	for _, c := range b {
		if c > 127 {
			return false
		}
	}
	return true
}

func isValidAttachmentFilename(name string) bool {
	if !utf8.ValidString(name) || len(name) == 0 || len(name) >= 256 {
		return false
	}

	isSpecial := func(r rune) bool {
		return r == '-' || r == '_' || r == ' ' || r == '.'
	}

	runes := []rune(name)
	if isSpecial(runes[0]) || isSpecial(runes[len(runes)-1]) {
		return false
	}

	lastWasSpecial := false
	for _, r := range runes {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			lastWasSpecial = false
			continue
		}
		if !isSpecial(r) {
			return false
		}
		if lastWasSpecial {
			return false
		}
		lastWasSpecial = true
	}

	return true
}

func isMessageRetrievable(msg *FMsgHeader) bool {
	if msg == nil {
		return false
	}
	if msg.Filepath != "" {
		st, err := os.Stat(msg.Filepath)
		if err == nil && !st.IsDir() {
			return true
		}
	}
	if len(msg.Pid) == 0 {
		return false
	}
	parentID, err := lookupMsgIdByHash(msg.Pid)
	if err != nil || parentID == 0 {
		return false
	}
	parentMsg, err := getMsgByID(parentID)
	if err != nil {
		return false
	}
	if parentMsg == nil {
		return false
	}
	return isMessageRetrievable(parentMsg)
}

func isParentParticipant(parent *FMsgHeader, addr *FMsgAddress) bool {
	if parent == nil || addr == nil {
		return false
	}
	target := strings.ToLower(addr.ToString())
	if strings.ToLower(parent.From.ToString()) == target {
		return true
	}
	for i := range parent.To {
		if strings.ToLower(parent.To[i].ToString()) == target {
			return true
		}
	}
	if parent.AddToFrom != nil && strings.ToLower(parent.AddToFrom.ToString()) == target {
		return true
	}
	for i := range parent.AddTo {
		if strings.ToLower(parent.AddTo[i].ToString()) == target {
			return true
		}
	}
	return false
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
	hash := *(*[32]byte)(hashSlice)
	log.Printf("INFO: CHALLENGE <-- %s", hex.EncodeToString(hashSlice))

	// Verify the challenger's IP is the Host-B IP registered for this message
	// (§10.5 step 2). An unrecognised hash OR a mismatched IP both → TERMINATE.
	remoteIP, _, _ := net.SplitHostPort(c.RemoteAddr().String())
	header, exists := lookupOutgoing(hash, remoteIP)
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

func sendCode(c net.Conn, code uint8) error {
	return rejectAccept(c, []byte{code})
}

func validateMessageFlags(c net.Conn, flags uint8) error {
	if flags&messageReservedBitsMask != 0 {
		if err := sendCode(c, RejectCodeInvalid); err != nil {
			return err
		}
		return fmt.Errorf("reserved message flag bits set: %#08b", flags)
	}
	return nil
}

func validateAttachmentFlags(c net.Conn, flags uint8) error {
	if flags&attachmentReservedBitsMask != 0 {
		if err := sendCode(c, RejectCodeInvalid); err != nil {
			return err
		}
		return fmt.Errorf("reserved attachment flag bits set: %#08b", flags)
	}
	return nil
}

func hasDomainRecipient(addrs []FMsgAddress, domain string) bool {
	for _, addr := range addrs {
		if strings.EqualFold(addr.Domain, domain) {
			return true
		}
	}
	return false
}

func determineSenderDomain(h *FMsgHeader) string {
	if len(h.AddTo) > 0 && h.AddToFrom != nil {
		return h.AddToFrom.Domain
	}
	return h.From.Domain
}

func verifySenderIP(c net.Conn, senderDomain string) error {
	if SkipAuthorisedIPs {
		return nil
	}

	remoteHost, _, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil {
		log.Printf("WARN: failed to parse remote address for DNS check: %s", err)
		return fmt.Errorf("DNS verification failed")
	}

	remoteIP := net.ParseIP(remoteHost)
	if remoteIP == nil {
		log.Printf("WARN: failed to parse remote IP: %s", remoteHost)
		return fmt.Errorf("DNS verification failed")
	}

	authorisedIPs, err := lookupAuthorisedIPs(senderDomain)
	if err != nil {
		log.Printf("WARN: DNS lookup failed for fmsg.%s: %s", senderDomain, err)
		return fmt.Errorf("DNS verification failed")
	}

	for _, ip := range authorisedIPs {
		if remoteIP.Equal(ip) {
			return nil
		}
	}

	log.Printf("WARN: remote IP %s not in authorised IPs for fmsg.%s", remoteIP.String(), senderDomain)
	return fmt.Errorf("DNS verification failed")
}

func handleAddToPath(c net.Conn, h *FMsgHeader) (*FMsgHeader, error) {
	if len(h.AddTo) == 0 {
		return h, nil
	}

	addToHasOurDomain := hasDomainRecipient(h.AddTo, Domain)

	parentID, err := lookupMsgIdByHash(h.Pid)
	if err != nil {
		return h, err
	}

	if parentID == 0 {
		h.InitialResponseCode = AcceptCodeContinue
		return h, nil
	}

	parentMsg, err := getMsgByID(parentID)
	if err != nil {
		return h, err
	}
	if parentMsg == nil || !isMessageRetrievable(parentMsg) {
		h.InitialResponseCode = AcceptCodeContinue
		return h, nil
	}

	if parentMsg.Timestamp-FutureTimeDelta > h.Timestamp {
		if err := sendCode(c, RejectCodeTimeTravel); err != nil {
			return h, err
		}
		return h, fmt.Errorf("add-to: time travel detected (parent time %f, current %f)", parentMsg.Timestamp, h.Timestamp)
	}

	if addToHasOurDomain {
		h.InitialResponseCode = AcceptCodeSkipData
		return h, nil
	}

	h.Filepath = parentMsg.Filepath
	for i := range h.Attachments {
		if i < len(parentMsg.Attachments) {
			h.Attachments[i].Filepath = parentMsg.Attachments[i].Filepath
		}
	}
	h.InitialResponseCode = AcceptCodeAddTo
	return h, nil
}

func validatePidReplyPath(c net.Conn, h *FMsgHeader) error {
	if len(h.AddTo) != 0 || h.Flags&FlagHasPid == 0 {
		return nil
	}

	parentID, err := lookupMsgIdByHash(h.Pid)
	if err != nil {
		return err
	}
	if parentID == 0 {
		if err := sendCode(c, RejectCodeParentNotFound); err != nil {
			return err
		}
		return fmt.Errorf("pid reply: parent not found for pid %s", hex.EncodeToString(h.Pid))
	}

	parentMsg, err := getMsgByID(parentID)
	if err != nil {
		return err
	}
	if parentMsg == nil {
		if err := sendCode(c, RejectCodeParentNotFound); err != nil {
			return err
		}
		return fmt.Errorf("pid reply: parent message not found by ID %d", parentID)
	}
	if !isMessageRetrievable(parentMsg) {
		if err := sendCode(c, RejectCodeParentNotFound); err != nil {
			return err
		}
		return fmt.Errorf("pid reply: parent is not retrievable for msg %d", parentID)
	}

	if parentMsg.Timestamp-FutureTimeDelta > h.Timestamp {
		if err := sendCode(c, RejectCodeTimeTravel); err != nil {
			return err
		}
		return fmt.Errorf("pid reply: time travel detected (parent time %f, current %f)", parentMsg.Timestamp, h.Timestamp)
	}
	if !isParentParticipant(parentMsg, &h.From) {
		if err := sendCode(c, RejectCodeInvalid); err != nil {
			return err
		}
		return fmt.Errorf("pid reply: sender %s was not a participant of parent", h.From.ToString())
	}

	return nil
}

func readVersionOrChallenge(c net.Conn, r *bufio.Reader, h *FMsgHeader) (bool, error) {
	v, err := r.ReadByte()
	if err != nil {
		return false, err
	}
	if v >= 129 {
		challengeVersion := 256 - int(v)
		if challengeVersion == 1 {
			return true, handleChallenge(c, r)
		}
		if err := sendCode(c, RejectCodeUnsupportedVersion); err != nil {
			log.Printf("WARN: failed to send unsupported version response: %s", err)
		}
		return false, fmt.Errorf("unsupported challenge version: %d", challengeVersion)
	}
	if v != 1 {
		if err := sendCode(c, RejectCodeUnsupportedVersion); err != nil {
			log.Printf("WARN: failed to send unsupported version response: %s", err)
		}
		return false, fmt.Errorf("unsupported message version: %d", v)
	}
	h.Version = v
	return false, nil
}

func readToRecipients(c net.Conn, r *bufio.Reader, h *FMsgHeader) (map[string]bool, error) {
	num, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	if num == 0 {
		if err := sendCode(c, RejectCodeInvalid); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("to count must be >= 1")
	}
	seen := make(map[string]bool)
	for num > 0 {
		addr, err := readAddress(r)
		if err != nil {
			return nil, err
		}
		key := strings.ToLower(addr.ToString())
		if seen[key] {
			return nil, fmt.Errorf("duplicate recipient address: %s", addr.ToString())
		}
		seen[key] = true
		h.To = append(h.To, *addr)
		num--
	}
	return seen, nil
}

func readAddToRecipients(c net.Conn, r *bufio.Reader, h *FMsgHeader, seen map[string]bool) error {
	if h.Flags&FlagHasAddTo == 0 {
		return nil
	}
	if h.Flags&FlagHasPid == 0 {
		if err := sendCode(c, RejectCodeInvalid); err != nil {
			return err
		}
		return fmt.Errorf("add to exists but pid does not")
	}

	addToFrom, err := readAddress(r)
	if err != nil {
		if err2 := sendCode(c, RejectCodeInvalid); err2 != nil {
			return err2
		}
		return fmt.Errorf("reading add-to-from address: %w", err)
	}

	addToFromKey := strings.ToLower(addToFrom.ToString())
	fromKey := strings.ToLower(h.From.ToString())
	inFromOrTo := fromKey == addToFromKey
	if !inFromOrTo {
		for _, toAddr := range h.To {
			if strings.ToLower(toAddr.ToString()) == addToFromKey {
				inFromOrTo = true
				break
			}
		}
	}
	if !inFromOrTo {
		if err := sendCode(c, RejectCodeInvalid); err != nil {
			return err
		}
		return fmt.Errorf("add-to-from (%s) not in from or to", addToFrom.ToString())
	}
	h.AddToFrom = addToFrom

	addToCount, err := r.ReadByte()
	if err != nil {
		return err
	}
	if addToCount == 0 {
		if err := sendCode(c, RejectCodeInvalid); err != nil {
			return err
		}
		return fmt.Errorf("add to flag set but count is 0")
	}

	addToSeen := make(map[string]bool)
	for addToCount > 0 {
		addr, err := readAddress(r)
		if err != nil {
			return err
		}
		key := strings.ToLower(addr.ToString())
		if addToSeen[key] {
			if err := sendCode(c, RejectCodeInvalid); err != nil {
				return err
			}
			return fmt.Errorf("duplicate recipient address in add to: %s", addr.ToString())
		}
		addToSeen[key] = true
		if seen[key] {
			if err := sendCode(c, RejectCodeInvalid); err != nil {
				return err
			}
			return fmt.Errorf("add-to address already in to: %s", addr.ToString())
		}
		h.AddTo = append(h.AddTo, *addr)
		addToCount--
	}

	return nil
}

func readAndValidateTimestamp(c net.Conn, r *bufio.Reader, h *FMsgHeader) error {
	if err := binary.Read(r, binary.LittleEndian, &h.Timestamp); err != nil {
		return err
	}
	now := timeutil.TimestampNow().Float64()
	delta := now - h.Timestamp
	if PastTimeDelta > 0 && delta > PastTimeDelta {
		if err := sendCode(c, RejectCodePastTime); err != nil {
			return err
		}
		return fmt.Errorf("message timestamp: %f too far in past, delta: %fs", h.Timestamp, delta)
	}
	if FutureTimeDelta > 0 && delta < 0 && math.Abs(delta) > FutureTimeDelta {
		if err := sendCode(c, RejectCodeFutureTime); err != nil {
			return err
		}
		return fmt.Errorf("message timestamp: %f too far in future, delta: %fs", h.Timestamp, delta)
	}
	return nil
}

func readMessageType(c net.Conn, r *bufio.Reader, h *FMsgHeader) error {
	if h.Flags&FlagCommonType != 0 {
		typeID, err := r.ReadByte()
		if err != nil {
			return err
		}
		mtype, ok := getCommonMediaType(typeID)
		if !ok {
			if err := sendCode(c, RejectCodeInvalid); err != nil {
				return err
			}
			return fmt.Errorf("unmapped common type ID: %d", typeID)
		}
		h.TypeID = typeID
		h.Type = mtype
		return nil
	}

	mime, err := ReadUInt8Slice(r)
	if err != nil {
		return err
	}
	if !isASCIIBytes(mime) {
		if err := sendCode(c, RejectCodeInvalid); err != nil {
			return err
		}
		return fmt.Errorf("message media type must be US-ASCII")
	}
	h.Type = string(mime)
	return nil
}

func readAttachmentType(c net.Conn, r *bufio.Reader, flags uint8) (string, uint8, error) {
	if flags&(1<<0) != 0 {
		typeID, err := r.ReadByte()
		if err != nil {
			return "", 0, err
		}
		mtype, ok := getCommonMediaType(typeID)
		if !ok {
			if err := sendCode(c, RejectCodeInvalid); err != nil {
				return "", 0, err
			}
			return "", 0, fmt.Errorf("unmapped attachment common type ID: %d", typeID)
		}
		return mtype, typeID, nil
	}

	typeBytes, err := ReadUInt8Slice(r)
	if err != nil {
		return "", 0, err
	}
	if !isASCIIBytes(typeBytes) {
		if err := sendCode(c, RejectCodeInvalid); err != nil {
			return "", 0, err
		}
		return "", 0, fmt.Errorf("attachment media type must be US-ASCII")
	}
	return string(typeBytes), 0, nil
}

func readAttachmentHeaders(c net.Conn, r *bufio.Reader, h *FMsgHeader) error {
	var attachCount uint8
	if err := binary.Read(r, binary.LittleEndian, &attachCount); err != nil {
		return err
	}

	totalSize := h.Size
	filenameSeen := make(map[string]bool)
	for i := uint8(0); i < attachCount; i++ {
		attFlags, err := r.ReadByte()
		if err != nil {
			return err
		}
		if err := validateAttachmentFlags(c, attFlags); err != nil {
			return err
		}

		attType, attTypeID, err := readAttachmentType(c, r, attFlags)
		if err != nil {
			return err
		}

		filenameBytes, err := ReadUInt8Slice(r)
		if err != nil {
			return err
		}
		filename := string(filenameBytes)
		if !isValidAttachmentFilename(filename) {
			if err := sendCode(c, RejectCodeInvalid); err != nil {
				return err
			}
			return fmt.Errorf("invalid attachment filename: %s", filename)
		}
		filenameKey := strings.ToLower(filename)
		if filenameSeen[filenameKey] {
			if err := sendCode(c, RejectCodeInvalid); err != nil {
				return err
			}
			return fmt.Errorf("duplicate attachment filename: %s", filename)
		}
		filenameSeen[filenameKey] = true

		var attSize uint32
		if err := binary.Read(r, binary.LittleEndian, &attSize); err != nil {
			return err
		}

		h.Attachments = append(h.Attachments, FMsgAttachmentHeader{
			Flags:    attFlags,
			TypeID:   attTypeID,
			Type:     attType,
			Filename: filename,
			Size:     attSize,
		})
		totalSize += attSize
	}

	if totalSize > MaxMessageSize {
		if err := sendCode(c, RejectCodeTooBig); err != nil {
			return err
		}
		return fmt.Errorf("total message size %d exceeds max %d", totalSize, MaxMessageSize)
	}

	return nil
}

func readHeader(c net.Conn) (*FMsgHeader, *bufio.Reader, error) {
	r := bufio.NewReaderSize(c, ReadBufferSize)
	var h = &FMsgHeader{InitialResponseCode: AcceptCodeContinue}

	d := calcNetIODuration(66000, MinDownloadRate) // max possible header size
	c.SetReadDeadline(time.Now().Add(d))

	handled, err := readVersionOrChallenge(c, r, h)
	if err != nil {
		if handled {
			return nil, r, err
		}
		return h, r, err
	}
	if handled {
		return nil, r, nil
	}

	// read flags
	flags, err := r.ReadByte()
	if err != nil {
		return h, r, err
	}
	h.Flags = flags
	if err := validateMessageFlags(c, flags); err != nil {
		return h, r, err
	}

	// read pid if any
	if flags&FlagHasPid == 1 {
		pid, err := io.ReadAll(io.LimitReader(r, 32))
		if err != nil {
			return h, r, err
		}
		h.Pid = make([]byte, 32)
		copy(h.Pid, pid)
	}

	// read from address
	from, err := readAddress(r)
	if err != nil {
		return h, r, err
	}

	h.From = *from

	seen, err := readToRecipients(c, r, h)
	if err != nil {
		return h, r, err
	}

	if err := readAddToRecipients(c, r, h, seen); err != nil {
		return h, r, err
	}

	if err := readAndValidateTimestamp(c, r, h); err != nil {
		return h, r, err
	}

	// read topic — only present when pid is NOT set (first message in a thread)
	if flags&FlagHasPid == 0 {
		topic, err := ReadUInt8Slice(r)
		if err != nil {
			return h, r, err
		}
		h.Topic = string(topic)
	}

	if err := readMessageType(c, r, h); err != nil {
		return h, r, err
	}

	// read message size
	if err := binary.Read(r, binary.LittleEndian, &h.Size); err != nil {
		return h, r, err
	}
	// Size check is deferred until attachment headers are parsed (see below)

	if err := readAttachmentHeaders(c, r, h); err != nil {
		return h, r, err
	}

	log.Printf("INFO: <-- MSG\n%s", h)

	if !hasDomainRecipient(h.To, Domain) && !hasDomainRecipient(h.AddTo, Domain) {
		if err := sendCode(c, RejectCodeInvalid); err != nil {
			return h, r, err
		}
		return h, r, fmt.Errorf("no recipients for domain %s", Domain)
	}

	if err := verifySenderIP(c, determineSenderDomain(h)); err != nil {
		return nil, r, err
	}

	h, err = handleAddToPath(c, h)
	if err != nil {
		return h, r, err
	}
	if h == nil {
		return nil, r, nil
	}

	if err := validatePidReplyPath(c, h); err != nil {
		return h, r, err
	}

	return h, r, nil
}

// Sends CHALLENGE request to sender, receiving and storing the challenge hash.
// DNS verification of the remote IP is performed during header exchange (readHeader).
// TODO [Spec step 2]: The spec defines challenge modes (NEVER, ALWAYS,
// HAS_NOT_PARTICIPATED, DIFFERENT_DOMAIN) as implementation choices.
// Currently defaults to ALWAYS. Implement configurable challenge mode.
func challenge(conn net.Conn, h *FMsgHeader, senderDomain string) error {

	// Connection 2 MUST target the same IP as Connection 1 (spec 2.1).
	remoteHost, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return fmt.Errorf("failed to parse remote address for challenge: %w", err)
	}
	conn2, err := tls.Dial("tcp", net.JoinHostPort(remoteHost, fmt.Sprintf("%d", RemotePort)), buildClientTLSConfig("fmsg."+senderDomain))
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
	if len(resp) != 32 {
		return fmt.Errorf("challenge response size %d, expected 32", len(resp))
	}
	copy(h.ChallengeHash[:], resp)
	h.ChallengeCompleted = true
	log.Printf("INFO: <-- CHALLENGE RESP\t%s\n", hex.EncodeToString(resp))

	// gracefully close 2nd connection
	if err := conn2.Close(); err != nil {
		return err
	}

	return nil
}

func validateMsgRecvForAddr(h *FMsgHeader, addr *FMsgAddress, msgHash []byte) (code uint8, err error) {
	duplicate, err := hasAddrReceivedMsgHash(msgHash, addr)
	if err != nil {
		return RejectCodeUserUndisclosed, err
	}
	if duplicate {
		return RejectCodeUserDuplicate, nil
	}

	detail, err := getAddressDetail(addr)
	if err != nil {
		return RejectCodeUserUndisclosed, err
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

func localRecipients(h *FMsgHeader) []FMsgAddress {
	addrs := make([]FMsgAddress, 0, len(h.To)+len(h.AddTo))
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
	return addrs
}

func allLocalRecipientsHaveMessageHash(msgHash []byte, addrs []FMsgAddress) (bool, error) {
	if len(addrs) == 0 {
		return false, nil
	}
	for i := range addrs {
		duplicate, err := hasAddrReceivedMsgHash(msgHash, &addrs[i])
		if err != nil {
			return false, err
		}
		if !duplicate {
			return false, nil
		}
	}
	return true, nil
}

func markAllCodes(codes []byte, code uint8) {
	for i := range codes {
		codes[i] = code
	}
}

func prepareMessageData(r io.Reader, h *FMsgHeader, skipData bool) ([]string, error) {
	if skipData {
		parentID, err := lookupMsgIdByHash(h.Pid)
		if err != nil {
			return nil, err
		}
		if parentID == 0 {
			return nil, fmt.Errorf("%w code 65 requires stored parent for pid %s", ErrProtocolViolation, hex.EncodeToString(h.Pid))
		}
		parentMsg, err := getMsgByID(parentID)
		if err != nil {
			return nil, err
		}
		if parentMsg == nil || parentMsg.Filepath == "" {
			return nil, fmt.Errorf("%w code 65 parent data unavailable for msg %d", ErrProtocolViolation, parentID)
		}
		h.Filepath = parentMsg.Filepath
		return nil, nil
	}

	createdPaths := make([]string, 0, 1+len(h.Attachments))

	fd, err := os.CreateTemp("", "fmsg-download-*")
	if err != nil {
		return nil, err
	}

	if _, err := io.CopyN(fd, r, int64(h.Size)); err != nil {
		fd.Close()
		_ = os.Remove(fd.Name())
		return nil, err
	}
	if err := fd.Close(); err != nil {
		_ = os.Remove(fd.Name())
		return nil, err
	}

	h.Filepath = fd.Name()
	createdPaths = append(createdPaths, fd.Name())

	for i := range h.Attachments {
		afd, err := os.CreateTemp("", "fmsg-attachment-*")
		if err != nil {
			for _, path := range createdPaths {
				_ = os.Remove(path)
			}
			return nil, err
		}

		if _, err := io.CopyN(afd, r, int64(h.Attachments[i].Size)); err != nil {
			afd.Close()
			_ = os.Remove(afd.Name())
			for _, path := range createdPaths {
				_ = os.Remove(path)
			}
			return nil, err
		}
		if err := afd.Close(); err != nil {
			_ = os.Remove(afd.Name())
			for _, path := range createdPaths {
				_ = os.Remove(path)
			}
			return nil, err
		}
		h.Attachments[i].Filepath = afd.Name()
		createdPaths = append(createdPaths, afd.Name())
	}

	return createdPaths, nil
}

func cleanupFiles(paths []string) {
	for _, path := range paths {
		if path == "" {
			continue
		}
		_ = os.Remove(path)
	}
}

func copyMessagePayload(src *os.File, dstPath string, compressed bool, wireSize uint32) error {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return err
	}

	fd2, err := os.Create(dstPath)
	if err != nil {
		return err
	}

	var copyErr error
	if compressed {
		lr := io.LimitReader(src, int64(wireSize))
		zr, err := zlib.NewReader(lr)
		if err != nil {
			fd2.Close()
			_ = os.Remove(dstPath)
			return err
		}
		_, copyErr = io.Copy(fd2, zr)
		_ = zr.Close()
	} else {
		_, copyErr = io.CopyN(fd2, src, int64(wireSize))
	}
	if err := fd2.Close(); err != nil {
		return err
	}

	if copyErr != nil {
		_ = os.Remove(dstPath)
		return copyErr
	}
	return nil
}

func uniqueAttachmentPath(dir string, timestamp uint32, idx int, filename string) string {
	ext := filepath.Ext(filename)
	base := fmt.Sprintf("%d_att_%d", timestamp, idx)
	p := filepath.Join(dir, base+ext)
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	for n := 1; ; n++ {
		p = filepath.Join(dir, fmt.Sprintf("%s_%d%s", base, n, ext))
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return p
		}
	}
}

func persistAttachmentPayloads(h *FMsgHeader, dirpath string) error {
	for i := range h.Attachments {
		a := &h.Attachments[i]
		src, err := os.Open(a.Filepath)
		if err != nil {
			return err
		}
		dstPath := uniqueAttachmentPath(dirpath, uint32(h.Timestamp), i, a.Filename)
		compressed := a.Flags&(1<<1) != 0
		err = copyMessagePayload(src, dstPath, compressed, a.Size)
		src.Close()
		if err != nil {
			return err
		}
		a.Filepath = dstPath
	}
	return nil
}

func storeAcceptedMessage(h *FMsgHeader, codes []byte, acceptedTo []FMsgAddress, acceptedAddTo []FMsgAddress, primaryFilepath string) bool {
	if len(acceptedTo) == 0 && len(acceptedAddTo) == 0 {
		return false
	}

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
		return false
	}

	h.To = origTo
	h.AddTo = origAddTo
	allAccepted := append(acceptedTo, acceptedAddTo...)
	for i := range allAccepted {
		if err := postMsgStatRecv(&allAccepted[i], h.Timestamp, int(h.Size)); err != nil {
			log.Printf("WARN: Failed to post msg recv stat: %s", err)
		}
	}
	return true
}

func downloadMessage(c net.Conn, r io.Reader, h *FMsgHeader, skipData bool) error {
	addrs := localRecipients(h)
	if len(addrs) == 0 {
		return fmt.Errorf("%w our domain: %s, not in recipient list", ErrProtocolViolation, Domain)
	}
	codes := make([]byte, len(addrs))

	createdPaths, err := prepareMessageData(r, h, skipData)
	if err != nil {
		return err
	}
	cleanupOnReturn := !skipData
	defer func() {
		if cleanupOnReturn {
			cleanupFiles(createdPaths)
		}
	}()

	// verify hash matches challenge response when challenge was completed
	msgHash, err := h.GetMessageHash()
	if err != nil {
		return err
	}
	if h.ChallengeCompleted && !bytes.Equal(h.ChallengeHash[:], msgHash) {
		challengeHashStr := hex.EncodeToString(h.ChallengeHash[:])
		actualHashStr := hex.EncodeToString(msgHash)
		return fmt.Errorf("%w actual hash: %s mismatch challenge response: %s", ErrProtocolViolation, actualHashStr, challengeHashStr)
	}

	// pid/add-to validation is handled during header exchange in readHeader().

	// determine file extension from MIME type
	exts, _ := mime.ExtensionsByType(h.Type)
	var ext string
	if exts == nil {
		ext = ".unknown"
	} else {
		ext = exts[0]
	}

	src, err := os.Open(h.Filepath)
	if err != nil {
		return err
	}
	defer src.Close()

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
		code, err := validateMsgRecvForAddr(h, &addr, msgHash)
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
		if err := copyMessagePayload(src, fp, h.Flags&FlagDeflate != 0, h.Size); err != nil {
			log.Printf("ERROR: copying downloaded message from: %s, to: %s", h.Filepath, fp)
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
			if err := persistAttachmentPayloads(h, filepath.Dir(primaryFilepath)); err != nil {
				log.Printf("ERROR: copying attachment payloads for message storage: %s", err)
				codes[i] = RejectCodeUndisclosed
				primaryFilepath = ""
				acceptedTo = acceptedTo[:0]
				acceptedAddTo = acceptedAddTo[:0]
				continue
			}
		}
	}

	stored := storeAcceptedMessage(h, codes, acceptedTo, acceptedAddTo, primaryFilepath)
	if stored {
		cleanupOnReturn = false
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

	if err := challenge(c, header, determineSenderDomain(header)); err != nil {
		log.Printf("ERROR: Challenge failed to, %s: %s", c.RemoteAddr().String(), err)
		abortConn(c)
		return
	}

	// §10.4 Step 1: Add-to handling (only when add-to set and parent verified stored)
	skipData := false
	switch header.InitialResponseCode {
	case AcceptCodeAddTo:
		// No local add-to recipients; store header and respond code 11, close.
		if err := storeMsgHeaderOnly(header); err != nil {
			log.Printf("ERROR: storing add-to header: %s", err)
			_ = sendCode(c, RejectCodeUndisclosed)
			abortConn(c)
			return
		}
		if err := sendCode(c, AcceptCodeAddTo); err != nil {
			log.Printf("ERROR: failed sending code 11 to %s: %s", c.RemoteAddr().String(), err)
			abortConn(c)
			return
		}
		log.Printf("INFO: additional recipients received (code 11) for pid %s", hex.EncodeToString(header.Pid))
		c.Close()
		return
	case AcceptCodeSkipData:
		// Local add-to recipients exist; parent stored; skip data.
		if err := sendCode(c, AcceptCodeSkipData); err != nil {
			log.Printf("ERROR: failed sending code 65 to %s: %s", c.RemoteAddr().String(), err)
			abortConn(c)
			return
		}
		skipData = true
		log.Printf("INFO: sent code 65 (skip data) to %s", c.RemoteAddr().String())
	default:
		// §10.4 Step 2: Duplicate check (before sending continue)
		if header.ChallengeCompleted {
			addrs := localRecipients(header)
			allDup, err := allLocalRecipientsHaveMessageHash(header.ChallengeHash[:], addrs)
			if err != nil {
				log.Printf("ERROR: duplicate check failed for %s: %s", c.RemoteAddr().String(), err)
				_ = sendCode(c, RejectCodeUndisclosed)
				abortConn(c)
				return
			}
			if allDup {
				if err := sendCode(c, RejectCodeDuplicate); err != nil {
					log.Printf("ERROR: failed sending code 10 to %s: %s", c.RemoteAddr().String(), err)
				}
				c.Close()
				return
			}
		}

		// §10.4 Step 3: Continue
		if err := sendCode(c, AcceptCodeContinue); err != nil {
			log.Printf("ERROR: failed sending code 64 to %s: %s", c.RemoteAddr().String(), err)
			abortConn(c)
			return
		}
		log.Printf("INFO: sent code 64 (continue) to %s", c.RemoteAddr().String())
	}

	// store message
	deadlineBytes := int(header.Size)
	if skipData {
		deadlineBytes = 1
	}
	c.SetReadDeadline(time.Now().Add(calcNetIODuration(deadlineBytes, MinDownloadRate)))
	if err := downloadMessage(c, r, header, skipData); err != nil {
		// if error was a protocal violation, abort; otherise let sender know there was an internal error
		log.Printf("ERROR: Download failed from, %s: %s", c.RemoteAddr().String(), err)
		if errors.Is(err, ErrProtocolViolation) {
			return
		} else {
			_ = sendCode(c, RejectCodeUndisclosed)
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

	// load TLS configuration (must be after loadEnvConfig for FMSG_TLS_INSECURE_SKIP_VERIFY)
	serverTLSConfig = buildServerTLSConfig()

	// start sender in background (small delay so listener is ready first)
	go func() {
		time.Sleep(1 * time.Second)
		startSender()
	}()

	// start listening
	addr := fmt.Sprintf("%s:%d", listenAddress, Port)
	ln, err := tls.Listen("tcp", addr, serverTLSConfig)
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
