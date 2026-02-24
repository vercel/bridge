package plumbing

import (
	"context"
	"fmt"
	"net"
	"time"
)

// ContextDialer matches the DialContext method on net.Dialer.
type ContextDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// StaticPortDialer replaces the port of every dial address with a fixed port.
type StaticPortDialer struct {
	Port   int
	Dialer ContextDialer
}

// NewStaticPortDialer creates a StaticPortDialer that rewrites all addresses
// to use port and delegates to the provided dialer (or &net.Dialer{} if nil).
func NewStaticPortDialer(port int, dialer ContextDialer) *StaticPortDialer {
	if dialer == nil {
		dialer = &net.Dialer{Timeout: 10 * time.Second}
	}
	return &StaticPortDialer{Port: port, Dialer: dialer}
}

func (d *StaticPortDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid address %q: %w", address, err)
	}
	return d.Dialer.DialContext(ctx, network, net.JoinHostPort(host, fmt.Sprintf("%d", d.Port)))
}

var _ ContextDialer = (*StaticPortDialer)(nil)
var _ ContextDialer = (*net.Dialer)(nil)
