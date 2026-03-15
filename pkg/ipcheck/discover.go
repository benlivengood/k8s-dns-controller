package ipcheck

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// ipv4Client forces all connections over IPv4 so whoami services return
// the node's public IPv4 address rather than an IPv6 address.
var ipv4Client = &http.Client{
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp4", addr)
		},
		ForceAttemptHTTP2: true,
		MaxIdleConns:      4,
		IdleConnTimeout:   30 * time.Second,
		DisableKeepAlives: true,
	},
}

// Provider represents a public IP discovery endpoint.
type Provider struct {
	Name string
	URL  string
}

// DefaultProviders is the built-in list of whoami services, ordered by preference.
var DefaultProviders = []Provider{
	{Name: "icanhazip", URL: "https://icanhazip.com"},
	{Name: "ifconfig.me", URL: "https://ifconfig.me/ip"},
	{Name: "ipify", URL: "https://api.ipify.org"},
	{Name: "ipecho", URL: "https://ipecho.net/plain"},
}

// Discover queries multiple providers and returns the public IP.
// It requires at least `quorum` providers to agree on the same IP
// to guard against a single provider returning garbage.
func Discover(ctx context.Context, providers []Provider, quorum int) (net.IP, error) {
	if len(providers) == 0 {
		providers = DefaultProviders
	}
	if quorum < 1 {
		quorum = 1
	}

	type result struct {
		ip  net.IP
		err error
	}

	results := make(chan result, len(providers))

	for _, p := range providers {
		go func(p Provider) {
			ip, err := query(ctx, p)
			results <- result{ip: ip, err: err}
		}(p)
	}

	votes := make(map[string]int)
	var errors []error

	for range providers {
		r := <-results
		if r.err != nil {
			errors = append(errors, r.err)
			continue
		}
		key := r.ip.String()
		votes[key]++
		if votes[key] >= quorum {
			return r.ip, nil
		}
	}

	if len(votes) > 0 {
		// No quorum but we got at least one answer — return the most common.
		var best string
		var bestCount int
		for ip, count := range votes {
			if count > bestCount {
				best = ip
				bestCount = count
			}
		}
		return net.ParseIP(best), fmt.Errorf("no quorum reached (best: %s with %d/%d votes); using best guess", best, bestCount, quorum)
	}

	return nil, fmt.Errorf("all providers failed: %v", errors)
}

func query(ctx context.Context, p Provider) (net.IP, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.Name, err)
	}
	req.Header.Set("User-Agent", "k8s-dns-controller/1.0")

	resp, err := ipv4Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.Name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: status %d", p.Name, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.Name, err)
	}

	raw := strings.TrimSpace(string(body))
	ip := net.ParseIP(raw)
	if ip == nil {
		return nil, fmt.Errorf("%s: %q is not a valid IP", p.Name, raw)
	}

	return ip, nil
}
