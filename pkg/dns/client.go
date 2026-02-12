package dns

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/miekg/dns"
)

// ExchangeClient resolves DNS queries. The intercepted flag tells the DNS
// server whether the response needs proxy-IP allocation (true) or should be
// written through as-is (false).
type ExchangeClient interface {
	ExchangeContext(ctx context.Context, msg *dns.Msg) (resp *dns.Msg, intercepted bool, err error)
}

var _ ExchangeClient = (*SystemExchangeClient)(nil)

// SystemExchangeClient resolves queries via an upstream DNS server using the
// standard miekg/dns client. It always returns intercepted=false.
type SystemExchangeClient struct {
	upstream string
	client   *dns.Client
}

// NewSystemExchangeClient creates a SystemExchangeClient. If upstream is empty,
// the system resolver from /etc/resolv.conf is used.
func NewSystemExchangeClient(upstream string) *SystemExchangeClient {
	if upstream == "" {
		upstream = getSystemResolver()
	}
	return &SystemExchangeClient{
		upstream: upstream,
		client:   &dns.Client{},
	}
}

// ExchangeContext forwards the query to the upstream DNS server.
func (c *SystemExchangeClient) ExchangeContext(ctx context.Context, msg *dns.Msg) (*dns.Msg, bool, error) {
	resp, _, err := c.client.ExchangeContext(ctx, msg, c.upstream)
	if err != nil {
		return nil, false, fmt.Errorf("upstream exchange: %w", err)
	}
	slog.Debug("System DNS exchange", "upstream", c.upstream, "questions", len(msg.Question))
	return resp, false, nil
}
