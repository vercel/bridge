package tunnel

import (
	"context"
	"io"
	"log/slog"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/plumbing"
)

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
	}
}

type streamBackend struct {
	stream Stream
	sendCh chan *bridgev1.TunnelNetworkMessage
	// Closed once Run returns, so a Send waiting for room gives up.
	stop chan struct{}
}

func (b *streamBackend) Run(ctx context.Context, deliver func(Frame)) error {
	defer close(b.stop)

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
			deliver(frameFor(msg))
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

func frameFor(msg *bridgev1.TunnelNetworkMessage) Frame {
	if msg.GetError() != "" {
		return Frame{Kind: FrameReset, ConnID: msg.GetConnectionId(), Reason: msg.GetError()}
	}
	f := Frame{Kind: FrameData, ConnID: msg.GetConnectionId(), Payload: msg.GetData()}
	if dest := msg.GetDest(); dest != nil {
		f.Conn = &ConnInfo{
			Source:   Addr{Host: msg.GetSource().GetIp(), Port: int(msg.GetSource().GetPort())},
			Dest:     Addr{Host: dest.GetIp(), Port: int(dest.GetPort())},
			Hostname: msg.GetHostname(),
		}
	}
	return f
}

func (b *streamBackend) ConnID(conn *ConnInfo) string {
	return plumbing.ConnectionID(conn.Source.Host, conn.Source.Port, conn.Dest.Host, conn.Dest.Port)
}

// Open sends nothing: the conn's first Data opens it.
func (b *streamBackend) Open(context.Context, string, *ConnInfo) error { return nil }

func (b *streamBackend) Send(f Frame) {
	msg := &bridgev1.TunnelNetworkMessage{ConnectionId: f.ConnID}
	switch f.Kind {
	case FrameData:
		msg.Data = f.Payload
		if f.Conn != nil {
			msg.Source = tunnelAddress(f.Conn.Source)
			msg.Dest = tunnelAddress(f.Conn.Dest)
			msg.Hostname = f.Conn.Hostname
		}
	case FrameReset:
		msg.Error = f.Reason
	default:
		return
	}
	select {
	case b.sendCh <- msg:
	case <-b.stop:
	}
}

func (b *streamBackend) Release(string)  {}
func (b *streamBackend) Window() int     { return 0 }
func (b *streamBackend) HalfClose() bool { return false }
