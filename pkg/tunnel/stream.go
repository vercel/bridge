package tunnel

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"

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
		conns:  xsync.NewMapOf[string, *messageFrames](),
	}
}

type streamBackend struct {
	stream Stream
	sendCh chan *bridgev1.TunnelNetworkMessage
	// Closed once Run returns, so a Send waiting for room gives up.
	stop  chan struct{}
	conns *xsync.MapOf[string, *messageFrames]
}

func (b *streamBackend) Run(ctx context.Context, accept func(net.Conn, *ConnInfo)) error {
	defer close(b.stop)
	defer b.conns.Range(func(_ string, f *messageFrames) bool {
		f.stream.PeerReset()
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
	f, ok := b.conns.Load(id)
	if msg.GetError() != "" {
		if ok {
			f.stream.PeerReset()
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
		f = b.newConn(id, &ConnInfo{Source: info.Dest, Dest: info.Source, Hostname: info.Hostname})
		accept(f.stream.Conn(), info)
	}
	f.stream.Deliver(ctx, msg.GetData())
}

func (b *streamBackend) Open(_ context.Context, info *ConnInfo) (net.Conn, error) {
	id := plumbing.ConnectionID(info.Source.Host, info.Source.Port, info.Dest.Host, info.Dest.Port)
	// Its first Data opens it.
	return b.newConn(id, info).stream.Conn(), nil
}

// newConn registers a conn that sends as info says.
func (b *streamBackend) newConn(id string, info *ConnInfo) *messageFrames {
	f := &messageFrames{b: b, id: id, info: info}
	f.stream = plumbing.NewMuxStream(f, plumbing.MuxConfig{
		Queue:  queuedMessages,
		Local:  addr(info.Source.String()),
		Remote: addr(info.Dest.String()),
	})
	b.conns.Store(id, f)
	return f
}

func (b *streamBackend) send(msg *bridgev1.TunnelNetworkMessage) error {
	select {
	case b.sendCh <- msg:
		return nil
	case <-b.stop:
		return net.ErrClosed
	}
}

// messageFrames writes one conn's messages: every one addresses the conn, and
// an error ends it.
type messageFrames struct {
	b  *streamBackend
	id string
	// As this side sends it.
	info   *ConnInfo
	stream *plumbing.MuxStream
}

func (f *messageFrames) Data(payload []byte) error {
	return f.b.send(&bridgev1.TunnelNetworkMessage{
		ConnectionId: f.id,
		Source:       tunnelAddress(f.info.Source),
		Dest:         tunnelAddress(f.info.Dest),
		Hostname:     f.info.Hostname,
		Data:         payload,
	})
}

// CloseWrite: the stream can't carry a half-close.
func (f *messageFrames) CloseWrite() error { return errors.ErrUnsupported }

func (f *messageFrames) Reset(reason string) error {
	return f.b.send(&bridgev1.TunnelNetworkMessage{ConnectionId: f.id, Error: reason})
}

// Window: the stream has no flow control; the queue holds the peer back.
func (f *messageFrames) Window(int) error { return nil }

// Release forgets the conn, unless another has since taken its id.
func (f *messageFrames) Release() {
	f.b.conns.Compute(f.id, func(cur *messageFrames, loaded bool) (*messageFrames, bool) {
		if !loaded {
			return nil, true
		}
		return cur, cur == f
	})
}
