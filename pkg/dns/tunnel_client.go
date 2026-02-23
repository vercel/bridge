package dns

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/vercel/bridge/pkg/tunnel"
)

// tunnelDNSResolver is the subset of tunnel.TunnelDialer needed by TunnelExchangeClient.
type tunnelDNSResolver interface {
	ResolveDNS(ctx context.Context, hostname string) (*tunnel.DNSResolveResult, error)
}

const defaultTunnelDNSTimeout = 5 * time.Second

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

	ctx, cancel := context.WithTimeout(ctx, defaultTunnelDNSTimeout)
	defer cancel()

	resp, err := c.tunnel.ResolveDNS(ctx, name)
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

// matchesPattern checks if the hostname matches any configured pattern.
func (c *TunnelExchangeClient) matchesPattern(hostname string) bool {
	for _, pattern := range c.patterns {
		if matchWildcard(pattern, hostname) {
			return true
		}
	}
	return false
}
