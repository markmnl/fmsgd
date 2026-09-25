package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// serveOnce accepts one TLS connection, runs handleConn on it after the
// client function returns, and returns everything handleConn logged.
func serveOnce(t *testing.T, client func(addr string)) string {
	t.Helper()
	cert, _ := senderTestCertificate(t, time.Now().Add(time.Hour))
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	logs := &syncBuffer{}
	log.SetOutput(logs)
	defer log.SetOutput(os.Stderr)

	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		handleConn(c)
	}()

	client(ln.Addr().String())

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("handleConn did not return")
	}
	return logs.String()
}

func tlsClient(t *testing.T, addr string) *tls.Conn {
	t.Helper()
	c, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, ServerName: "fmsg.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestHandleConnTCPProbeDoesNotWarn(t *testing.T) {
	logs := serveOnce(t, func(addr string) {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	})
	if strings.Contains(logs, "WARN") {
		t.Fatalf("TCP connect-and-close should not warn, got:\n%s", logs)
	}
	if !strings.Contains(logs, "INFO: 127.0.0.1:") || !strings.Contains(logs, "closed the connection without sending data") {
		t.Fatalf("TCP connect-and-close should be logged at INFO with the peer address, got:\n%s", logs)
	}
}

func TestHandleConnTLSProbeDoesNotWarn(t *testing.T) {
	logs := serveOnce(t, func(addr string) {
		c := tlsClient(t, addr)
		if err := c.Handshake(); err != nil {
			t.Fatal(err)
		}
		c.Close()
	})
	if strings.Contains(logs, "WARN") {
		t.Fatalf("TLS handshake-and-close should not warn, got:\n%s", logs)
	}
	if !strings.Contains(logs, "INFO: 127.0.0.1:") || !strings.Contains(logs, "closed the connection without sending data") {
		t.Fatalf("TLS handshake-and-close should be logged at INFO with the peer address, got:\n%s", logs)
	}
}

func TestHandleConnPartialHeaderWarns(t *testing.T) {
	logs := serveOnce(t, func(addr string) {
		c := tlsClient(t, addr)
		if _, err := c.Write([]byte{1}); err != nil { // version byte, then nothing
			t.Fatal(err)
		}
		c.Close()
	})
	if !strings.Contains(logs, "WARN: reading header from") {
		t.Fatalf("a partial header should warn, got:\n%s", logs)
	}
}

func TestClosedWithoutData(t *testing.T) {
	reset := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}
	cases := []struct {
		name      string
		bytesRead int64
		err       error
		want      bool
	}{
		{"eof before data", 0, io.EOF, true},
		{"reset before data", 0, reset, true},
		{"eof after data", 1, io.EOF, false},
		{"unexpected eof after data", 3, io.ErrUnexpectedEOF, false},
		{"timeout before data", 0, os.ErrDeadlineExceeded, false},
		{"other error", 0, errors.New("tls: first record does not look like a TLS handshake"), false},
	}
	for _, tc := range cases {
		c := &responseTrackingConn{bytesRead: tc.bytesRead}
		if got := closedWithoutData(c, tc.err); got != tc.want {
			t.Errorf("%s: closedWithoutData = %v, want %v", tc.name, got, tc.want)
		}
	}
}
