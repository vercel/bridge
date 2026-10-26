package tunnel

import (
	"bytes"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vercel/bridge/pkg/plumbing"
)

const (
	// Per Data frame.
	maxFrame = 64 << 10
	// What a conn may have queued from a peer without flow control before the
	// backend waits for it to be read.
	maxQueued = 256 << 10
)

// streamConn is one conn's side on the tunnel's transport, as a net.Conn: Read
// takes what the peer sent, Write sends to it, and CloseWrite half-closes it.
// The tunnel uses it through meter, which holds writes to the conn's window.
type streamConn struct {
	t     *tunnelImpl
	id    string
	info  *ConnInfo
	meter *plumbing.MeteredConn
	// 0 when the backend has no flow control.
	window    int
	halfClose bool

	// From the peer, not yet read.
	inMu    sync.Mutex
	in      [][]byte
	inBytes int
	inEOF   bool
	inWake  chan struct{}
	inRoom  chan struct{}
	// The rest of the chunk Read is on; only Read touches it.
	pending []byte

	closeSent atomic.Bool

	local    atomic.Pointer[net.Conn]
	done     chan struct{}
	doneOnce sync.Once
}

var _ net.Conn = (*streamConn)(nil)

func (t *tunnelImpl) newConn(id string, info *ConnInfo) *streamConn {
	c := &streamConn{
		t:         t,
		id:        id,
		info:      info,
		window:    t.backend.Window(),
		halfClose: t.backend.HalfClose(),
		inWake:    make(chan struct{}, 1),
		inRoom:    make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
	cfg := plumbing.MeteredConfig{Window: c.window}
	if c.window > 0 {
		// Gives the peer back what was read.
		cfg.OnRead = func(n int) { c.send(Frame{Kind: FrameWindow, Bytes: n}) }
	}
	c.meter = plumbing.NewMeteredConn(c, cfg)
	return c
}

func (c *streamConn) Read(p []byte) (int, error) {
	for len(c.pending) == 0 {
		if c.isDone() {
			return 0, net.ErrClosed
		}
		c.inMu.Lock()
		if len(c.in) > 0 {
			c.pending = c.in[0]
			c.in[0] = nil
			c.in = c.in[1:]
			c.inMu.Unlock()
			break
		}
		eof := c.inEOF
		c.inMu.Unlock()
		if eof {
			return 0, io.EOF
		}
		select {
		case <-c.inWake:
		case <-c.done:
		}
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	c.consumed(n)
	return n, nil
}

// consumed makes room for what a peer without flow control sends next.
func (c *streamConn) consumed(n int) {
	c.inMu.Lock()
	c.inBytes -= n
	c.inMu.Unlock()
	wake(c.inRoom)
}

func (c *streamConn) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		if c.isDone() {
			return written, net.ErrClosed
		}
		size := min(len(p)-written, maxFrame)
		// The backend may send it after Write returns.
		c.send(Frame{Kind: FrameData, Payload: bytes.Clone(p[written : written+size])})
		written += size
	}
	return written, nil
}

// CloseWrite tells the peer this side has nothing more to send. Over a
// transport without half-close, it ends the conn.
func (c *streamConn) CloseWrite() error {
	if !c.halfClose {
		c.finish()
		return nil
	}
	if !c.isDone() && c.closeSent.CompareAndSwap(false, true) {
		c.send(Frame{Kind: FrameCloseWrite})
	}
	return nil
}

// Done is closed once the conn is over.
func (c *streamConn) Done() <-chan struct{} { return c.done }

// Close resets the conn.
func (c *streamConn) Close() error {
	c.reset("closed")
	return nil
}

func (c *streamConn) LocalAddr() net.Addr              { return addr(c.info.Source.String()) }
func (c *streamConn) RemoteAddr() net.Addr             { return addr(c.info.Dest.String()) }
func (c *streamConn) SetDeadline(time.Time) error      { return nil }
func (c *streamConn) SetReadDeadline(time.Time) error  { return nil }
func (c *streamConn) SetWriteDeadline(time.Time) error { return nil }

// deliver queues payload from the peer.
func (c *streamConn) deliver(p []byte) {
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

// peerClosed: the peer has nothing more to send. Read returns io.EOF once it
// has returned the rest.
func (c *streamConn) peerClosed() {
	c.inMu.Lock()
	c.inEOF = true
	c.inMu.Unlock()
	wake(c.inWake)
}

// attach hands the conn its local end, which it closes when it's over.
// Reports false if the conn is already over.
func (c *streamConn) attach(local net.Conn) bool {
	c.local.Store(&local)
	if c.isDone() {
		_ = local.Close()
		return false
	}
	return true
}

// reset ends the conn and tells the peer why.
func (c *streamConn) reset(reason string) { c.end(reason, true) }

// finish ends the conn without telling the peer: it's done, or already knows.
func (c *streamConn) finish() { c.end("", false) }

func (c *streamConn) end(reason string, tell bool) {
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

func (c *streamConn) isDone() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *streamConn) send(f Frame) {
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
