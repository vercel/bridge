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

	"github.com/puzpuzpuz/xsync/v3"
	"github.com/vercel/bridge/api/go/vercel/api/dev_tunnel/v1"
	"github.com/vercel/bridge/pkg/tunnel"
	"google.golang.org/grpc/metadata"
)

var heartbeat = &devtunnel.ConnectRequest{Frame: &devtunnel.ConnectRequest_Heartbeat{Heartbeat: &devtunnel.Heartbeat{}}}

// call is one Connect call. The relay ends every conn on it with it.
type call struct {
	d      *Stream
	logger *slog.Logger
	// The relay numbers each call's conns from 1, so the tunnel's id for one
	// starts with this.
	prefix string
	ctx    context.Context
	cancel context.CancelCauseFunc
	out    outbox
	// The tunnel's id for each conn on the call, by its id here.
	ids *xsync.MapOf[uint64, string]

	// Set once the relay routes the tunnel's conns to this call.
	held      atomic.Bool
	lastHeard atomic.Int64
	closed    atomic.Bool
}

func (d *Stream) newCall() *call {
	ctx, cancel := context.WithCancelCause(d.ctx)
	return &call{
		d:      d,
		logger: d.logger,
		prefix: strconv.FormatUint(d.nextCall.Add(1), 10) + ".",
		ctx:    ctx,
		cancel: cancel,
		out:    outbox{wake: make(chan struct{}, 1)},
		ids:    xsync.NewMapOf[uint64, string](),
	}
}

// run holds the call until the relay ends it. moving is called if the relay
// says to reconnect elsewhere; the call keeps its conns until then.
func (c *call) run(token string, moving func()) error {
	defer c.cancel(nil)
	ctx := metadata.AppendToOutgoingContext(c.ctx,
		"authorization", "Bearer "+token,
		instanceHeader, c.d.instance,
	)
	stream, err := c.d.rpc.Connect(ctx)
	if err != nil {
		return err
	}
	defer c.d.lost(c)
	defer c.endConns()

	c.lastHeard.Store(time.Now().UnixNano())
	// The call starts with its first message.
	c.send(heartbeat)
	go func() {
		for {
			frame, ok := c.out.pop(c.ctx)
			if !ok {
				return
			}
			// A failed send ends the call, which Recv reports.
			if err := stream.Send(frame); err != nil {
				return
			}
		}
	}()
	go c.heartbeats()

	for {
		msg, err := stream.Recv()
		if err != nil {
			if cause := context.Cause(c.ctx); errors.Is(cause, errSilent) {
				return cause
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		c.lastHeard.Store(time.Now().UnixNano())
		c.handle(msg, moving)
	}
}

func (c *call) heartbeats() {
	ticker := time.NewTicker(c.d.timings.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if time.Since(time.Unix(0, c.lastHeard.Load())) > c.d.timings.silent {
				c.logger.Info("Relay went silent, reconnecting")
				c.cancel(errSilent)
				return
			}
			c.send(heartbeat)
		}
	}
}

// handle hands one of the relay's frames to the tunnel as a Message.
func (c *call) handle(msg *devtunnel.ConnectResponse, moving func()) {
	d := c.d
	switch f := msg.GetFrame().(type) {
	case *devtunnel.ConnectResponse_Heartbeat:
		if c.held.CompareAndSwap(false, true) {
			d.holding(c)
		}
	case *devtunnel.ConnectResponse_Reconnect:
		d.moving(c)
		moving()
	case *devtunnel.ConnectResponse_Open:
		c.open(f.Open)
	case *devtunnel.ConnectResponse_Data:
		if id, _, ok := c.route(f.Data.GetConnId()); ok {
			d.bytesReceived.Add(int64(len(f.Data.GetPayload())))
			d.deliver(&tunnel.Message{Kind: tunnel.MessageData, ConnID: id, Payload: f.Data.GetPayload()})
		}
	case *devtunnel.ConnectResponse_Close:
		if id, r, ok := c.route(f.Close.GetConnId()); ok {
			r.peerClosed.Store(true)
			d.deliver(&tunnel.Message{Kind: tunnel.MessageCloseWrite, ConnID: id})
			if r.closeSent.Load() {
				d.forget(id, r)
			}
		}
	case *devtunnel.ConnectResponse_Reset_:
		if id, r, ok := c.route(f.Reset_.GetConnId()); ok && d.forget(id, r) {
			c.logger.Debug("Relay reset the connection", "conn_id", id, "reason", f.Reset_.GetReason())
			d.deliver(&tunnel.Message{Kind: tunnel.MessageReset, ConnID: id, Reason: f.Reset_.GetReason()})
		}
	case *devtunnel.ConnectResponse_Window:
		if id, _, ok := c.route(f.Window.GetConnId()); ok {
			d.deliver(&tunnel.Message{Kind: tunnel.MessageWindow, ConnID: id, Bytes: int(f.Window.GetBytes())})
		}
	}
}

// open hands one of the deployment's conns to the tunnel, which connects it
// to the app.
func (c *call) open(open *devtunnel.Open) {
	wire := open.GetConnId()
	port := c.d.cfg.Port
	if open.GetProtocol() != devtunnel.Protocol_PROTOCOL_TCP || int(open.GetPort()) != port {
		c.send(resetFrame(wire, fmt.Sprintf("bridge serves only port %d", port)))
		return
	}
	id := c.prefix + strconv.FormatUint(wire, 10)
	if !c.track(id, &route{c: c, wire: wire, dir: ingress}) {
		return
	}
	c.d.deliver(&tunnel.Message{
		Kind:   tunnel.MessageOpen,
		ConnID: id,
		Conn:   &tunnel.ConnInfo{Dest: tunnel.Addr{Host: "127.0.0.1", Port: port}},
	})
}

// route returns the tunnel's id and route for the conn the relay calls wire,
// if it's open on this call.
func (c *call) route(wire uint64) (string, *route, bool) {
	id, ok := c.ids.Load(wire)
	if !ok {
		return "", nil, false
	}
	r, ok := c.d.routes.Load(id)
	if !ok || r.c != c {
		return "", nil, false
	}
	return id, r, true
}

// track routes id to this call, or reports false once the call has ended.
func (c *call) track(id string, r *route) bool {
	if c.closed.Load() {
		return false
	}
	// Counted before it's stored, so whichever forget removes it uncounts it.
	c.d.counters(r.dir, 1)
	c.d.routes.Store(id, r)
	c.ids.Store(r.wire, id)
	// endConns may have swept the routes before the Store.
	if c.closed.Load() {
		c.d.forget(id, r)
		return false
	}
	return true
}

// endConns resets every conn on the call, which the relay ended with it.
func (c *call) endConns() {
	c.closed.Store(true)
	c.d.routes.Range(func(id string, r *route) bool {
		if r.c == c && c.d.forget(id, r) {
			c.d.deliver(&tunnel.Message{Kind: tunnel.MessageReset, ConnID: id, Reason: "the tunnel's stream ended"})
		}
		return true
	})
}

func (c *call) send(frame *devtunnel.ConnectRequest) {
	c.out.push(frame)
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
