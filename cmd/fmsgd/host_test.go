package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

type testAddr string

func (a testAddr) Network() string { return "tcp" }
func (a testAddr) String() string  { return string(a) }

type testConn struct {
	bytes.Buffer
}

func (c *testConn) Read(b []byte) (int, error)         { return 0, nil }
func (c *testConn) Write(b []byte) (int, error)        { return c.Buffer.Write(b) }
func (c *testConn) Close() error                       { return nil }
func (c *testConn) LocalAddr() net.Addr                { return testAddr("127.0.0.1:1000") }
func (c *testConn) RemoteAddr() net.Addr               { return testAddr("127.0.0.1:2000") }
func (c *testConn) SetDeadline(t time.Time) error      { return nil }
func (c *testConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *testConn) SetWriteDeadline(t time.Time) error { return nil }

func TestIsValidUser(t *testing.T) {
	valid := []string{"alice", "Bob", "a-b", "a_b", "a.b", "user123", "A", "u\u00f1icode", "\u7528\u62371"}
	for _, u := range valid {
		if !isValidUser(u) {
			t.Errorf("isValidUser(%q) = false, want true", u)
		}
	}

	invalid := []string{"", " ", "a b", "a@b", "a/b", string(make([]byte, 65)), "-alice", "alice-", "a..b", "a-_b"}
	for _, u := range invalid {
		if isValidUser(u) {
			t.Errorf("isValidUser(%q) = true, want false", u)
		}
	}
}

func TestIsValidDomain(t *testing.T) {
	valid := []string{"example.com", "a.b.c", "foo-bar.com", "localhost"}
	for _, d := range valid {
		if !isValidDomain(d) {
			t.Errorf("isValidDomain(%q) = false, want true", d)
		}
	}

	invalid := []string{
		"",
		"nodot",        // no dots and not localhost
		".leading.dot", // empty label
		"trailing.",    // empty label
		"-start.com",   // label starts with hyphen
		"end-.com",     // label ends with hyphen
		"has space.com",
	}
	for _, d := range invalid {
		if isValidDomain(d) {
			t.Errorf("isValidDomain(%q) = true, want false", d)
		}
	}
}

func TestParseAddress(t *testing.T) {
	tests := []struct {
		input   string
		wantErr bool
		user    string
		domain  string
	}{
		{"@alice@example.com", false, "alice", "example.com"},
		{"@Bob@EXAMPLE.COM", false, "Bob", "EXAMPLE.COM"},
		{"@a-b.c@x.y.z", false, "a-b.c", "x.y.z"},
		// errors
		{"alice@example.com", true, "", ""}, // missing leading @
		{"@alice", true, "", ""},            // missing second @
		{"@", true, "", ""},                 // too short
		{"ab", true, "", ""},                // too short
		{"@@example.com", true, "", ""},     // empty user
		{"@alice@", true, "", ""},           // empty domain (not valid)
		{"@alice@nodot", true, "", ""},      // domain with no dot (not localhost)
	}
	for _, tt := range tests {
		addr, err := parseAddress([]byte(tt.input))
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseAddress(%q) = nil error, want error", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseAddress(%q) error: %v", tt.input, err)
			continue
		}
		if addr.User != tt.user || addr.Domain != tt.domain {
			t.Errorf("parseAddress(%q) = {%q, %q}, want {%q, %q}", tt.input, addr.User, addr.Domain, tt.user, tt.domain)
		}
	}
}

func TestReadUInt8Slice(t *testing.T) {
	// Build a buffer: uint8 length = 5, then "hello"
	var buf bytes.Buffer
	buf.WriteByte(5)
	buf.WriteString("hello")
	// Extra trailing bytes should not be consumed
	buf.WriteString("extra")

	slice, err := ReadUInt8Slice(&buf)
	if err != nil {
		t.Fatalf("ReadUInt8Slice error: %v", err)
	}
	if string(slice) != "hello" {
		t.Fatalf("ReadUInt8Slice = %q, want %q", string(slice), "hello")
	}
	// "extra" should remain
	rest := make([]byte, 5)
	n, _ := buf.Read(rest)
	if string(rest[:n]) != "extra" {
		t.Fatalf("remaining bytes = %q, want %q", string(rest[:n]), "extra")
	}
}

func TestReadUInt8SliceEmpty(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(0) // zero-length slice

	slice, err := ReadUInt8Slice(&buf)
	if err != nil {
		t.Fatalf("ReadUInt8Slice error: %v", err)
	}
	if len(slice) != 0 {
		t.Fatalf("expected empty slice, got len %d", len(slice))
	}
}

func TestCalcNetIODuration(t *testing.T) {
	// Small sizes should return MinNetIODeadline
	d := calcNetIODuration(100, 5000)
	if d < MinNetIODeadline {
		t.Fatalf("calcNetIODuration(100, 5000) = %v, want >= %v", d, MinNetIODeadline)
	}

	// Large sizes should exceed MinNetIODeadline
	d = calcNetIODuration(1_000_000, 5000)
	expected := time.Duration(float64(1_000_000) / 5000 * float64(time.Second)) // 200s
	if d != expected {
		t.Fatalf("calcNetIODuration(1000000, 5000) = %v, want %v", d, expected)
	}
}

func TestResponseCodeName(t *testing.T) {
	tests := []struct {
		code uint8
		want string
	}{
		{RejectCodeInvalid, "invalid"},
		{RejectCodeUnsupportedVersion, "unsupported version"},
		{RejectCodeUndisclosed, "undisclosed"},
		{RejectCodeTooBig, "too big"},
		{RejectCodeInsufficentResources, "insufficient resources"},
		{RejectCodeParentNotFound, "parent not found"},
		{RejectCodePastTime, "past time"},
		{RejectCodeFutureTime, "future time"},
		{RejectCodeTimeTravel, "time travel"},
		{RejectCodeDuplicate, "duplicate"},
		{AcceptCodeAddTo, "accept add to"},
		{RejectCodeUserUnknown, "user unknown"},
		{RejectCodeUserFull, "user full"},
		{RejectCodeUserNotAccepting, "user not accepting"},
		{RejectCodeUserDuplicate, "user duplicate"},
		{RejectCodeUserUndisclosed, "user undisclosed"},
		{RejectCodeAccept, "accept"},
		{99, "unknown(99)"},
	}
	for _, tt := range tests {
		got := responseCodeName(tt.code)
		if got != tt.want {
			t.Errorf("responseCodeName(%d) = %q, want %q", tt.code, got, tt.want)
		}
	}
}

func TestPerRecipientDuplicateAndUndisclosedCodeValues(t *testing.T) {
	if RejectCodeUserDuplicate != 103 {
		t.Fatalf("RejectCodeUserDuplicate = %d, want 103", RejectCodeUserDuplicate)
	}
	if RejectCodeUserUndisclosed != 105 {
		t.Fatalf("RejectCodeUserUndisclosed = %d, want 105", RejectCodeUserUndisclosed)
	}
}

func TestFlagConstants(t *testing.T) {
	// Verify flag bit assignments match SPEC.md
	if FlagHasPid != 1 {
		t.Errorf("FlagHasPid = %d, want 1 (bit 0)", FlagHasPid)
	}
	if FlagHasAddTo != 2 {
		t.Errorf("FlagHasAddTo = %d, want 2 (bit 1)", FlagHasAddTo)
	}
	if FlagCommonType != 4 {
		t.Errorf("FlagCommonType = %d, want 4 (bit 2)", FlagCommonType)
	}
	if FlagImportant != 8 {
		t.Errorf("FlagImportant = %d, want 8 (bit 3)", FlagImportant)
	}
	if FlagNoReply != 16 {
		t.Errorf("FlagNoReply = %d, want 16 (bit 4)", FlagNoReply)
	}
	if FlagDeflate != 32 {
		t.Errorf("FlagDeflate = %d, want 32 (bit 5)", FlagDeflate)
	}
}

func encodeUInt8String(t *testing.T, s string) []byte {
	t.Helper()
	if len(s) > 255 {
		t.Fatalf("string too long for uint8 prefix: %d", len(s))
	}
	b := []byte{byte(len(s))}
	b = append(b, []byte(s)...)
	return b
}

func TestHasDomainRecipient(t *testing.T) {
	addrs := []FMsgAddress{
		{User: "alice", Domain: "example.com"},
		{User: "bob", Domain: "other.org"},
	}
	if !hasDomainRecipient(addrs, "EXAMPLE.COM") {
		t.Fatalf("expected domain match")
	}
	if hasDomainRecipient(addrs, "missing.test") {
		t.Fatalf("did not expect domain match")
	}
}

func TestDetermineSenderDomain(t *testing.T) {
	h := &FMsgHeader{
		From: FMsgAddress{User: "alice", Domain: "from.example"},
	}
	if got := determineSenderDomain(h); got != "from.example" {
		t.Fatalf("determineSenderDomain() = %q, want %q", got, "from.example")
	}

	h.AddTo = []FMsgAddress{{User: "new", Domain: "to.example"}}
	h.AddToFrom = &FMsgAddress{User: "bob", Domain: "sender.example"}
	if got := determineSenderDomain(h); got != "sender.example" {
		t.Fatalf("determineSenderDomain() = %q, want %q", got, "sender.example")
	}
}

func TestReadToRecipients(t *testing.T) {
	b := []byte{2}
	b = append(b, encodeUInt8String(t, "@alice@example.com")...)
	b = append(b, encodeUInt8String(t, "@bob@example.com")...)

	h := &FMsgHeader{}
	seen, err := readToRecipients(nil, bufio.NewReader(bytes.NewReader(b)), h)
	if err != nil {
		t.Fatalf("readToRecipients returned error: %v", err)
	}
	if len(h.To) != 2 {
		t.Fatalf("len(h.To) = %d, want 2", len(h.To))
	}
	if !seen["@alice@example.com"] || !seen["@bob@example.com"] {
		t.Fatalf("seen map missing expected recipients: %#v", seen)
	}
}

func TestReadAddToRecipients(t *testing.T) {
	h := &FMsgHeader{
		Flags: FlagHasPid | FlagHasAddTo,
		From:  FMsgAddress{User: "alice", Domain: "example.com"},
		To:    []FMsgAddress{{User: "bob", Domain: "example.com"}},
	}
	seen := map[string]bool{"@bob@example.com": true}

	b := []byte{}
	b = append(b, encodeUInt8String(t, "@alice@example.com")...) // add-to-from
	b = append(b, 1)                                             // add-to count
	b = append(b, encodeUInt8String(t, "@carol@example.com")...)

	err := readAddToRecipients(nil, bufio.NewReader(bytes.NewReader(b)), h, seen)
	if err != nil {
		t.Fatalf("readAddToRecipients returned error: %v", err)
	}
	if h.AddToFrom == nil || h.AddToFrom.ToString() != "@alice@example.com" {
		t.Fatalf("unexpected AddToFrom: %+v", h.AddToFrom)
	}
	if len(h.AddTo) != 1 || h.AddTo[0].ToString() != "@carol@example.com" {
		t.Fatalf("unexpected AddTo: %+v", h.AddTo)
	}
}

func TestReadMessageType(t *testing.T) {
	hCommon := &FMsgHeader{Flags: FlagCommonType}
	if err := readMessageType(nil, bufio.NewReader(bytes.NewReader([]byte{3})), hCommon); err != nil {
		t.Fatalf("readMessageType(common) error: %v", err)
	}
	if hCommon.TypeID != 3 {
		t.Fatalf("common type ID = %d, want 3", hCommon.TypeID)
	}
	if hCommon.Type != "application/json" {
		t.Fatalf("common type = %q, want %q", hCommon.Type, "application/json")
	}

	hText := &FMsgHeader{Flags: 0}
	b := encodeUInt8String(t, "text/plain")
	if err := readMessageType(nil, bufio.NewReader(bytes.NewReader(b)), hText); err != nil {
		t.Fatalf("readMessageType(string) error: %v", err)
	}
	if hText.Type != "text/plain" {
		t.Fatalf("string type = %q, want %q", hText.Type, "text/plain")
	}
}

func TestReadAttachmentHeaders(t *testing.T) {
	origMax := MaxMessageSize
	MaxMessageSize = 1024
	t.Cleanup(func() {
		MaxMessageSize = origMax
	})

	h := &FMsgHeader{Size: 10}
	b := []byte{1}   // attachment count
	b = append(b, 0) // attachment flags (no common type)
	b = append(b, encodeUInt8String(t, "text/plain")...)
	b = append(b, encodeUInt8String(t, "file.txt")...)

	var sz [4]byte
	binary.LittleEndian.PutUint32(sz[:], 12)
	b = append(b, sz[:]...)

	err := readAttachmentHeaders(nil, bufio.NewReader(bytes.NewReader(b)), h)
	if err != nil {
		t.Fatalf("readAttachmentHeaders returned error: %v", err)
	}
	if len(h.Attachments) != 1 {
		t.Fatalf("len(h.Attachments) = %d, want 1", len(h.Attachments))
	}
	att := h.Attachments[0]
	if att.TypeID != 0 {
		t.Fatalf("attachment type ID = %d, want 0 for non-common", att.TypeID)
	}
	if att.Type != "text/plain" || att.Filename != "file.txt" || att.Size != 12 {
		t.Fatalf("unexpected attachment parsed: %+v", att)
	}
}

func TestReadAddToRecipientsRejectsWhenPidMissing(t *testing.T) {
	h := &FMsgHeader{Flags: FlagHasAddTo}
	c := &testConn{}

	err := readAddToRecipients(c, bufio.NewReader(bytes.NewReader(nil)), h, map[string]bool{})
	if err == nil {
		t.Fatalf("expected error when add-to flag is set without pid")
	}
	if got := c.Bytes(); len(got) != 1 || got[0] != RejectCodeInvalid {
		t.Fatalf("expected reject code %d, got %v", RejectCodeInvalid, got)
	}
}

func TestReadAddToRecipientsRejectsDuplicateAddTo(t *testing.T) {
	h := &FMsgHeader{
		Flags: FlagHasPid | FlagHasAddTo,
		From:  FMsgAddress{User: "alice", Domain: "example.com"},
		To:    []FMsgAddress{{User: "bob", Domain: "example.com"}},
	}
	c := &testConn{}
	seen := map[string]bool{"@bob@example.com": true}

	b := []byte{}
	b = append(b, encodeUInt8String(t, "@alice@example.com")...) // add-to-from
	b = append(b, 2)                                             // add-to count
	b = append(b, encodeUInt8String(t, "@carol@example.com")...)
	b = append(b, encodeUInt8String(t, "@carol@example.com")...)

	err := readAddToRecipients(c, bufio.NewReader(bytes.NewReader(b)), h, seen)
	if err == nil {
		t.Fatalf("expected duplicate add-to error")
	}
	if got := c.Bytes(); len(got) != 1 || got[0] != RejectCodeInvalid {
		t.Fatalf("expected reject code %d, got %v", RejectCodeInvalid, got)
	}
}

func TestReadMessageTypeRejectsUnknownCommonType(t *testing.T) {
	h := &FMsgHeader{Flags: FlagCommonType}
	c := &testConn{}

	err := readMessageType(c, bufio.NewReader(bytes.NewReader([]byte{200})), h)
	if err == nil {
		t.Fatalf("expected error for unknown common type")
	}
	if got := c.Bytes(); len(got) != 1 || got[0] != RejectCodeInvalid {
		t.Fatalf("expected reject code %d, got %v", RejectCodeInvalid, got)
	}
}

func TestReadMessageTypeRejectsNonASCIIStringType(t *testing.T) {
	h := &FMsgHeader{Flags: 0}
	c := &testConn{}

	b := encodeUInt8String(t, "text/\u03c0lain")
	err := readMessageType(c, bufio.NewReader(bytes.NewReader(b)), h)
	if err == nil {
		t.Fatalf("expected error for non-ASCII message type")
	}
	if got := c.Bytes(); len(got) != 1 || got[0] != RejectCodeInvalid {
		t.Fatalf("expected reject code %d, got %v", RejectCodeInvalid, got)
	}
}

func TestReadAttachmentTypeRejectsNonASCIIStringType(t *testing.T) {
	c := &testConn{}
	b := encodeUInt8String(t, "text/\u03c0lain")

	_, _, err := readAttachmentType(c, bufio.NewReader(bytes.NewReader(b)), 0)
	if err == nil {
		t.Fatalf("expected error for non-ASCII attachment type")
	}
	if got := c.Bytes(); len(got) != 1 || got[0] != RejectCodeInvalid {
		t.Fatalf("expected reject code %d, got %v", RejectCodeInvalid, got)
	}
}

func TestReadAttachmentHeadersRejectsInvalidFilename(t *testing.T) {
	origMax := MaxMessageSize
	MaxMessageSize = 1024
	t.Cleanup(func() {
		MaxMessageSize = origMax
	})

	h := &FMsgHeader{Size: 10}
	c := &testConn{}
	b := []byte{1}
	b = append(b, 0)
	b = append(b, encodeUInt8String(t, "text/plain")...)
	b = append(b, encodeUInt8String(t, "bad..name")...) // invalid: consecutive special chars

	var sz [4]byte
	binary.LittleEndian.PutUint32(sz[:], 12)
	b = append(b, sz[:]...)

	err := readAttachmentHeaders(c, bufio.NewReader(bytes.NewReader(b)), h)
	if err == nil {
		t.Fatalf("expected error for invalid attachment filename")
	}
	if got := c.Bytes(); len(got) != 1 || got[0] != RejectCodeInvalid {
		t.Fatalf("expected reject code %d, got %v", RejectCodeInvalid, got)
	}
}

func TestReadAttachmentHeadersRejectsTooBig(t *testing.T) {
	origMax := MaxMessageSize
	MaxMessageSize = 20
	t.Cleanup(func() {
		MaxMessageSize = origMax
	})

	h := &FMsgHeader{Size: 15}
	c := &testConn{}
	b := []byte{1}
	b = append(b, 0)
	b = append(b, encodeUInt8String(t, "text/plain")...)
	b = append(b, encodeUInt8String(t, "file.txt")...)

	var sz [4]byte
	binary.LittleEndian.PutUint32(sz[:], 10)
	b = append(b, sz[:]...)

	err := readAttachmentHeaders(c, bufio.NewReader(bytes.NewReader(b)), h)
	if err == nil {
		t.Fatalf("expected size overflow error")
	}
	if got := c.Bytes(); len(got) != 1 || got[0] != RejectCodeTooBig {
		t.Fatalf("expected reject code %d, got %v", RejectCodeTooBig, got)
	}
}

func TestValidateMessageFlagsRejectsReservedBits(t *testing.T) {
	c := &testConn{}
	err := validateMessageFlags(c, 1<<6)
	if err == nil {
		t.Fatalf("expected error for reserved message flag bit")
	}
	if got := c.Bytes(); len(got) != 1 || got[0] != RejectCodeInvalid {
		t.Fatalf("expected reject code %d, got %v", RejectCodeInvalid, got)
	}
}

func TestReadAttachmentHeadersRejectsReservedAttachmentBits(t *testing.T) {
	h := &FMsgHeader{Size: 0}
	c := &testConn{}

	// attachment count=1, then attachment flags with reserved bit 2 set
	b := []byte{1, 1 << 2}
	err := readAttachmentHeaders(c, bufio.NewReader(bytes.NewReader(b)), h)
	if err == nil {
		t.Fatalf("expected error for reserved attachment flag bits")
	}
	if got := c.Bytes(); len(got) != 1 || got[0] != RejectCodeInvalid {
		t.Fatalf("expected reject code %d, got %v", RejectCodeInvalid, got)
	}
}

func TestResolvePostChallengeCode(t *testing.T) {
	tests := []struct {
		name               string
		initialCode        uint8
		challengeCompleted bool
		allLocalDup        bool
		want               uint8
	}{
		// Add-to (code 11) path — never overridden by dup check.
		{"add-to no challenge", AcceptCodeAddTo, false, false, AcceptCodeAddTo},
		{"add-to challenge no dup", AcceptCodeAddTo, true, false, AcceptCodeAddTo},
		{"add-to challenge all dup", AcceptCodeAddTo, true, true, AcceptCodeAddTo},

		// Continue (code 64) path — dup check yields code 10 when all dup.
		{"continue no challenge", AcceptCodeContinue, false, false, AcceptCodeContinue},
		{"continue challenge no dup", AcceptCodeContinue, true, false, AcceptCodeContinue},
		{"continue challenge all dup", AcceptCodeContinue, true, true, RejectCodeDuplicate},

		// Skip-data (code 65) path — dup check yields code 10 when all dup.
		{"skip-data no challenge", AcceptCodeSkipData, false, false, AcceptCodeSkipData},
		{"skip-data challenge no dup", AcceptCodeSkipData, true, false, AcceptCodeSkipData},
		{"skip-data challenge all dup", AcceptCodeSkipData, true, true, RejectCodeDuplicate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolvePostChallengeCode(tt.initialCode, tt.challengeCompleted, tt.allLocalDup)
			if got != tt.want {
				t.Errorf("resolvePostChallengeCode(%d, %v, %v) = %d (%s), want %d (%s)",
					tt.initialCode, tt.challengeCompleted, tt.allLocalDup,
					got, responseCodeName(got), tt.want, responseCodeName(tt.want))
			}
		})
	}
}

func TestReadAttachmentHeadersReadsExpandedSizeForCompressedAttachment(t *testing.T) {
	origMax := MaxMessageSize
	origExpanded := MaxExpandedSize
	MaxMessageSize = 1024
	MaxExpandedSize = 1024
	t.Cleanup(func() {
		MaxMessageSize = origMax
		MaxExpandedSize = origExpanded
	})

	h := &FMsgHeader{Size: 0}
	b := []byte{1}                      // 1 attachment
	b = append(b, 1<<1)                 // attachment flags: zlib-deflate (bit 1)
	b = append(b, encodeUInt8String(t, "text/plain")...)
	b = append(b, encodeUInt8String(t, "file.txt")...)

	var wireSize [4]byte
	binary.LittleEndian.PutUint32(wireSize[:], 50)
	b = append(b, wireSize[:]...)

	var expandedSize [4]byte
	binary.LittleEndian.PutUint32(expandedSize[:], 200)
	b = append(b, expandedSize[:]...)

	err := readAttachmentHeaders(nil, bufio.NewReader(bytes.NewReader(b)), h)
	if err != nil {
		t.Fatalf("readAttachmentHeaders returned error: %v", err)
	}
	if len(h.Attachments) != 1 {
		t.Fatalf("len(h.Attachments) = %d, want 1", len(h.Attachments))
	}
	att := h.Attachments[0]
	if att.Size != 50 {
		t.Fatalf("att.Size = %d, want 50", att.Size)
	}
	if att.ExpandedSize != 200 {
		t.Fatalf("att.ExpandedSize = %d, want 200", att.ExpandedSize)
	}
}

func TestReadAttachmentHeadersRejectsExpandedSizeExceedsMax(t *testing.T) {
	origMax := MaxMessageSize
	origExpanded := MaxExpandedSize
	MaxMessageSize = 1024
	MaxExpandedSize = 100
	t.Cleanup(func() {
		MaxMessageSize = origMax
		MaxExpandedSize = origExpanded
	})

	h := &FMsgHeader{Size: 0}
	c := &testConn{}
	b := []byte{1}      // 1 attachment
	b = append(b, 1<<1) // attachment flags: zlib-deflate (bit 1)
	b = append(b, encodeUInt8String(t, "text/plain")...)
	b = append(b, encodeUInt8String(t, "file.txt")...)

	var wireSize [4]byte
	binary.LittleEndian.PutUint32(wireSize[:], 50)
	b = append(b, wireSize[:]...)

	// expanded size exceeds MaxExpandedSize=100
	var expandedSize [4]byte
	binary.LittleEndian.PutUint32(expandedSize[:], 200)
	b = append(b, expandedSize[:]...)

	err := readAttachmentHeaders(c, bufio.NewReader(bytes.NewReader(b)), h)
	if err == nil {
		t.Fatalf("expected error when expanded size exceeds max")
	}
	if got := c.Bytes(); len(got) != 1 || got[0] != RejectCodeTooBig {
		t.Fatalf("expected reject code %d, got %v", RejectCodeTooBig, got)
	}
}
