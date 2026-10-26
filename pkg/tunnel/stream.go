package tunnel

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/puzpuzpuz/xsync/v3"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/plumbing"
)

// What a conn may have queued from the peer before the stream waits for it
// to be read. The stream has no flow control of its own.
const queuedMessages = 64

// Stream abstracts a bidirectional gRPC stream. Both the server-side
// BidiStreamingServer and client-side TunnelNetworkClient satisfy this
// interface directly since both directions use TunnelNetworkMessage.
type Stream interface {
	Send(*bridgev1.TunnelNetworkMessage) error
	Recv() (*bridgev1.TunnelNetworkMessage, error)
}

// NewStreamBackend carries a tunnel over a bridge TunnelNetwork stream. Every
// message addresses its conn, the first one to an unknown conn opens it, and
// an error ends it. The stream has no half-close and no flow control.
func NewStreamBackend(stream Stream) Backend {
	return &streamBackend{
		stream: stream,
		sendCh: make(chan *bridgev1.TunnelNetworkMessage, 64),
		stop:   make(chan struct{}),
		conns:  xsync.NewMapOf[string, *messageStream](),
	}
}

type streamBackend struct {
	stream Stream
	sendCh chan *bridgev1.TunnelNetworkMessage
	// Closed once Run returns, so a Send waiting for room gives up.
	stop  chan struct{}
	conns *xsync.MapOf[string, *messageStream]
}

func (b *streamBackend) Run(ctx context.Context, accept func(net.Conn, *ConnInfo)) error {
	defer close(b.stop)
	defer b.conns.Range(func(_ string, s *messageStream) bool {
		s.end()
		return true
	})

	// Single goroutine drains the send channel onto the stream.
	go func() {
		for {
			select {
			case msg := <-b.sendCh:
				if err := b.stream.Send(msg); err != nil {
					slog.Info("Tunnel: stream send error", "conn_id", msg.GetConnectionId(), "error", err)
					return
				}
				slog.Debug("Tunnel: sent", "conn_id", msg.GetConnectionId(), "bytes", len(msg.GetData()))
			case <-ctx.Done():
				return
			}
		}
	}()

	recvCh := make(chan *bridgev1.TunnelNetworkMessage)
	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := b.stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			select {
			case recvCh <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case msg := <-recvCh:
			b.handle(ctx, msg, accept)
		case err := <-recvErr:
			if err == io.EOF {
				slog.Info("Tunnel: stream closed (EOF)")
				return nil
			}
			slog.Info("Tunnel: stream recv error", "error", err)
			return err
		case <-ctx.Done():
			return nil
		}
	}
}

// handle routes one of the peer's messages to its conn. The first message of
// an unknown conn opens it, before the dial, so the chunks that arrive during
// the dial queue on it instead of each opening another.
func (b *streamBackend) handle(ctx context.Context, msg *bridgev1.TunnelNetworkMessage, accept func(net.Conn, *ConnInfo)) {
	id := msg.GetConnectionId()
	s, ok := b.conns.Load(id)
	if msg.GetError() != "" {
		if ok {
			s.end()
		}
		return
	}
	if !ok {
		dest := msg.GetDest()
		if dest == nil {
			slog.Info("Tunnel: ignoring message with no dest", "conn_id", id)
			return
		}
		info := &ConnInfo{
			Source:   Addr{Host: msg.GetSource().GetIp(), Port: int(msg.GetSource().GetPort())},
			Dest:     Addr{Host: dest.GetIp(), Port: int(dest.GetPort())},
			Hostname: msg.GetHostname(),
		}
		// This side sends from where the peer's conn was headed, back to it.
		s = b.newStream(id, &ConnInfo{Source: info.Dest, Dest: info.Source, Hostname: info.Hostname})
		b.conns.Store(id, s)
		accept(s.conn(), info)
	}
	s.push(ctx, msg.GetData())
}

func (b *streamBackend) Open(_ context.Context, info *ConnInfo) (net.Conn, error) {
	id := plumbing.ConnectionID(info.Source.Host, info.Source.Port, info.Dest.Host, info.Dest.Port)
	s := b.newStream(id, info)
	b.conns.Store(id, s)
	// Its first Data opens it.
	return s.conn(), nil
}

func (b *streamBackend) newStream(id string, info *ConnInfo) *messageStream {
	return &messageStream{
		b:    b,
		id:   id,
		info: info,
		in:   make(chan []byte, queuedMessages),
		done: make(chan struct{}),
	}
}

func (b *streamBackend) send(msg *bridgev1.TunnelNetworkMessage) error {
	select {
	case b.sendCh <- msg:
		return nil
	case <-b.stop:
		return net.ErrClosed
	}
}

// messageStream is one conn's messages on the stream, as a plumbing.Stream.
type messageStream struct {
	b  *streamBackend
	id string
	// As this side sends it.
	info *ConnInfo
	in   chan []byte
	done chan struct{}
	once sync.Once
}

var _ plumbing.Stream = (*messageStream)(nil)

func (s *messageStream) conn() net.Conn {
	return plumbing.NewMeteredConn(plumbing.NewStreamConn(s, addr(s.info.Source.String()), addr(s.info.Dest.String())), 0)
}

// push queues payload for Recv, waiting while the conn has a full queue.
func (s *messageStream) push(ctx context.Context, payload []byte) {
	if len(payload) == 0 {
		return
	}
	select {
	case s.in <- payload:
	case <-s.done:
	case <-ctx.Done():
	}
}

func (s *messageStream) Recv() ([]byte, error) {
	// A conn the peer ended drops what it hadn't read.
	select {
	case <-s.done:
		return nil, net.ErrClosed
	default:
	}
	select {
	case p := <-s.in:
		return p, nil
	case <-s.done:
		return nil, net.ErrClosed
	}
}

func (s *messageStream) Send(p []byte) error {
	select {
	case <-s.done:
		return net.ErrClosed
	default:
	}
	return s.b.send(&bridgev1.TunnelNetworkMessage{
		ConnectionId: s.id,
		Source:       tunnelAddress(s.info.Source),
		Dest:         tunnelAddress(s.info.Dest),
		Hostname:     s.info.Hostname,
		Data:         bytes.Clone(p),
	})
}

// CloseSend ends the conn: the stream can't carry a half-close.
func (s *messageStream) CloseSend() error {
	s.end()
	return nil
}

// Close ends the conn and tells the peer.
func (s *messageStream) Close() error {
	select {
	case <-s.done:
		return nil
	default:
	}
	s.end()
	return s.b.send(&bridgev1.TunnelNetworkMessage{ConnectionId: s.id, Error: "connection closed"})
}

func (s *messageStream) Done() <-chan struct{} { return s.done }

// end forgets the conn, unless another has since taken its id.
func (s *messageStream) end() {
	s.once.Do(func() {
		close(s.done)
		s.b.conns.Compute(s.id, func(cur *messageStream, loaded bool) (*messageStream, bool) {
			if !loaded {
				return nil, true
			}
			return cur, cur == s
		})
	})
}
