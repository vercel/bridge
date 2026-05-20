package dns

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// DNSResolveResult holds the result of a DNS resolution.
type DNSResolveResult struct {
	Addresses []string
	Error     string
}

// tunnelDNSResolver resolves hostnames via the bridge proxy (gRPC or other transport).
type tunnelDNSResolver interface {
	ResolveDNS(ctx context.Context, hostname string) (*DNSResolveResult, error)
}

const (
	defaultTunnelDNSTimeout = 5 * time.Second

	// tunnelDNSMaxAttempts is the total number of attempts for a single
	// tunnel DNS resolution before surfacing an error. Transient gRPC
	// blips (port-forward reconnects, in-flight stream drops) usually
	// recover within a few tens of milliseconds; a small retry budget
	// absorbs them without making bad-state cases hang.
	tunnelDNSMaxAttempts = 3

	// tunnelDNSBaseBackoff is the initial backoff between retries; each
	// attempt doubles it.
	tunnelDNSBaseBackoff = 50 * time.Millisecond
)

var _ ExchangeClient = (*TunnelExchangeClient)(nil)

// TunnelExchangeClient routes DNS queries that match configured patterns
// through the tunnel, and delegates everything else to a SystemExchangeClient.
type TunnelExchangeClient struct {
	patterns []string
	tunnel   tunnelDNSResolver
	fallback ExchangeClient
}

// NewTunnelExchangeClient creates a TunnelExchangeClient.
// Patterns are lowered and trimmed. If upstream is empty, the system resolver
// from /etc/resolv.conf is used for fallback queries.
func NewTunnelExchangeClient(patterns []string, tunnelClient tunnelDNSResolver, upstream string) *TunnelExchangeClient {
	normalized := make([]string, 0, len(patterns))
	for _, p := range patterns {
		normalized = append(normalized, strings.ToLower(strings.TrimSpace(p)))
	}
	return &TunnelExchangeClient{
		patterns: normalized,
		tunnel:   tunnelClient,
		fallback: NewSystemExchangeClient(upstream),
	}
}

// ExchangeContext resolves the query. Matched A-record queries are resolved through
// the tunnel; everything else goes to the fallback system resolver.
func (c *TunnelExchangeClient) ExchangeContext(ctx context.Context, msg *dns.Msg) (*dns.Msg, bool, error) {
	if len(msg.Question) == 0 {
		return nil, false, fmt.Errorf("empty question section")
	}

	q := msg.Question[0]
	name := strings.ToLower(strings.TrimSuffix(q.Name, "."))

	// For matched domains, handle both A and AAAA queries.
	// musl (Alpine) sends A and AAAA in parallel; if the AAAA falls through
	// to the system resolver and gets NXDOMAIN, musl discards the A result.
	// Return an empty NOERROR for AAAA on matched domains so musl uses the A answer.
	if q.Qtype == dns.TypeAAAA && c.matchesPattern(name) {
		reply := new(dns.Msg)
		reply.SetReply(msg)
		return reply, false, nil
	}

	// Only intercept A queries through the tunnel
	if q.Qtype != dns.TypeA {
		return c.fallback.ExchangeContext(ctx, msg)
	}

	if !c.matchesPattern(name) {
		return c.fallback.ExchangeContext(ctx, msg)
	}

	slog.Debug("Resolving via tunnel", "hostname", name)

	resp, err := c.resolveWithRetry(ctx, name)
	if err != nil {
		return nil, false, fmt.Errorf("tunnel DNS resolution for %s: %w", name, err)
	}

	if resp.Error != "" {
		return nil, false, fmt.Errorf("tunnel DNS error for %s: %s", name, resp.Error)
	}

	// Build a dns.Msg from the tunnel response
	reply := new(dns.Msg)
	reply.SetReply(msg)
	reply.Authoritative = false

	for _, addr := range resp.Addresses {
		ip := net.ParseIP(addr)
		if ip == nil || ip.To4() == nil {
			continue
		}
		reply.Answer = append(reply.Answer, &dns.A{
			Hdr: dns.RR_Header{
				Name:   q.Name,
				Rrtype: dns.TypeA,
				Class:  dns.ClassINET,
				Ttl:    1,
			},
			A: ip.To4(),
		})
	}

	return reply, true, nil
}

// resolveWithRetry calls the underlying tunnel resolver up to
// tunnelDNSMaxAttempts times. Each attempt has its own deadline derived from
// defaultTunnelDNSTimeout; the parent context still bounds the total budget.
// Only RPC-level transport errors are retried — a successful RPC that
// reports a DNS-level error (resp.Error) is returned to the caller as-is.
func (c *TunnelExchangeClient) resolveWithRetry(parent context.Context, name string) (*DNSResolveResult, error) {
	var lastErr error
	backoff := tunnelDNSBaseBackoff
	for attempt := 1; attempt <= tunnelDNSMaxAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(parent, defaultTunnelDNSTimeout)
		resp, err := c.tunnel.ResolveDNS(ctx, name)
		cancel()
		if err == nil {
			return resp, nil
		}
		lastErr = err

		// Stop retrying if the caller cancelled.
		if parent.Err() != nil {
			break
		}
		if attempt == tunnelDNSMaxAttempts {
			break
		}

		slog.Debug("Tunnel DNS attempt failed; retrying",
			"hostname", name, "attempt", attempt, "error", err, "backoff", backoff)
		select {
		case <-parent.Done():
			return nil, parent.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return nil, lastErr
}

// matchesPattern checks if the hostname matches any configured pattern.
func (c *TunnelExchangeClient) matchesPattern(hostname string) bool {
	for _, pattern := range c.patterns {
		if matchWildcard(pattern, hostname) {
			return true
		}
	}
	return false
}
