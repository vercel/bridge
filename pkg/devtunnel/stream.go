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

	devtunnelv1 "github.com/vercel/bridge/api/go/vercel/api/dev_tunnel/v1"
	"github.com/vercel/bridge/pkg/tunnel"
	"google.golang.org/grpc/metadata"
)

var heartbeat = &devtunnelv1.ConnectRequest{Frame: &devtunnelv1.ConnectRequest_Heartbeat{Heartbeat: &devtunnelv1.Heartbeat{}}}

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
func (s *stream) handle(msg *devtunnelv1.ConnectResponse, moving func()) {
	switch f := msg.GetFrame().(type) {
	case *devtunnelv1.ConnectResponse_Heartbeat:
		if s.held.CompareAndSwap(false, true) {
			s.b.holding(s)
		}
	case *devtunnelv1.ConnectResponse_Reconnect:
		s.b.moving(s)
		moving()
	case *devtunnelv1.ConnectResponse_Open:
		s.open(f.Open)
	case *devtunnelv1.ConnectResponse_Data:
		if id, ok := s.routed(f.Data.GetConnId()); ok {
			s.b.bytesReceived.Add(int64(len(f.Data.GetPayload())))
			s.b.deliver(tunnel.Frame{Kind: tunnel.FrameData, ConnID: id, Payload: f.Data.GetPayload()})
		}
	case *devtunnelv1.ConnectResponse_Close:
		if id, ok := s.routed(f.Close.GetConnId()); ok {
			s.b.deliver(tunnel.Frame{Kind: tunnel.FrameCloseWrite, ConnID: id})
		}
	case *devtunnelv1.ConnectResponse_Reset_:
		if id, ok := s.routed(f.Reset_.GetConnId()); ok && s.b.untrack(id, s) {
			s.logger.Debug("Relay reset the connection", "conn_id", id, "reason", f.Reset_.GetReason())
			s.b.deliver(tunnel.Frame{Kind: tunnel.FrameReset, ConnID: id, Reason: f.Reset_.GetReason()})
		}
	case *devtunnelv1.ConnectResponse_Window:
		if id, ok := s.routed(f.Window.GetConnId()); ok {
			s.b.deliver(tunnel.Frame{Kind: tunnel.FrameWindow, ConnID: id, Bytes: int(f.Window.GetBytes())})
		}
	}
}

// open hands one of the deployment's conns to the tunnel, which connects it
// to the app.
func (s *stream) open(open *devtunnelv1.Open) {
	wire := open.GetConnId()
	port := s.b.cfg.Port
	if open.GetProtocol() != devtunnelv1.Protocol_PROTOCOL_TCP || int(open.GetPort()) != port {
		s.send(resetFrame(wire, fmt.Sprintf("bridge serves only port %d", port)))
		return
	}
	id := s.idFor(wire)
	if !s.track(id, route{s: s, wire: wire, dir: ingress}) {
		return
	}
	s.b.deliver(tunnel.Frame{
		Kind:   tunnel.FrameOpen,
		ConnID: id,
		Conn:   &tunnel.ConnInfo{Dest: tunnel.Addr{Host: "127.0.0.1", Port: port}},
	})
}

// idFor is the tunnel's id for the conn the relay calls wire on this stream.
// Outbound conns keep the id the tunnel gave them.
func (s *stream) idFor(wire uint64) string {
	if wire >= clientConnIDFloor {
		return "e" + strconv.FormatUint(wire&^clientConnIDFloor, 10)
	}
	return s.prefix + strconv.FormatUint(wire, 10)
}

// routed returns the tunnel's id for wire, if it's a conn open on this stream.
func (s *stream) routed(wire uint64) (string, bool) {
	id := s.idFor(wire)
	r, ok := s.b.routes.Load(id)
	return id, ok && r.s == s
}

// track routes id to this stream, or reports false once the stream has ended.
func (s *stream) track(id string, r route) bool {
	if s.closed.Load() {
		return false
	}
	// Counted before it's stored, so whichever untrack removes it uncounts it.
	s.b.counters(r.dir, 1)
	s.b.routes.Store(id, r)
	// endConns may have swept the routes before the Store.
	if s.closed.Load() {
		s.b.untrack(id, s)
		return false
	}
	return true
}

// endConns resets every conn on the stream, which the relay ended with it.
func (s *stream) endConns() {
	s.closed.Store(true)
	s.b.routes.Range(func(id string, r route) bool {
		if r.s == s && s.b.untrack(id, s) {
			s.b.deliver(tunnel.Frame{Kind: tunnel.FrameReset, ConnID: id, Reason: "the tunnel's stream ended"})
		}
		return true
	})
}

func (s *stream) send(frame *devtunnelv1.ConnectRequest) {
	s.out.push(frame)
}

// outbox queues frames for the relay in order. Data is bounded by each conn's
// window; the rest is small.
type outbox struct {
	mu     sync.Mutex
	frames []*devtunnelv1.ConnectRequest
	wake   chan struct{}
}

func (o *outbox) push(frame *devtunnelv1.ConnectRequest) {
	o.mu.Lock()
	o.frames = append(o.frames, frame)
	o.mu.Unlock()
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

func (o *outbox) pop(ctx context.Context) (*devtunnelv1.ConnectRequest, bool) {
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

func resetFrame(id uint64, reason string) *devtunnelv1.ConnectRequest {
	return &devtunnelv1.ConnectRequest{Frame: &devtunnelv1.ConnectRequest_Reset_{Reset_: &devtunnelv1.Reset{ConnId: id, Reason: reason}}}
}
