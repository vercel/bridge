package tunnel

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"

	"github.com/puzpuzpuz/xsync/v3"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/mitm"
	"github.com/vercel/bridge/pkg/plumbing"
)

// Stream abstracts a bidirectional gRPC stream. Both the server-side
// BidiStreamingServer and client-side TunnelNetworkClient satisfy this
// interface directly since both directions use TunnelNetworkMessage.
type Stream interface {
	Send(*bridgev1.TunnelNetworkMessage) error
	Recv() (*bridgev1.TunnelNetworkMessage, error)
}

// Tunnel is a wrapper around a Tunnel gRPC stream that is responsible for managing the connections it is currently multiplexing. It is intended to be used by both client and server-side
type Tunnel interface {
	// AddConn Adds a connection to the map of connections that are tracked under this Tunnel. When a conn is added, it is placed into a sync map and a goroutine is spawned that reads from the conn. Whenever data is received from the conn, its data is forwarded to the underlying gRPC stream. A destination override may be provided to override the call for original destination from the Conn. The hostname is the DNS name associated with this connection (may be empty).
	AddConn(conn net.Conn, destOverride string, hostname string)

	// Start starts the pump for the Tunnel. It has a loop for receiving messages which are routed based on their connection ID.
	Start(ctx context.Context)

	// Close cancels the tunnel context, closing all connections and stopping all goroutines.
	Close() error

	// Done returns a channel that is closed when the recv pump exits.
	Done() <-chan struct{}
}

// Option configures a Tunnel.
type Option func(*tunnelImpl)

// WithHijacker sets a hijacker that can intercept outbound connections
// before the tunnel dials the real destination.
func WithHijacker(h mitm.Hijacker) Option {
	return func(t *tunnelImpl) { t.hijacker = h }
}

// New creates a Tunnel. When a message is received down the stream and a corresponding connection ID cannot be found, the dialer is called to instantiate that connection. The client-side uses the StaticPortDialer while the server-side uses a standard ContextDialer.
func New(dialer plumbing.ContextDialer, stream Stream, opts ...Option) Tunnel {
	t := &tunnelImpl{
		dialer: dialer,
		stream: stream,
		sendCh: make(chan *bridgev1.TunnelNetworkMessage, 64),
		conns:  xsync.NewMapOf[string, net.Conn](),
		done:   make(chan struct{}),
	}
	for _, o := range opts {
		o(t)
	}
	return t
}

type tunnelImpl struct {
	dialer    plumbing.ContextDialer
	hijacker  mitm.Hijacker
	stream    Stream
	sendCh    chan *bridgev1.TunnelNetworkMessage
	conns     *xsync.MapOf[string, net.Conn]
	done      chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
}

func (t *tunnelImpl) Done() <-chan struct{} { return t.done }

func (t *tunnelImpl) AddConn(conn net.Conn, destOverride string, hostname string) {
	srcAddr := conn.RemoteAddr().String()
	dstAddr := conn.LocalAddr().String()
	if destOverride != "" {
		dstAddr = destOverride
	}

	srcHost, srcPortStr, _ := net.SplitHostPort(srcAddr)
	srcPort, _ := strconv.Atoi(srcPortStr)

	dstHost, dstPortStr, _ := net.SplitHostPort(dstAddr)
	dstPort, _ := strconv.Atoi(dstPortStr)

	connID := plumbing.ConnectionID(srcHost, srcPort, dstHost, dstPort)
	src := &bridgev1.TunnelAddress{Ip: srcHost, Port: int32(srcPort)}
	dst := &bridgev1.TunnelAddress{Ip: dstHost, Port: int32(dstPort)}

	slog.Debug("Tunnel: adding connection", "conn_id", connID, "src", srcAddr, "dst", dstAddr, "hostname", hostname)

	t.conns.Store(connID, conn)
	go t.readFromConn(conn, connID, src, dst, hostname)
}

// readFromConn reads from a net.Conn and forwards data to the stream via sendCh.
func (t *tunnelImpl) readFromConn(conn net.Conn, connID string, src, dst *bridgev1.TunnelAddress, hostname string) {
	defer func() {
		conn.Close()
		t.conns.Delete(connID)
	}()

	buf := make([]byte, 32*1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			select {
			case t.sendCh <- &bridgev1.TunnelNetworkMessage{
				ConnectionId: connID,
				Source:       src,
				Dest:         dst,
				Data:         data,
				Hostname:     hostname,
			}:
			case <-t.ctx.Done():
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (t *tunnelImpl) Close() error {
	t.closeOnce.Do(func() {
		t.cancel()
		t.closeAll()
		close(t.done)
	})
	return nil
}

func (t *tunnelImpl) Start(ctx context.Context) {
	t.ctx, t.cancel = context.WithCancel(ctx)

	// Single goroutine drains the send channel onto the stream.
	go func() {
		for {
			select {
			case msg, ok := <-t.sendCh:
				if !ok {
					return
				}
				if err := t.stream.Send(msg); err != nil {
					slog.Info("Tunnel: stream send error", "conn_id", msg.GetConnectionId(), "error", err)
					return
				}
				slog.Debug("Tunnel: sent", "conn_id", msg.GetConnectionId(), "bytes", len(msg.GetData()))
			case <-t.ctx.Done():
				return
			}
		}
	}()

	// Recv pump runs until the stream closes or context is cancelled.
	go func() {
		defer t.Close()

		recvCh := make(chan *bridgev1.TunnelNetworkMessage)
		recvErr := make(chan error, 1)
		go func() {
			for {
				msg, err := t.stream.Recv()
				if err != nil {
					recvErr <- err
					return
				}
				recvCh <- msg
			}
		}()

		for {
			select {
			case msg := <-recvCh:
				connID := msg.GetConnectionId()

				// Route to existing connection.
				if conn, ok := t.conns.Load(connID); ok {
					if msg.GetError() != "" {
						conn.Close()
						t.conns.Delete(connID)
						continue
					}
					if data := msg.GetData(); len(data) > 0 {
						if _, err := conn.Write(data); err != nil {
							slog.Debug("Write to conn failed", "connection_id", connID, "error", err)
							conn.Close()
							t.conns.Delete(connID)
						}
					}
					continue
				}

				// Unknown connection ID → dial via the configured dialer.
				go t.handleNewConn(msg)

			case err := <-recvErr:
				if err != io.EOF {
					slog.Info("Tunnel: stream recv error", "error", err)
				} else {
					slog.Info("Tunnel: stream closed (EOF)")
				}
				return

			case <-t.ctx.Done():
				return
			}
		}
	}()
}

func (t *tunnelImpl) handleNewConn(msg *bridgev1.TunnelNetworkMessage) {
	dest := msg.GetDest()
	if dest == nil {
		slog.Info("Tunnel: ignoring message with no dest", "conn_id", msg.GetConnectionId())
		return
	}

	connID := msg.GetConnectionId()
	hostname := msg.GetHostname()

	// Resolve the connection: hijacker gets first shot, then fall through to the dialer.
	var conn net.Conn
	var err error

	if t.hijacker != nil && t.hijacker.ShouldHijack(msg) {
		conn, err = t.hijacker.Hijack(t.ctx, msg)
		if err != nil {
			slog.Info("Tunnel: hijack failed, falling back to dialer", "conn_id", connID, "hostname", hostname, "error", err)
		}
	}

	if conn == nil {
		addr := fmt.Sprintf("%s:%d", dest.GetIp(), dest.GetPort())
		conn, err = t.dialer.DialContext(t.ctx, "tcp", addr)
	}

	if err != nil {
		slog.Info("Tunnel: connect failed", "conn_id", connID, "hostname", hostname, "error", err)
		select {
		case t.sendCh <- &bridgev1.TunnelNetworkMessage{
			ConnectionId: connID,
			Error:        err.Error(),
		}:
		case <-t.ctx.Done():
		}
		return
	}

	t.conns.Store(connID, conn)
	go t.readFromConn(conn, connID, msg.GetDest(), msg.GetSource(), hostname)

	if data := msg.GetData(); len(data) > 0 {
		if _, err := conn.Write(data); err != nil {
			slog.Debug("Failed to write initial data", "connection_id", connID, "error", err)
			conn.Close()
			t.conns.Delete(connID)
		}
	}
}

func (t *tunnelImpl) closeAll() {
	t.conns.Range(func(key string, conn net.Conn) bool {
		conn.Close()
		t.conns.Delete(key)
		return true
	})
}
