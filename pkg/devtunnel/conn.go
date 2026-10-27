package devtunnel

import (
	"github.com/vercel/bridge/api/go/vercel/api/dev_tunnel/v1"
	"github.com/vercel/bridge/pkg/plumbing"
	"github.com/vercel/bridge/pkg/tunnel"
)

// Per Data frame.
const maxPayload = 64 << 10

type direction int

const (
	// From the deployment to the app.
	ingress direction = iota
	// From the app out through the deployment.
	egress
)

// relayFrames writes one conn's frames on a Connect stream: Data, Close,
// Reset, and a Window for each payload received.
type relayFrames struct {
	s      *stream
	id     string
	wire   uint64
	dir    direction
	stream *plumbing.MuxStream
}

// newConn opens id on the stream as the relay's conn wire, or returns nil
// once the stream has ended.
func (s *stream) newConn(id string, wire uint64, dir direction, remote tunnel.Addr) *relayFrames {
	if s.closed.Load() {
		return nil
	}
	f := &relayFrames{s: s, id: id, wire: wire, dir: dir}
	f.stream = plumbing.NewMuxStream(f, plumbing.MuxConfig{
		MaxFrame: maxPayload,
		Window:   connWindow,
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

func (f *relayFrames) Data(payload []byte) error {
	f.s.b.bytesSent.Add(int64(len(payload)))
	f.s.send(&devtunnel.ConnectRequest{Frame: &devtunnel.ConnectRequest_Data{Data: &devtunnel.Data{ConnId: f.wire, Payload: payload}}})
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

func (f *relayFrames) Window(n int) error {
	f.s.send(&devtunnel.ConnectRequest{Frame: &devtunnel.ConnectRequest_Window{Window: &devtunnel.Window{ConnId: f.wire, Bytes: uint32(n)}}})
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
