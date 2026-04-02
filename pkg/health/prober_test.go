package health

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCheckTLS_Success(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	_ = host
	port, _ := strconv.Atoi(portStr)

	ok := CheckTLS(context.Background(), "127.0.0.1", port, 5*time.Second)
	if !ok {
		t.Error("expected TLS check to succeed against test server")
	}
}

func TestCheckTLS_Refused(t *testing.T) {
	// Pick a port that nothing is listening on.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	ok := CheckTLS(context.Background(), "127.0.0.1", port, 2*time.Second)
	if ok {
		t.Error("expected TLS check to fail on closed port")
	}
}

func TestCheckTLS_PlainTCP(t *testing.T) {
	// Start a plain TCP listener (no TLS) -- handshake should fail.
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

	ok := CheckTLS(context.Background(), "127.0.0.1", port, 2*time.Second)
	if ok {
		t.Error("expected TLS handshake to fail against plain TCP")
	}
}

func TestCheckTLS_SelfSigned(t *testing.T) {
	// httptest.NewTLSServer uses a self-signed cert. CheckTLS should succeed
	// because InsecureSkipVerify is true.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	port, _ := strconv.Atoi(portStr)

	// Verify the cert IS self-signed (sanity check).
	conn, err := tls.Dial("tcp", srv.Listener.Addr().String(), &tls.Config{InsecureSkipVerify: false})
	if err == nil {
		conn.Close()
		t.Skip("test cert unexpectedly trusted")
	}

	ok := CheckTLS(context.Background(), "127.0.0.1", port, 5*time.Second)
	if !ok {
		t.Error("expected self-signed TLS to succeed with InsecureSkipVerify")
	}
}

func TestCheckAll(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	port, _ := strconv.Atoi(portStr)

	ips := map[string]string{
		"good-node": "127.0.0.1",
		"bad-node":  "192.0.2.1", // TEST-NET, unreachable
	}

	results := CheckAll(context.Background(), ips, port, 2*time.Second)

	if !results["good-node"] {
		t.Error("expected good-node to be reachable")
	}
	if results["bad-node"] {
		t.Error("expected bad-node to be unreachable")
	}
}
