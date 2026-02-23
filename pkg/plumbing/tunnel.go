package plumbing

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
)

// TunnelStream abstracts a bidirectional gRPC stream. Both the server-side
// BidiStreamingServer and client-side TunnelNetworkClient satisfy this
// interface directly since both directions use TunnelNetworkMessage.
type TunnelStream interface {
	Send(*bridgev1.TunnelNetworkMessage) error
	Recv() (*bridgev1.TunnelNetworkMessage, error)
}

// Tunnel is a wrapper around a Tunnel gRPC stream that is responsible for managing the connections it is currently multiplexing. It is intended to be used by both client and server-side
type Tunnel interface {
	// AddConn Adds a connection to the map of connections that are tracked under this Tunnel. When a conn is added, it is placed into a sync map and a goroutine is spawned that reads from the conn. Whenever data is received from the conn, its data is forwarded to the underlying gRPC stream. A destination override may be provided to override the call for original destination from the Conn.
	AddConn(conn net.Conn, destOverride string)

	// Start starts the pump for the Tunnel. It has a loop for receiving messages which are routed based on their connection ID.
	Start(ctx context.Context)

	// Close cancels the tunnel context, closing all connections and stopping all goroutines.
	Close() error

	// Done returns a channel that is closed when the recv pump exits.
	Done() <-chan struct{}
}

// NewTunnel constructor for Tunnel. When a message is received down the stream and a corresponding connection ID cannot be found, the dialer is called to instantiate that connection. The client-side uses the StaticPortDialer while the server-side uses a standard ContextDialer.
func NewTunnel(dialer ContextDialer, stream TunnelStream) Tunnel {
	return &tunnel{
		dialer: dialer,
		stream: stream,
		sendCh: make(chan *bridgev1.TunnelNetworkMessage, 64),
		conns:  xsync.NewMapOf[string, net.Conn](),
		done:   make(chan struct{}),
	}
}

type tunnel struct {
	dialer    ContextDialer
	stream    TunnelStream
	sendCh    chan *bridgev1.TunnelNetworkMessage
	conns     *xsync.MapOf[string, net.Conn]
	done      chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
}

func (t *tunnel) Done() <-chan struct{} { return t.done }

func (t *tunnel) AddConn(conn net.Conn, destOverride string) {
	srcAddr := conn.RemoteAddr().String()
	dstAddr := conn.LocalAddr().String()
	if destOverride != "" {
		dstAddr = destOverride
	}

	srcHost, srcPortStr, _ := net.SplitHostPort(srcAddr)
	srcPort, _ := strconv.Atoi(srcPortStr)

	dstHost, dstPortStr, _ := net.SplitHostPort(dstAddr)
	dstPort, _ := strconv.Atoi(dstPortStr)

	connID := ConnectionID(srcHost, srcPort, dstHost, dstPort)
	src := &bridgev1.TunnelAddress{Ip: srcHost, Port: int32(srcPort)}
	dst := &bridgev1.TunnelAddress{Ip: dstHost, Port: int32(dstPort)}

	slog.Debug("Tunnel: adding connection", "conn_id", connID, "src", srcAddr, "dst", dstAddr)

	t.conns.Store(connID, conn)
	go t.readFromConn(conn, connID, src, dst)
}

// readFromConn reads from a net.Conn and forwards data to the stream via sendCh.
func (t *tunnel) readFromConn(conn net.Conn, connID string, src, dst *bridgev1.TunnelAddress) {
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

func (t *tunnel) Close() error {
	t.closeOnce.Do(func() {
		t.cancel()
		t.closeAll()
		close(t.done)
	})
	return nil
}

func (t *tunnel) Start(ctx context.Context) {
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

func (t *tunnel) handleNewConn(msg *bridgev1.TunnelNetworkMessage) {
	dest := msg.GetDest()
	if dest == nil {
		slog.Info("Tunnel: ignoring message with no dest", "conn_id", msg.GetConnectionId())
		return
	}

	addr := fmt.Sprintf("%s:%d", dest.GetIp(), dest.GetPort())
	slog.Info("Tunnel: dialing new connection", "conn_id", msg.GetConnectionId(), "dest", addr)
	conn, err := t.dialer.DialContext(t.ctx, "tcp", addr)
	if err != nil {
		slog.Info("Tunnel: dial failed", "conn_id", msg.GetConnectionId(), "dest", addr, "error", err)
		select {
		case t.sendCh <- &bridgev1.TunnelNetworkMessage{
			ConnectionId: msg.GetConnectionId(),
			Error:        fmt.Sprintf("failed to dial %s: %v", addr, err),
		}:
		case <-t.ctx.Done():
		}
		return
	}

	slog.Info("Tunnel: dial succeeded", "conn_id", msg.GetConnectionId(), "dest", addr)

	connID := msg.GetConnectionId()
	t.conns.Store(connID, conn)

	// Swap source/dest for the return direction.
	go t.readFromConn(conn, connID, msg.GetDest(), msg.GetSource())

	if data := msg.GetData(); len(data) > 0 {
		if _, err := conn.Write(data); err != nil {
			slog.Debug("Failed to write initial data", "connection_id", connID, "error", err)
			conn.Close()
			t.conns.Delete(connID)
		}
	}
}

func (t *tunnel) closeAll() {
	t.conns.Range(func(key string, conn net.Conn) bool {
		conn.Close()
		t.conns.Delete(key)
		return true
	})
}
