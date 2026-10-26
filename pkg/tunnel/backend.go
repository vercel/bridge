package tunnel

import (
	"context"
	"net"
	"strconv"
)

// Backend carries a tunnel's conns over one multiplexed transport: a bridge
// server's TunnelNetwork stream (NewStreamBackend), or a dev tunnel's Connect
// stream (devtunnel.Backend).
type Backend interface {
	// Run carries frames until ctx ends or the transport does, passing each of
	// the peer's to deliver in order. deliver blocks only while a conn has too
	// much queued, and only for a backend without flow control.
	Run(ctx context.Context, deliver func(Frame)) error
	// ConnID numbers a conn this side opens.
	ConnID(conn *ConnInfo) string
	// Open tells the peer about a conn this side opens, ahead of its Data.
	Open(ctx context.Context, id string, conn *ConnInfo) error
	// Send queues a frame for the peer.
	Send(f Frame)
	// Release forgets a conn that's over.
	Release(id string)
	// Window is what each side of a conn may send before the other grants
	// more, or 0 if the transport has no flow control.
	Window() int
	// HalfClose reports whether the transport carries a conn's half-close.
	// Without it, a conn ends at either side's EOF.
	HalfClose() bool
}

type FrameKind uint8

const (
	// The peer opened a conn to Conn.Dest.
	FrameOpen FrameKind = iota + 1
	// Payload for a conn. From a peer that sends no Open, Data for an unknown
	// conn opens it when it has a Conn.
	FrameData
	// The sender has nothing more to send on the conn.
	FrameCloseWrite
	// The conn is abandoned in both directions, for Reason.
	FrameReset
	// The receiver may send Bytes more of the conn's Data.
	FrameWindow
)

// Frame is one message about one conn.
type Frame struct {
	Kind    FrameKind
	ConnID  string
	Payload []byte
	Bytes   int
	Reason  string
	// Where the conn goes. Set on Open, and on every frame of a backend that
	// addresses each one.
	Conn *ConnInfo
}

// Addr is a host (an IP address or a hostname) and a port.
type Addr struct {
	Host string
	Port int
}

func (a Addr) String() string { return net.JoinHostPort(a.Host, strconv.Itoa(a.Port)) }

// ConnInfo addresses a conn, as the side sending on it sees it.
type ConnInfo struct {
	Source, Dest Addr
	// The DNS name Dest was looked up as, if any.
	Hostname string
}
