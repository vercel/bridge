package devtunnel

import (
	"net"
	"sync/atomic"

	"github.com/vercel/bridge/api/go/vercel/api/dev_tunnel/v1"
	"github.com/vercel/bridge/pkg/plumbing"
	"github.com/vercel/bridge/pkg/tunnel"
)

const (
	// Per Data frame.
	maxPayload = 64 << 10
	// What a conn may have queued for the relay, not yet on the stream, before
	// its sends wait. A dev server writing faster than the stream drains is
	// held back by TCP, while the other conns keep flowing.
	maxUnsentBytes = 1 << 20
)

type direction int

const (
	// From the deployment to the app.
	ingress direction = iota
	// From the app out through the deployment.
	egress
)

// relayFrames writes one conn's frames on a Connect stream: Data, Close and
// Reset. The relay's Window frames are ignored: what the relay sends is taken
// as fast as it comes.
type relayFrames struct {
	s      *stream
	id     string
	wire   uint64
	dir    direction
	stream *plumbing.MuxStream

	unsent  atomic.Int64
	drained chan struct{}
}

// newConn opens id on the stream as the relay's conn wire, or returns nil
// once the stream has ended.
func (s *stream) newConn(id string, wire uint64, dir direction, remote tunnel.Addr) *relayFrames {
	if s.closed.Load() {
		return nil
	}
	f := &relayFrames{s: s, id: id, wire: wire, dir: dir, drained: make(chan struct{}, 1)}
	f.stream = plumbing.NewMuxStream(f, plumbing.MuxConfig{
		MaxFrame: maxPayload,
		Local:    addr("bridge"),
		Remote:   addr(remote.String()),
	})
	s.b.counters(dir, 1)
	s.b.conns.Store(id, f)
	// endConns may have swept the conns before the Store.
	if s.closed.Load() {
		f.stream.PeerReset()
		return nil
	}
	return f
}

// Data waits while the conn has maxUnsentBytes queued for the relay.
func (f *relayFrames) Data(payload []byte) error {
	for f.unsent.Load() >= maxUnsentBytes {
		select {
		case <-f.drained:
		case <-f.stream.Done():
			return net.ErrClosed
		}
	}
	n := int64(len(payload))
	f.unsent.Add(n)
	f.s.b.bytesSent.Add(n)
	f.s.sendThen(&devtunnel.ConnectRequest{Frame: &devtunnel.ConnectRequest_Data{Data: &devtunnel.Data{ConnId: f.wire, Payload: payload}}}, func() {
		f.unsent.Add(-n)
		select {
		case f.drained <- struct{}{}:
		default:
		}
	})
	return nil
}

func (f *relayFrames) CloseWrite() error {
	f.s.send(&devtunnel.ConnectRequest{Frame: &devtunnel.ConnectRequest_Close{Close: &devtunnel.Close{ConnId: f.wire}}})
	return nil
}

func (f *relayFrames) Reset(reason string) error {
	f.s.send(resetFrame(f.wire, reason))
	return nil
}

// Release forgets the conn.
func (f *relayFrames) Release() {
	f.s.b.conns.Compute(f.id, func(cur *relayFrames, loaded bool) (*relayFrames, bool) {
		if !loaded {
			return nil, true
		}
		return cur, cur == f
	})
	f.s.b.counters(f.dir, -1)
}

type addr string

func (a addr) Network() string { return "dev-tunnel" }
func (a addr) String() string  { return string(a) }
