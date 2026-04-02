package health

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"time"
)

// CheckTLS attempts a TLS handshake to ip:port over IPv4, accepting any
// certificate. Returns true if the handshake completes successfully.
func CheckTLS(ctx context.Context, ip string, port int, timeout time.Duration) bool {
	addr := net.JoinHostPort(ip, fmt.Sprintf("%d", port))

	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp4", addr)
	if err != nil {
		return false
	}

	tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
	tlsConn.SetDeadline(time.Now().Add(timeout))

	err = tlsConn.Handshake()
	tlsConn.Close()
	return err == nil
}

// CheckAll probes every IP concurrently and returns which nodes succeeded.
// The ips map is node-name -> IP-address.
func CheckAll(ctx context.Context, ips map[string]string, port int, timeout time.Duration) map[string]bool {
	type result struct {
		node string
		ok   bool
	}

	var wg sync.WaitGroup
	results := make(chan result, len(ips))

	for node, ip := range ips {
		wg.Add(1)
		go func(node, ip string) {
			defer wg.Done()
			results <- result{node: node, ok: CheckTLS(ctx, ip, port, timeout)}
		}(node, ip)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	out := make(map[string]bool, len(ips))
	for r := range results {
		out[r.node] = r.ok
	}
	return out
}
