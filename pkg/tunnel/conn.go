package tunnel

import (
	"bytes"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/vercel/bridge/pkg/plumbing"
)

const (
	// Per Data frame.
	maxFrame = 64 << 10
	// What a conn may have queued from a peer without flow control before the
	// backend waits for it to be read.
	maxQueued = 256 << 10
)

// connStream is one conn's frames on the tunnel's transport, as a
// plumbing.Stream. The tunnel uses it as conn: a net.Conn over it, metered
// to the conn's window.
type connStream struct {
	t    *tunnelImpl
	id   string
	info *ConnInfo
	conn plumbing.MeteredConn
	// 0 when the backend has no flow control.
	window    int
	halfClose bool

	// From the peer, not yet received.
	inMu    sync.Mutex
	in      [][]byte
	inBytes int
	inEOF   bool
	inWake  chan struct{}
	inRoom  chan struct{}

	closeSent atomic.Bool
	local     atomic.Pointer[net.Conn]
	done      chan struct{}
	doneOnce  sync.Once
}

var _ plumbing.Stream = (*connStream)(nil)

func (t *tunnelImpl) newConn(id string, info *ConnInfo) *connStream {
	c := &connStream{
		t:         t,
		id:        id,
		info:      info,
		window:    t.backend.Window(),
		halfClose: t.backend.HalfClose(),
		inWake:    make(chan struct{}, 1),
		inRoom:    make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
	c.conn = plumbing.NewMeteredConn(
		plumbing.NewStreamConn(c, addr(info.Source.String()), addr(info.Dest.String())),
		c.window,
	)
	return c
}

func (c *connStream) Recv() ([]byte, error) {
	for {
		if c.isDone() {
			return nil, net.ErrClosed
		}
		c.inMu.Lock()
		if len(c.in) > 0 {
			chunk := c.in[0]
			c.in[0] = nil
			c.in = c.in[1:]
			c.inMu.Unlock()
			c.consumed(len(chunk))
			return chunk, nil
		}
		eof := c.inEOF
		c.inMu.Unlock()
		if eof {
			return nil, io.EOF
		}
		select {
		case <-c.inWake:
		case <-c.done:
		}
	}
}

// consumed gives back what Recv took: as window to a peer with flow control,
// and as room in the queue to one without.
func (c *connStream) consumed(n int) {
	if c.window > 0 {
		c.send(Frame{Kind: FrameWindow, Bytes: n})
		return
	}
	c.inMu.Lock()
	c.inBytes -= n
	c.inMu.Unlock()
	wake(c.inRoom)
}

func (c *connStream) Send(p []byte) error {
	for len(p) > 0 {
		if c.isDone() {
			return net.ErrClosed
		}
		size := min(len(p), maxFrame)
		// The backend may send it after Send returns.
		c.send(Frame{Kind: FrameData, Payload: bytes.Clone(p[:size])})
		p = p[size:]
	}
	return nil
}

// CloseSend tells the peer this side has nothing more to send. Over a
// transport without half-close, it ends the conn.
func (c *connStream) CloseSend() error {
	if !c.halfClose {
		c.finish()
		return nil
	}
	if !c.isDone() && c.closeSent.CompareAndSwap(false, true) {
		c.send(Frame{Kind: FrameCloseWrite})
	}
	return nil
}

// Close resets the conn.
func (c *connStream) Close() error {
	c.reset("closed")
	return nil
}

// Done is closed once the conn is over.
func (c *connStream) Done() <-chan struct{} { return c.done }

// deliver queues payload from the peer.
func (c *connStream) deliver(p []byte) {
	if len(p) == 0 {
		return
	}
	for {
		c.inMu.Lock()
		if c.inEOF || c.isDone() {
			c.inMu.Unlock()
			return
		}
		// A window already bounds what the peer can send.
		if c.window > 0 || c.inBytes < maxQueued {
			c.in = append(c.in, p)
			c.inBytes += len(p)
			c.inMu.Unlock()
			wake(c.inWake)
			return
		}
		c.inMu.Unlock()
		select {
		case <-c.inRoom:
		case <-c.done:
			return
		}
	}
}

// peerClosed: the peer has nothing more to send. Recv returns io.EOF once it
// has returned the rest.
func (c *connStream) peerClosed() {
	c.inMu.Lock()
	c.inEOF = true
	c.inMu.Unlock()
	wake(c.inWake)
}

// attach hands the conn its local end, which it closes when it's over.
// Reports false if the conn is already over.
func (c *connStream) attach(local net.Conn) bool {
	c.local.Store(&local)
	if c.isDone() {
		_ = local.Close()
		return false
	}
	return true
}

// reset ends the conn and tells the peer why.
func (c *connStream) reset(reason string) { c.end(reason, true) }

// finish ends the conn without telling the peer: it's done, or already knows.
func (c *connStream) finish() { c.end("", false) }

func (c *connStream) end(reason string, tell bool) {
	c.doneOnce.Do(func() {
		if tell {
			c.send(Frame{Kind: FrameReset, Reason: reason})
		}
		close(c.done)
		if local := c.local.Load(); local != nil {
			_ = (*local).Close()
		}
		c.t.forget(c)
		c.t.backend.Release(c.id)
	})
}

func (c *connStream) isDone() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *connStream) send(f Frame) {
	f.ConnID = c.id
	f.Conn = c.info
	c.t.backend.Send(f)
}

// wake signals ch without blocking; ch holds one signal.
func wake(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

type addr string

func (a addr) Network() string { return "tunnel" }
func (a addr) String() string  { return string(a) }
