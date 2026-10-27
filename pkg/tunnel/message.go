package tunnel

import (
	"net"
	"strconv"
)

// Stream carries a tunnel's Messages over one transport: a bridge
// TunnelNetwork stream (bridgestream), or a dev tunnel's Connect stream
// (devtunnel). It translates them to and from the transport's own.
type Stream interface {
	// Send sends m to the peer. It's called from many goroutines at once.
	Send(m *Message) error
	// Recv returns the peer's next Message. An error ends the tunnel.
	Recv() (*Message, error)
	// Features says what the transport carries beyond Open, Data and Reset.
	Features() Features
}

// Features are what a transport carries beyond Open, Data and Reset.
type Features struct {
	// Window is what each side of a conn may send before the other grants
	// more with a Window message; 0 for no flow control.
	Window int
	// HalfClose: a CloseWrite reaches the peer, which can still send. Without
	// it, a conn ends at either side's EOF.
	HalfClose bool
}

type MessageKind uint8

const (
	// A new conn to Conn.Dest.
	MessageOpen MessageKind = iota + 1
	// Payload for a conn.
	MessageData
	// The sender has nothing more to send on the conn.
	MessageCloseWrite
	// The conn is abandoned in both directions, for Reason.
	MessageReset
	// The receiver may send Bytes more of the conn's Data.
	MessageWindow
)

// Message is one message of the tunnel's own protocol, about one conn.
type Message struct {
	Kind   MessageKind
	ConnID string
	// Open only.
	Conn    *ConnInfo
	Payload []byte
	Reason  string
	Bytes   int
}

// Addr is a host (an IP address or a hostname) and a port.
type Addr struct {
	Host string
	Port int
}

func (a Addr) String() string { return net.JoinHostPort(a.Host, strconv.Itoa(a.Port)) }

// ConnInfo addresses a conn, as the side opening it sees it.
type ConnInfo struct {
	Source, Dest Addr
	// The DNS name Dest was looked up as, if any.
	Hostname string
}

// Reply addresses the other way: from where the conn was headed, back to
// where it came from.
func (c *ConnInfo) Reply() *ConnInfo {
	return &ConnInfo{Source: c.Dest, Dest: c.Source, Hostname: c.Hostname}
}

// SplitAddr parses "host:port".
func SplitAddr(s string) Addr {
	host, portStr, _ := net.SplitHostPort(s)
	port, _ := strconv.Atoi(portStr)
	return Addr{Host: host, Port: port}
}
