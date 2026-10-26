package devtunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	devtunnelv1 "github.com/vercel/bridge/api/go/vercel/api/dev_tunnel/v1"
	"google.golang.org/grpc/metadata"
)

var heartbeat = &devtunnelv1.ConnectRequest{Frame: &devtunnelv1.ConnectRequest_Heartbeat{Heartbeat: &devtunnelv1.Heartbeat{}}}

// stream is one Connect call and the conns it carries. The relay ends every
// conn with the stream.
type stream struct {
	c      *Client
	logger *slog.Logger
	ctx    context.Context
	cancel context.CancelCauseFunc
	out    outbox

	// Set once the relay routes the tunnel's conns to this stream.
	held      atomic.Bool
	lastHeard atomic.Int64

	mu     sync.Mutex
	conns  map[uint64]*conn
	closed bool
}

func (c *Client) newStream() *stream {
	ctx, cancel := context.WithCancelCause(c.ctx)
	return &stream{
		c:      c,
		logger: c.logger,
		ctx:    ctx,
		cancel: cancel,
		out:    outbox{wake: make(chan struct{}, 1)},
		conns:  map[uint64]*conn{},
	}
}

// run holds the stream until the relay ends it. moving is called if the
// relay says to reconnect elsewhere; the stream keeps its conns until then.
func (s *stream) run(token string, moving func()) error {
	defer s.cancel(nil)
	ctx := metadata.AppendToOutgoingContext(s.ctx,
		"authorization", "Bearer "+token,
		instanceHeader, s.c.instance,
	)
	call, err := s.c.rpc.Connect(ctx)
	if err != nil {
		return err
	}
	defer s.c.lost(s)
	defer s.abandonAll()

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
	ticker := time.NewTicker(s.c.timings.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if time.Since(time.Unix(0, s.lastHeard.Load())) > s.c.timings.silent {
				s.logger.Info("Relay went silent, reconnecting")
				s.cancel(errSilent)
				return
			}
			s.send(heartbeat)
		}
	}
}

func (s *stream) handle(msg *devtunnelv1.ConnectResponse, moving func()) {
	switch f := msg.GetFrame().(type) {
	case *devtunnelv1.ConnectResponse_Heartbeat:
		if s.held.CompareAndSwap(false, true) {
			s.c.holding(s)
		}
	case *devtunnelv1.ConnectResponse_Reconnect:
		s.c.moving(s)
		moving()
	case *devtunnelv1.ConnectResponse_Open:
		s.openIngress(f.Open)
	case *devtunnelv1.ConnectResponse_Data:
		if cn := s.conn(f.Data.GetConnId()); cn != nil {
			s.c.bytesReceived.Add(int64(len(f.Data.GetPayload())))
			cn.received(f.Data.GetPayload())
		}
	case *devtunnelv1.ConnectResponse_Close:
		if cn := s.conn(f.Close.GetConnId()); cn != nil {
			cn.closeReceived()
		}
	case *devtunnelv1.ConnectResponse_Reset_:
		if cn := s.conn(f.Reset_.GetConnId()); cn != nil {
			cn.logger.Debug("Relay reset the connection", "reason", f.Reset_.GetReason())
			cn.abandon()
		}
	case *devtunnelv1.ConnectResponse_Window:
		if cn := s.conn(f.Window.GetConnId()); cn != nil {
			cn.grant(int(f.Window.GetBytes()))
		}
	}
}

// openIngress connects one of the deployment's conns to the app.
func (s *stream) openIngress(open *devtunnelv1.Open) {
	id := open.GetConnId()
	port := s.c.cfg.Port
	if open.GetProtocol() != devtunnelv1.Protocol_PROTOCOL_TCP || int(open.GetPort()) != port {
		s.send(resetFrame(id, fmt.Sprintf("bridge serves only port %d", port)))
		return
	}
	cn := s.newConn(id, ingress)
	if cn == nil {
		return
	}
	go func() {
		nc, err := s.c.cfg.AppDialer.DialContext(s.ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			cn.logger.Debug("App refused the connection", "error", err)
			cn.reset(err.Error())
			return
		}
		cn.attach(nc)
	}()
}

// newConn registers a conn, or returns nil once the stream has ended.
func (s *stream) newConn(id uint64, dir direction) *conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	cn := &conn{
		id:         id,
		s:          s,
		dir:        dir,
		logger:     s.logger.With("conn_id", id, "direction", dir.String()),
		sendWindow: connWindow,
	}
	cn.cond = sync.NewCond(&cn.mu)
	s.conns[id] = cn
	s.c.counters(dir, 1)
	return cn
}

func (s *stream) conn(id uint64) *conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns[id]
}

func (s *stream) forget(cn *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns[cn.id] == cn {
		delete(s.conns, cn.id)
		s.c.counters(cn.dir, -1)
	}
}

func (s *stream) abandonAll() {
	s.mu.Lock()
	s.closed = true
	conns := make([]*conn, 0, len(s.conns))
	for _, cn := range s.conns {
		conns = append(conns, cn)
	}
	s.mu.Unlock()
	for _, cn := range conns {
		cn.abandon()
	}
}

func (s *stream) send(frame *devtunnelv1.ConnectRequest) {
	s.out.push(frame)
}

// counters tracks conns opening (delta 1) and ending (delta -1).
func (c *Client) counters(dir direction, delta int64) {
	open, total := &c.ingressConns, &c.ingressTotal
	if dir == egress {
		open, total = &c.egressConns, &c.egressTotal
	}
	open.Add(delta)
	if delta > 0 {
		total.Add(delta)
	}
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
