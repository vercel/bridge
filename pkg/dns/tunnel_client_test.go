package dns

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTunnel is a tunnelDNSResolver test double whose ResolveDNS behaviour is
// controlled via the responder fn. attempts counts how many times it was
// invoked so tests can verify retry semantics.
type fakeTunnel struct {
	attempts  atomic.Int32
	responder func(call int32) (*DNSResolveResult, error)
}

func (f *fakeTunnel) ResolveDNS(_ context.Context, _ string) (*DNSResolveResult, error) {
	n := f.attempts.Add(1)
	return f.responder(n)
}

// newTunnelClient builds a TunnelExchangeClient with the given fake; patterns
// default to "*" so every name is matched.
func newTunnelClient(t *testing.T, tun *fakeTunnel) *TunnelExchangeClient {
	t.Helper()
	c := NewTunnelExchangeClient([]string{"*"}, tun, "")
	return c
}

func aQuery(name string) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	return m
}

func TestTunnelExchange_FirstAttemptSucceeds(t *testing.T) {
	tun := &fakeTunnel{
		responder: func(_ int32) (*DNSResolveResult, error) {
			return &DNSResolveResult{Addresses: []string{"203.0.113.1"}}, nil
		},
	}
	c := newTunnelClient(t, tun)

	reply, intercepted, err := c.ExchangeContext(context.Background(), aQuery("example.com"))
	require.NoError(t, err)
	assert.True(t, intercepted)
	require.Len(t, reply.Answer, 1)
	assert.Equal(t, int32(1), tun.attempts.Load(), "must not retry on success")
}

func TestTunnelExchange_RetriesOnTransientError(t *testing.T) {
	tun := &fakeTunnel{
		responder: func(n int32) (*DNSResolveResult, error) {
			if n < tunnelDNSMaxAttempts {
				return nil, errors.New("transient gRPC error")
			}
			return &DNSResolveResult{Addresses: []string{"203.0.113.5"}}, nil
		},
	}
	c := newTunnelClient(t, tun)

	reply, intercepted, err := c.ExchangeContext(context.Background(), aQuery("example.com"))
	require.NoError(t, err)
	assert.True(t, intercepted)
	require.Len(t, reply.Answer, 1)
	assert.Equal(t, int32(tunnelDNSMaxAttempts), tun.attempts.Load())
}

func TestTunnelExchange_ReturnsErrorAfterAllRetriesFail(t *testing.T) {
	tun := &fakeTunnel{
		responder: func(_ int32) (*DNSResolveResult, error) {
			return nil, errors.New("transport down")
		},
	}
	c := newTunnelClient(t, tun)

	_, _, err := c.ExchangeContext(context.Background(), aQuery("example.com"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "transport down")
	assert.Equal(t, int32(tunnelDNSMaxAttempts), tun.attempts.Load(), "should attempt the full budget")
}

// TestTunnelExchange_DoesNotRetryDNSLevelError verifies that a successful RPC
// reporting a DNS-level error (NXDOMAIN from cluster DNS, etc.) is not retried
// — the answer is authoritative.
func TestTunnelExchange_DoesNotRetryDNSLevelError(t *testing.T) {
	tun := &fakeTunnel{
		responder: func(_ int32) (*DNSResolveResult, error) {
			return &DNSResolveResult{Error: "NXDOMAIN"}, nil
		},
	}
	c := newTunnelClient(t, tun)

	_, _, err := c.ExchangeContext(context.Background(), aQuery("nope.example.com"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NXDOMAIN")
	assert.Equal(t, int32(1), tun.attempts.Load(), "DNS-level error is authoritative — no retry")
}

func TestTunnelExchange_StopsRetryingOnContextCancel(t *testing.T) {
	tun := &fakeTunnel{
		responder: func(_ int32) (*DNSResolveResult, error) {
			return nil, errors.New("never works")
		},
	}
	c := newTunnelClient(t, tun)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := c.ExchangeContext(ctx, aQuery("example.com"))
	require.Error(t, err)
	// One attempt completes before the context-cancel check between attempts;
	// the loop must NOT make the full 3 attempts when the caller cancels.
	assert.LessOrEqual(t, tun.attempts.Load(), int32(1),
		"cancelled context should short-circuit retries; got %d attempts", tun.attempts.Load())
}

// TestTunnelExchange_NonMatchedFallsThroughWithoutRetry confirms that domains
// outside --forward-domains are delegated to the system resolver and don't
// hit the retry path.
func TestTunnelExchange_NonMatchedFallsThroughWithoutRetry(t *testing.T) {
	tun := &fakeTunnel{
		responder: func(_ int32) (*DNSResolveResult, error) {
			t.Fatal("tunnel should not be called for non-matched domains")
			return nil, nil
		},
	}
	c := &TunnelExchangeClient{
		patterns: []string{"only-this.example"},
		tunnel:   tun,
		fallback: &stubExchangeClient{reply: new(dns.Msg)},
	}

	_, _, err := c.ExchangeContext(context.Background(), aQuery("other.example"))
	require.NoError(t, err)
	assert.Equal(t, int32(0), tun.attempts.Load())
}

// stubExchangeClient is a no-op ExchangeClient for non-matched fallback paths.
type stubExchangeClient struct {
	reply *dns.Msg
}

func (s *stubExchangeClient) ExchangeContext(_ context.Context, _ *dns.Msg) (*dns.Msg, bool, error) {
	return s.reply, false, nil
}

// TestTunnelExchange_RetryBackoffDoesNotBlockForever guards against an
// unbounded backoff sequence regressing into wait-too-long territory.
func TestTunnelExchange_RetryBackoffBudget(t *testing.T) {
	tun := &fakeTunnel{
		responder: func(_ int32) (*DNSResolveResult, error) {
			return nil, errors.New("flaky")
		},
	}
	c := newTunnelClient(t, tun)

	start := time.Now()
	_, _, err := c.ExchangeContext(context.Background(), aQuery("example.com"))
	require.Error(t, err)
	// 50ms + 100ms backoff between 3 attempts. Should comfortably fit in 1s.
	assert.Less(t, time.Since(start), time.Second, "retry budget should be small")
}
