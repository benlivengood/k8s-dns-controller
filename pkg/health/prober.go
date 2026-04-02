package health

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"time"
)

// Prober holds configuration for TLS health checks.
type Prober struct {
	Port       int
	Timeout    time.Duration
	ServerName string      // expected TLS server name (e.g. "*.k8s.example.com")
	TLSConfig  *tls.Config // optional override; nil uses system trust store
}

// Check attempts a TLS handshake to ip:port over IPv4 and validates the
// server certificate chain and server name. Returns true if the handshake
// and certificate validation succeed.
func (p *Prober) Check(ctx context.Context, ip string) bool {
	addr := net.JoinHostPort(ip, fmt.Sprintf("%d", p.Port))

	dialer := &net.Dialer{Timeout: p.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp4", addr)
	if err != nil {
		return false
	}

	var cfg *tls.Config
	if p.TLSConfig != nil {
		cfg = p.TLSConfig.Clone()
	} else {
		cfg = &tls.Config{}
	}
	if p.ServerName != "" && cfg.ServerName == "" {
		cfg.ServerName = p.ServerName
	}

	tlsConn := tls.Client(conn, cfg)
	tlsConn.SetDeadline(time.Now().Add(p.Timeout))

	err = tlsConn.Handshake()
	tlsConn.Close()
	return err == nil
}

// CheckAll probes every IP concurrently and returns which nodes succeeded.
// The ips map is node-name -> IP-address.
func (p *Prober) CheckAll(ctx context.Context, ips map[string]string) map[string]bool {
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
			results <- result{node: node, ok: p.Check(ctx, ip)}
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
