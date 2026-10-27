package tunnel

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/puzpuzpuz/xsync/v3"
	"github.com/vercel/bridge/pkg/mitm"
	"github.com/vercel/bridge/pkg/plumbing"
)

const (
	// Each direction of a conn copies through one buffer of this size.
	copyBufferSize = 32 << 10
	// Per Data message.
	maxPayload = 64 << 10
	// What a conn may have queued from a peer without flow control before the
	// tunnel waits for it to be read.
	queuedPayloads = 64
)

// Tunnel multiplexes conns over one Stream. It is used on both ends: the
// intercept and the bridge server, or the intercept and a dev tunnel's relay.
type Tunnel interface {
	// AddConn sends conn through the tunnel, to destOverride if given or else
	// where conn was headed. hostname is the DNS name the destination was
	// looked up as (may be empty).
	AddConn(conn net.Conn, destOverride string, hostname string)

	// Start reads the stream, dialing a local conn for each the peer opens.
	Start(ctx context.Context)

	// Close cancels the tunnel context, closing all connections and stopping all goroutines.
	Close() error

	// Done returns a channel that is closed when the tunnel stops.
	Done() <-chan struct{}
}

// Option configures a Tunnel.
type Option func(*tunnelImpl)

// WithHijacker sets a hijacker that can intercept outbound connections
// before the tunnel dials the real destination.
func WithHijacker(h mitm.Hijacker) Option {
	return func(t *tunnelImpl) { t.hijacker = h }
}

// New creates a Tunnel over stream. A conn the peer opens is dialed with
// dialer: the client side uses the StaticPortDialer while the server side uses
// a standard ContextDialer.
func New(dialer plumbing.ContextDialer, stream Stream, opts ...Option) Tunnel {
	t := &tunnelImpl{
		dialer:   dialer,
		stream:   stream,
		features: stream.Features(),
		conns:    xsync.NewMapOf[string, *conn](),
		done:     make(chan struct{}),
		logger:   slog.Default(),
	}
	t.bufs.New = func() any {
		buf := make([]byte, copyBufferSize)
		return &buf
	}
	t.ctx, t.cancel = context.WithCancel(context.Background())
	for _, o := range opts {
		o(t)
	}
	return t
}

type tunnelImpl struct {
	dialer    plumbing.ContextDialer
	hijacker  mitm.Hijacker
	stream    Stream
	features  Features
	conns     *xsync.MapOf[string, *conn]
	bufs      sync.Pool
	logger    *slog.Logger
	done      chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
}

func (t *tunnelImpl) Done() <-chan struct{} { return t.done }

func (t *tunnelImpl) Start(ctx context.Context) {
	stop := context.AfterFunc(ctx, t.cancel)
	recv := make(chan *Message)
	recvErr := make(chan error, 1)
	go func() {
		for {
			m, err := t.stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			select {
			case recv <- m:
			case <-t.ctx.Done():
				return
			}
		}
	}()
	go func() {
		defer stop()
		defer t.Close()
		for {
			select {
			case m := <-recv:
				t.handle(m)
			case err := <-recvErr:
				if errors.Is(err, io.EOF) {
					t.logger.Info("Tunnel: stream closed (EOF)")
				} else {
					t.logger.Info("Tunnel: stream recv error", "error", err)
				}
				return
			case <-t.ctx.Done():
				return
			}
		}
	}()
}

func (t *tunnelImpl) Close() error {
	t.closeOnce.Do(func() {
		t.cancel()
		t.conns.Range(func(_ string, c *conn) bool {
			c.stream.PeerReset()
			return true
		})
		close(t.done)
	})
	return nil
}

func (t *tunnelImpl) AddConn(local net.Conn, destOverride string, hostname string) {
	srcAddr := local.RemoteAddr().String()
	dstAddr := local.LocalAddr().String()
	if destOverride != "" {
		dstAddr = destOverride
	}
	info := &ConnInfo{Source: SplitAddr(srcAddr), Dest: SplitAddr(dstAddr), Hostname: hostname}
	id := plumbing.ConnectionID(info.Source.Host, info.Source.Port, info.Dest.Host, info.Dest.Port)
	logger := t.logger.With("conn_id", id, "src", srcAddr, "dst", dstAddr, "hostname", hostname)
	logger.Debug("Tunnel: adding connection")

	c := t.newConn(id, info)
	go func() {
		if err := t.stream.Send(&Message{Kind: MessageOpen, ConnID: id, Conn: info}); err != nil {
			logger.Info("Tunnel: open failed", "error", err)
			c.stream.PeerReset()
			_ = local.Close()
			return
		}
		t.pump(local, c)
	}()
}

// handle routes one of the peer's messages to its conn.
func (t *tunnelImpl) handle(m *Message) {
	if m.Kind == MessageOpen {
		t.open(m)
		return
	}
	c, ok := t.conns.Load(m.ConnID)
	if !ok {
		return
	}
	switch m.Kind {
	case MessageData:
		c.stream.Deliver(t.ctx, m.Payload)
	case MessageCloseWrite:
		c.stream.PeerClosed()
	case MessageReset:
		c.stream.PeerReset()
	case MessageWindow:
		c.stream.Conn().Grant(m.Bytes)
	}
}

// open registers a conn the peer opened before dialing it, so its Data queues
// on it during the dial.
func (t *tunnelImpl) open(m *Message) {
	if m.Conn == nil {
		t.logger.Info("Tunnel: ignoring open with no dest", "conn_id", m.ConnID)
		return
	}
	c := t.newConn(m.ConnID, m.Conn.Reply())
	go func() {
		local, err := t.dial(m.Conn)
		if err != nil {
			t.logger.Info("Tunnel: connect failed", "conn_id", m.ConnID, "hostname", m.Conn.Hostname, "error", err)
			_ = c.stream.Close()
			return
		}
		t.pump(local, c)
	}()
}

// dial gives the hijacker first shot at a conn the peer opened, then falls
// through to the dialer.
func (t *tunnelImpl) dial(info *ConnInfo) (net.Conn, error) {
	dest := info.Dest.String()
	if t.hijacker != nil && t.hijacker.ShouldHijack(info.Hostname) {
		local, err := t.hijacker.Hijack(t.ctx, info.Hostname, dest)
		if err == nil {
			return local, nil
		}
		t.logger.Info("Tunnel: hijack failed, falling back to dialer", "hostname", info.Hostname, "error", err)
	}
	return t.dialer.DialContext(t.ctx, "tcp", dest)
}

// pump copies between local and c in both directions, each through a buffer
// from the pool, then closes both. Either side's EOF half-closes the other; a
// failure either way closes both, which resets c.
func (t *tunnelImpl) pump(local net.Conn, c *conn) {
	remote := c.stream.Conn()
	toRemote := t.bufs.Get().(*[]byte)
	fromRemote := t.bufs.Get().(*[]byte)
	defer func() {
		t.bufs.Put(toRemote)
		t.bufs.Put(fromRemote)
	}()

	// Once the conn is over, reading remote fails and aborts too.
	abort := sync.OnceFunc(func() {
		_ = remote.Close()
		_ = local.Close()
	})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		copyHalf(remote, local, *toRemote, abort)
	}()
	go func() {
		defer wg.Done()
		copyHalf(local, remote, *fromRemote, abort)
	}()
	wg.Wait()
	abort()

	st := remote.Stats()
	t.logger.Debug("Tunnel: connection closed",
		"conn_id", c.id,
		"bytes_sent", st.BytesWritten,
		"bytes_received", st.BytesRead,
		"window_wait", st.WriteWait,
		"window_waits", st.WriteWaits,
	)
}

// copyHalf copies src to dst until src ends, then half-closes dst.
func copyHalf(dst, src net.Conn, buf []byte, abort func()) {
	// Hides ReadFrom and WriteTo, which would copy through buffers of their own.
	_, err := io.CopyBuffer(struct{ io.Writer }{dst}, struct{ io.Reader }{src}, buf)
	if err != nil {
		abort()
		return
	}
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// conn is one of the tunnel's conns, as the plumbing.Frames its MuxStream
// writes: each frame is a Message on the tunnel's Stream.
type conn struct {
	t      *tunnelImpl
	id     string
	stream *plumbing.MuxStream
}

// newConn registers a conn that sends as info addresses it.
func (t *tunnelImpl) newConn(id string, info *ConnInfo) *conn {
	c := &conn{t: t, id: id}
	queue := 0
	if t.features.Window == 0 {
		queue = queuedPayloads
	}
	c.stream = plumbing.NewMuxStream(c, plumbing.MuxConfig{
		MaxFrame: maxPayload,
		Queue:    queue,
		Window:   t.features.Window,
		Local:    addr(info.Source.String()),
		Remote:   addr(info.Dest.String()),
	})
	t.conns.Store(id, c)
	return c
}

func (c *conn) Data(payload []byte) error {
	return c.t.stream.Send(&Message{Kind: MessageData, ConnID: c.id, Payload: payload})
}

func (c *conn) CloseWrite() error {
	err := c.t.stream.Send(&Message{Kind: MessageCloseWrite, ConnID: c.id})
	if !c.t.features.HalfClose {
		return errors.ErrUnsupported
	}
	return err
}

func (c *conn) Reset(reason string) error {
	return c.t.stream.Send(&Message{Kind: MessageReset, ConnID: c.id, Reason: reason})
}

func (c *conn) Window(n int) error {
	if c.t.features.Window == 0 {
		return nil
	}
	return c.t.stream.Send(&Message{Kind: MessageWindow, ConnID: c.id, Bytes: n})
}

// Release forgets the conn, unless another has since taken its id.
func (c *conn) Release() {
	c.t.conns.Compute(c.id, func(cur *conn, loaded bool) (*conn, bool) {
		if !loaded {
			return nil, true
		}
		return cur, cur == c
	})
}

type addr string

func (a addr) Network() string { return "tunnel" }
func (a addr) String() string  { return string(a) }
