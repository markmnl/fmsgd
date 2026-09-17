package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/big"
	"net"
	"testing"
	"time"
)

func senderTestCertificate(t *testing.T, notAfter time.Time) (tls.Certificate, *tls.Config) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		DNSNames:     []string{"fmsg.example.com"},
		NotBefore:    time.Now().Add(-2 * time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	config := buildClientTLSConfig("fmsg.example.com")
	if config.InsecureSkipVerify {
		t.Fatal("sender must verify the server certificate")
	}
	config.RootCAs = x509.NewCertPool()
	config.RootCAs.AddCert(leaf)
	config.ClientSessionCache = tls.NewLRUClientSessionCache(1)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: privateKey, Leaf: leaf}, config
}

func startSenderTLSServer(t *testing.T, cert tls.Certificate) (int, <-chan error) {
	t.Helper()
	listener, err := tls.Listen("tcp4", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		NextProtos:   []string{"fmsg/1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		defer close(result)
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			err = conn.SetDeadline(time.Now().Add(5 * time.Second))
			if err == nil {
				err = conn.(*tls.Conn).Handshake()
			}
			if err == nil {
				_, err = conn.Write([]byte{AcceptCodeContinue})
			}
		}
		result <- err
	}()
	t.Cleanup(func() {
		listener.Close()
		<-result
	})
	return listener.Addr().(*net.TCPAddr).Port, result
}

func TestDialTargetIPsExpiredCertificate(t *testing.T) {
	cert, config := senderTestCertificate(t, time.Now().Add(-time.Hour))
	// The fixture is trusted and has the correct hostname; expiry is the failure.
	_, err := cert.Leaf.Verify(x509.VerifyOptions{DNSName: config.ServerName, Roots: config.RootCAs})
	var invalid x509.CertificateInvalidError
	if !errors.As(err, &invalid) || invalid.Reason != x509.Expired {
		t.Fatalf("fixture verification = %v, want expired certificate", err)
	}
	port, serverResult := startSenderTLSServer(t, cert)
	conn := dialTargetIPs([]net.IP{net.ParseIP("127.0.0.1")}, port, config)
	if conn != nil {
		t.Fatalf("failed TLS dial retained a non-nil net.Conn (%T); caller must return for retry", conn)
	}
	if err := <-serverResult; err == nil {
		t.Fatal("expired certificate unexpectedly completed a TLS handshake")
	}
}

func TestDialTargetIPsFallbackAfterUnreachableIP(t *testing.T) {
	cert, config := senderTestCertificate(t, time.Now().Add(time.Hour))
	port, serverResult := startSenderTLSServer(t, cert)
	// The server listens only on 127.0.0.1, so the first loopback IP refuses TCP.
	conn := dialTargetIPs([]net.IP{net.ParseIP("127.0.0.2"), net.ParseIP("127.0.0.1")}, port, config)
	if conn == nil {
		t.Fatal("did not fall back to the reachable IP")
	}
	defer conn.Close()
	if addr := conn.RemoteAddr().(*net.TCPAddr); !addr.IP.Equal(net.ParseIP("127.0.0.1")) || addr.Port != port {
		t.Fatalf("connected to %s, want the fallback server on port %d", addr, port)
	}
	state := conn.(*tls.Conn).ConnectionState()
	if !state.HandshakeComplete || state.Version != tls.VersionTLS13 || state.NegotiatedProtocol != "fmsg/1" || len(state.VerifiedChains) == 0 {
		t.Fatalf("fallback did not establish verified TLS 1.3 with fmsg/1: %+v", state)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var response [1]byte
	if _, err := io.ReadFull(conn, response[:]); err != nil {
		t.Fatalf("fallback connection is not usable: %v", err)
	}
	if response[0] != AcceptCodeContinue {
		t.Fatalf("response = %d, want %d", response[0], AcceptCodeContinue)
	}
	if err := <-serverResult; err != nil {
		t.Fatalf("fallback server: %v", err)
	}
}
