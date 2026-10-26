package devtunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vercel/bridge/api/go/vercel/api/dev_tunnel/v1"
	"github.com/vercel/bridge/pkg/tunnel"
	"google.golang.org/grpc/metadata"
)

var heartbeat = &devtunnel.ConnectRequest{Frame: &devtunnel.ConnectRequest_Heartbeat{Heartbeat: &devtunnel.Heartbeat{}}}

// stream is one Connect call. The relay ends every conn on it with it.
type stream struct {
	b      *Backend
	logger *slog.Logger
	// The relay numbers each stream's conns from 1, so the tunnel's id for one
	// starts with this.
	prefix string
	ctx    context.Context
	cancel context.CancelCauseFunc
	out    outbox

	// Set once the relay routes the tunnel's conns to this stream.
	held      atomic.Bool
	lastHeard atomic.Int64
	closed    atomic.Bool
}

func (b *Backend) newStream() *stream {
	ctx, cancel := context.WithCancelCause(b.ctx)
	return &stream{
		b:      b,
		logger: b.logger,
		prefix: strconv.FormatUint(b.nextStream.Add(1), 10) + ".",
		ctx:    ctx,
		cancel: cancel,
		out:    outbox{wake: make(chan struct{}, 1)},
	}
}

// run holds the stream until the relay ends it. moving is called if the
// relay says to reconnect elsewhere; the stream keeps its conns until then.
func (s *stream) run(token string, moving func()) error {
	defer s.cancel(nil)
	ctx := metadata.AppendToOutgoingContext(s.ctx,
		"authorization", "Bearer "+token,
		instanceHeader, s.b.instance,
	)
	call, err := s.b.rpc.Connect(ctx)
	if err != nil {
		return err
	}
	defer s.b.lost(s)
	defer s.endConns()

	s.lastHeard.Store(time.Now().UnixNano())
	// The call starts with its first message.
	s.send(heartbeat)
	go func() {
		for {
			frame, ok := s.out.pop(s.ctx)
			if !ok {
				return
			}
			// A failed send ends the call, which Recv reports.
			if err := call.Send(frame); err != nil {
				return
			}
		}
	}()
	go s.heartbeats()

	for {
		msg, err := call.Recv()
		if err != nil {
			if cause := context.Cause(s.ctx); errors.Is(cause, errSilent) {
				return cause
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		s.lastHeard.Store(time.Now().UnixNano())
		s.handle(msg, moving)
	}
}

func (s *stream) heartbeats() {
	ticker := time.NewTicker(s.b.timings.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if time.Since(time.Unix(0, s.lastHeard.Load())) > s.b.timings.silent {
				s.logger.Info("Relay went silent, reconnecting")
				s.cancel(errSilent)
				return
			}
			s.send(heartbeat)
		}
	}
}

// handle passes one of the relay's frames to the tunnel.
func (s *stream) handle(msg *devtunnel.ConnectResponse, moving func()) {
	switch f := msg.GetFrame().(type) {
	case *devtunnel.ConnectResponse_Heartbeat:
		if s.held.CompareAndSwap(false, true) {
			s.b.holding(s)
		}
	case *devtunnel.ConnectResponse_Reconnect:
		s.b.moving(s)
		moving()
	case *devtunnel.ConnectResponse_Open:
		s.open(f.Open)
	case *devtunnel.ConnectResponse_Data:
		if rs := s.conn(f.Data.GetConnId()); rs != nil {
			s.b.bytesReceived.Add(int64(len(f.Data.GetPayload())))
			rs.push(f.Data.GetPayload())
		}
	case *devtunnel.ConnectResponse_Close:
		if rs := s.conn(f.Close.GetConnId()); rs != nil {
			rs.peerClosed()
		}
	case *devtunnel.ConnectResponse_Reset_:
		if rs := s.conn(f.Reset_.GetConnId()); rs != nil {
			s.logger.Debug("Relay reset the connection", "conn_id", rs.id, "reason", f.Reset_.GetReason())
			rs.end()
		}
	case *devtunnel.ConnectResponse_Window:
		if rs := s.conn(f.Window.GetConnId()); rs != nil {
			rs.conn.Grant(int(f.Window.GetBytes()))
		}
	}
}

// open hands one of the deployment's conns to the tunnel, which connects it
// to the app.
func (s *stream) open(open *devtunnel.Open) {
	wire := open.GetConnId()
	port := s.b.cfg.Port
	if open.GetProtocol() != devtunnel.Protocol_PROTOCOL_TCP || int(open.GetPort()) != port {
		s.send(resetFrame(wire, fmt.Sprintf("bridge serves only port %d", port)))
		return
	}
	dest := tunnel.Addr{Host: "127.0.0.1", Port: port}
	if rs := s.newConn(s.idFor(wire), wire, ingress, dest); rs != nil {
		s.b.accept(rs.conn, &tunnel.ConnInfo{Dest: dest})
	}
}

// idFor is the backend's id for the conn the relay calls wire on this stream.
// The relay numbers each stream's conns from 1; outbound conns are numbered
// across streams.
func (s *stream) idFor(wire uint64) string {
	if wire >= clientConnIDFloor {
		return "e" + strconv.FormatUint(wire&^clientConnIDFloor, 10)
	}
	return s.prefix + strconv.FormatUint(wire, 10)
}

// conn returns the conn the relay calls wire, if it's open on this stream.
func (s *stream) conn(wire uint64) *relayStream {
	rs, ok := s.b.conns.Load(s.idFor(wire))
	if !ok || rs.s != s {
		return nil
	}
	return rs
}

// endConns ends every conn on the stream, which the relay ended with it.
func (s *stream) endConns() {
	s.closed.Store(true)
	s.b.conns.Range(func(_ string, rs *relayStream) bool {
		if rs.s == s {
			rs.end()
		}
		return true
	})
}

func (s *stream) send(frame *devtunnel.ConnectRequest) {
	s.out.push(frame)
}

// outbox queues frames for the relay in order. Data is bounded by each conn's
// window; the rest is small.
type outbox struct {
	mu     sync.Mutex
	frames []*devtunnel.ConnectRequest
	wake   chan struct{}
}

func (o *outbox) push(frame *devtunnel.ConnectRequest) {
	o.mu.Lock()
	o.frames = append(o.frames, frame)
	o.mu.Unlock()
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

func (o *outbox) pop(ctx context.Context) (*devtunnel.ConnectRequest, bool) {
	for {
		o.mu.Lock()
		if len(o.frames) > 0 {
			frame := o.frames[0]
			o.frames[0] = nil
			o.frames = o.frames[1:]
			o.mu.Unlock()
			return frame, true
		}
		o.mu.Unlock()
		select {
		case <-o.wake:
		case <-ctx.Done():
			return nil, false
		}
	}
}

func resetFrame(id uint64, reason string) *devtunnel.ConnectRequest {
	return &devtunnel.ConnectRequest{Frame: &devtunnel.ConnectRequest_Reset_{Reset_: &devtunnel.Reset{ConnId: id, Reason: reason}}}
}
