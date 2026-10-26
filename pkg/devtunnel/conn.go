package devtunnel

import (
	"bytes"
	"io"
	"net"
	"sync"
	"sync/atomic"

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

// relayStream is one conn's frames on a Connect stream, as a plumbing.Stream:
// Recv returns the relay's Data and grants it back, Send sends Data,
// CloseSend sends Close and Close sends Reset. The tunnel gets conn, which
// holds writes to the relay's window.
type relayStream struct {
	s    *stream
	id   string
	wire uint64
	dir  direction
	conn plumbing.MeteredConn

	// From the relay, not yet received. The window bounds it.
	mu   sync.Mutex
	in   [][]byte
	eof  bool
	wake chan struct{}

	// Both sides have half-closed once both are set.
	closeSent, eofRead atomic.Bool
	done               chan struct{}
	once               sync.Once
}

var _ plumbing.Stream = (*relayStream)(nil)

// newConn opens id on the stream as the relay's conn wire, or returns nil
// once the stream has ended.
func (s *stream) newConn(id string, wire uint64, dir direction, remote tunnel.Addr) *relayStream {
	if s.closed.Load() {
		return nil
	}
	rs := &relayStream{s: s, id: id, wire: wire, dir: dir, wake: make(chan struct{}, 1), done: make(chan struct{})}
	rs.conn = plumbing.NewMeteredConn(plumbing.NewStreamConn(rs, addr("bridge"), addr(remote.String())), connWindow)
	s.b.counters(dir, 1)
	s.b.conns.Store(id, rs)
	// endConns may have swept the conns before the Store.
	if s.closed.Load() {
		rs.end()
		return nil
	}
	return rs
}

func (rs *relayStream) Recv() ([]byte, error) {
	for {
		if rs.isDone() {
			return nil, net.ErrClosed
		}
		rs.mu.Lock()
		if len(rs.in) > 0 {
			chunk := rs.in[0]
			rs.in[0] = nil
			rs.in = rs.in[1:]
			rs.mu.Unlock()
			rs.s.send(&devtunnel.ConnectRequest{Frame: &devtunnel.ConnectRequest_Window{Window: &devtunnel.Window{
				ConnId: rs.wire,
				Bytes:  uint32(len(chunk)),
			}}})
			return chunk, nil
		}
		eof := rs.eof
		rs.mu.Unlock()
		if eof {
			rs.eofRead.Store(true)
			if rs.closeSent.Load() {
				rs.end()
			}
			return nil, io.EOF
		}
		select {
		case <-rs.wake:
		case <-rs.done:
		}
	}
}

func (rs *relayStream) Send(p []byte) error {
	for len(p) > 0 {
		if rs.isDone() {
			return net.ErrClosed
		}
		size := min(len(p), maxPayload)
		rs.s.b.bytesSent.Add(int64(size))
		// Queued, so it can't share p.
		rs.s.send(&devtunnel.ConnectRequest{Frame: &devtunnel.ConnectRequest_Data{Data: &devtunnel.Data{
			ConnId:  rs.wire,
			Payload: bytes.Clone(p[:size]),
		}}})
		p = p[size:]
	}
	return nil
}

func (rs *relayStream) CloseSend() error {
	if rs.isDone() || !rs.closeSent.CompareAndSwap(false, true) {
		return nil
	}
	rs.s.send(&devtunnel.ConnectRequest{Frame: &devtunnel.ConnectRequest_Close{Close: &devtunnel.Close{ConnId: rs.wire}}})
	if rs.eofRead.Load() {
		rs.end()
	}
	return nil
}

// Close resets the conn.
func (rs *relayStream) Close() error {
	if rs.isDone() {
		return nil
	}
	rs.s.send(resetFrame(rs.wire, "connection closed"))
	rs.end()
	return nil
}

func (rs *relayStream) Done() <-chan struct{} { return rs.done }

// push queues payload from the relay.
func (rs *relayStream) push(payload []byte) {
	if len(payload) == 0 {
		return
	}
	rs.mu.Lock()
	if !rs.eof && !rs.isDone() {
		rs.in = append(rs.in, payload)
	}
	rs.mu.Unlock()
	rs.signal()
}

// peerClosed: the relay's side has nothing more to send.
func (rs *relayStream) peerClosed() {
	rs.mu.Lock()
	rs.eof = true
	rs.mu.Unlock()
	rs.signal()
}

// end forgets the conn, without telling the relay.
func (rs *relayStream) end() {
	rs.once.Do(func() {
		close(rs.done)
		rs.s.b.conns.Compute(rs.id, func(cur *relayStream, loaded bool) (*relayStream, bool) {
			if !loaded {
				return nil, true
			}
			return cur, cur == rs
		})
		rs.s.b.counters(rs.dir, -1)
	})
}

func (rs *relayStream) signal() {
	select {
	case rs.wake <- struct{}{}:
	default:
	}
}

func (rs *relayStream) isDone() bool {
	select {
	case <-rs.done:
		return true
	default:
		return false
	}
}

type addr string

func (a addr) Network() string { return "dev-tunnel" }
func (a addr) String() string  { return string(a) }
