package main

import (
	"bytes"
	"testing"
	"time"
)

func TestIsValidUser(t *testing.T) {
	valid := []string{"alice", "Bob", "a-b", "a_b", "a.b", "user123", "A"}
	for _, u := range valid {
		if !isValidUser(u) {
			t.Errorf("isValidUser(%q) = false, want true", u)
		}
	}

	invalid := []string{"", " ", "a b", "a@b", "a/b", string(make([]byte, 65))}
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
		{AcceptCodeHeader, "accept header"},
		{RejectCodeUserUnknown, "user unknown"},
		{RejectCodeUserFull, "user full"},
		{RejectCodeUserNotAccepting, "user not accepting"},
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
