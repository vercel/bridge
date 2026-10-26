package tunnel

import (
	"context"
	"net"
	"strconv"
)

// Backend carries a tunnel's conns over one multiplexed transport: a bridge
// server's TunnelNetwork stream (NewStreamBackend), or a dev tunnel's Connect
// stream (devtunnel.Backend). Each conn is a net.Conn over one of its streams;
// its CloseWrite half-closes it and Close resets it.
type Backend interface {
	// Run carries the transport until ctx ends or the transport does, handing
	// accept each conn the peer opens, with where it goes. It ends every conn
	// before returning.
	Run(ctx context.Context, accept func(conn net.Conn, info *ConnInfo)) error
	// Open opens a conn through the transport to info.Dest.
	Open(ctx context.Context, info *ConnInfo) (net.Conn, error)
}

// Addr is a host (an IP address or a hostname) and a port.
type Addr struct {
	Host string
	Port int
}

func (a Addr) String() string { return net.JoinHostPort(a.Host, strconv.Itoa(a.Port)) }

// ConnInfo addresses a conn.
type ConnInfo struct {
	Source, Dest Addr
	// The DNS name Dest was looked up as, if any.
	Hostname string
}

type addr string

func (a addr) Network() string { return "tunnel" }
func (a addr) String() string  { return string(a) }
