package health

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func trustedTLSConfig(srv *httptest.Server) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return &tls.Config{RootCAs: pool, ServerName: "example.com"}
}

func testPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	port, _ := strconv.Atoi(portStr)
	return port
}

func TestCheck_ValidCert(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &Prober{
		Port:      testPort(t, srv),
		Timeout:   5 * time.Second,
		TLSConfig: trustedTLSConfig(srv),
	}

	if !p.Check(context.Background(), "127.0.0.1") {
		t.Error("expected TLS check to succeed with trusted CA and matching server name")
	}
}

func TestCheck_UntrustedCert(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	p := &Prober{
		Port:       testPort(t, srv),
		Timeout:    5 * time.Second,
		ServerName: "example.com",
		TLSConfig:  &tls.Config{RootCAs: x509.NewCertPool()},
	}

	if p.Check(context.Background(), "127.0.0.1") {
		t.Error("expected TLS check to fail with untrusted cert")
	}
}

func TestCheck_WrongServerName(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	p := &Prober{
		Port:       testPort(t, srv),
		Timeout:    5 * time.Second,
		ServerName: "wrong.invalid",
		TLSConfig:  &tls.Config{RootCAs: pool},
	}

	if p.Check(context.Background(), "127.0.0.1") {
		t.Error("expected TLS check to fail with wrong server name")
	}
}

func TestCheck_Refused(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	p := &Prober{Port: port, Timeout: 2 * time.Second, ServerName: "example.com"}

	if p.Check(context.Background(), "127.0.0.1") {
		t.Error("expected TLS check to fail on closed port")
	}
}

func TestCheck_PlainTCP(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	p := &Prober{Port: port, Timeout: 2 * time.Second, ServerName: "example.com"}

	if p.Check(context.Background(), "127.0.0.1") {
		t.Error("expected TLS handshake to fail against plain TCP")
	}
}

func TestCheckAll(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	p := &Prober{
		Port:      testPort(t, srv),
		Timeout:   2 * time.Second,
		TLSConfig: trustedTLSConfig(srv),
	}

	ips := map[string]string{
		"good-node": "127.0.0.1",
		"bad-node":  "192.0.2.1",
	}

	results := p.CheckAll(context.Background(), ips)

	if !results["good-node"] {
		t.Error("expected good-node to be reachable")
	}
	if results["bad-node"] {
		t.Error("expected bad-node to be unreachable")
	}
}
