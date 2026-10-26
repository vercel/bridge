package tunnel

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"

	"github.com/puzpuzpuz/xsync/v3"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/mitm"
	"github.com/vercel/bridge/pkg/plumbing"
)

// Each direction of a conn copies through one buffer of this size.
const copyBufferSize = 32 << 10

// Tunnel multiplexes conns over one transport, its Backend. It is used on both
// ends: the intercept and the bridge server, or the intercept and a dev
// tunnel's relay.
type Tunnel interface {
	// AddConn sends conn through the tunnel, to destOverride if given or else
	// where conn was headed. hostname is the DNS name the destination was
	// looked up as (may be empty).
	AddConn(conn net.Conn, destOverride string, hostname string)

	// Start runs the backend, opening a local conn for each the peer opens.
	Start(ctx context.Context)

	// Close cancels the tunnel context, closing all connections and stopping all goroutines.
	Close() error

	// Done returns a channel that is closed when the backend stops.
	Done() <-chan struct{}
}

// Option configures a Tunnel.
type Option func(*tunnelImpl)

// WithHijacker sets a hijacker that can intercept outbound connections
// before the tunnel dials the real destination.
func WithHijacker(h mitm.Hijacker) Option {
	return func(t *tunnelImpl) { t.hijacker = h }
}

// New creates a Tunnel over a bridge TunnelNetwork stream. A conn the peer
// opens is dialed with dialer: the client side uses the StaticPortDialer while
// the server side uses a standard ContextDialer.
func New(dialer plumbing.ContextDialer, stream Stream, opts ...Option) Tunnel {
	return NewWithBackend(dialer, NewStreamBackend(stream), opts...)
}

// NewWithBackend creates a Tunnel over backend, dialing the conns the peer
// opens with dialer.
func NewWithBackend(dialer plumbing.ContextDialer, backend Backend, opts ...Option) Tunnel {
	t := &tunnelImpl{
		dialer:  dialer,
		backend: backend,
		conns:   xsync.NewMapOf[string, *streamConn](),
		done:    make(chan struct{}),
		logger:  slog.Default(),
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
	backend   Backend
	conns     *xsync.MapOf[string, *streamConn]
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
	go func() {
		defer stop()
		defer t.Close()
		if err := t.backend.Run(t.ctx, t.deliver); err != nil {
			t.logger.Info("Tunnel: backend stopped", "error", err)
		}
	}()
}

func (t *tunnelImpl) Close() error {
	t.closeOnce.Do(func() {
		t.cancel()
		t.conns.Range(func(_ string, c *streamConn) bool {
			c.finish()
			return true
		})
		close(t.done)
	})
	return nil
}

func (t *tunnelImpl) AddConn(conn net.Conn, destOverride string, hostname string) {
	srcAddr := conn.RemoteAddr().String()
	dstAddr := conn.LocalAddr().String()
	if destOverride != "" {
		dstAddr = destOverride
	}
	info := &ConnInfo{Source: splitAddr(srcAddr), Dest: splitAddr(dstAddr), Hostname: hostname}
	id := t.backend.ConnID(info)
	logger := t.logger.With("conn_id", id, "src", srcAddr, "dst", dstAddr, "hostname", hostname)
	logger.Debug("Tunnel: adding connection")

	c := t.newConn(id, info)
	t.conns.Store(id, c)
	go func() {
		if err := t.backend.Open(t.ctx, id, info); err != nil {
			logger.Info("Tunnel: open failed", "error", err)
			c.finish()
			_ = conn.Close()
			return
		}
		t.pump(conn, c)
	}()
}

// deliver routes one of the peer's frames to its conn.
func (t *tunnelImpl) deliver(f Frame) {
	c, ok := t.conns.Load(f.ConnID)
	switch f.Kind {
	case FrameOpen:
		t.open(f)
	case FrameData:
		if ok {
			c.deliver(f.Payload)
		} else {
			t.open(f)
		}
	case FrameCloseWrite:
		if ok {
			c.peerClosed()
		}
	case FrameReset:
		if ok {
			c.finish()
		}
	case FrameWindow:
		if ok {
			c.grant(f.Bytes)
		}
	}
}

// open registers a conn the peer opened before dialing it, so the frames that
// arrive during the dial queue on it instead of each opening another.
func (t *tunnelImpl) open(f Frame) {
	if f.Conn == nil {
		t.logger.Info("Tunnel: ignoring message with no dest", "conn_id", f.ConnID)
		return
	}
	// This side sends from where the peer's conn was headed, back to it.
	reply := &ConnInfo{Source: f.Conn.Dest, Dest: f.Conn.Source, Hostname: f.Conn.Hostname}
	c, loaded := t.conns.LoadOrStore(f.ConnID, t.newConn(f.ConnID, reply))
	c.deliver(f.Payload)
	if loaded {
		return
	}
	go func() {
		local, err := t.dial(f)
		if err != nil {
			t.logger.Info("Tunnel: connect failed", "conn_id", f.ConnID, "hostname", f.Conn.Hostname, "error", err)
			c.reset(err.Error())
			return
		}
		t.pump(local, c)
	}()
}

// dial gives the hijacker first shot at a conn the peer opened, then falls
// through to the dialer.
func (t *tunnelImpl) dial(f Frame) (net.Conn, error) {
	if t.hijacker != nil {
		msg := &bridgev1.TunnelNetworkMessage{
			ConnectionId: f.ConnID,
			Source:       tunnelAddress(f.Conn.Source),
			Dest:         tunnelAddress(f.Conn.Dest),
			Hostname:     f.Conn.Hostname,
		}
		if t.hijacker.ShouldHijack(msg) {
			conn, err := t.hijacker.Hijack(t.ctx, msg)
			if err == nil {
				return conn, nil
			}
			t.logger.Info("Tunnel: hijack failed, falling back to dialer", "conn_id", f.ConnID, "hostname", f.Conn.Hostname, "error", err)
		}
	}
	return t.dialer.DialContext(t.ctx, "tcp", f.Conn.Dest.String())
}

// pump copies between local and c in both directions, each through a buffer
// from the pool, until the conn is over.
func (t *tunnelImpl) pump(local net.Conn, c *streamConn) {
	if !c.attach(local) {
		return
	}
	toPeer := t.bufs.Get().(*[]byte)
	fromPeer := t.bufs.Get().(*[]byte)
	defer func() {
		t.bufs.Put(toPeer)
		t.bufs.Put(fromPeer)
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		copyHalf(c, local, *toPeer, c)
	}()
	go func() {
		defer wg.Done()
		copyHalf(local, c, *fromPeer, c)
	}()
	wg.Wait()
	c.finish()
}

// copyHalf copies src to dst until src ends, then half-closes dst. A failure
// either way abandons the conn.
func copyHalf(dst net.Conn, src net.Conn, buf []byte, c *streamConn) {
	// Hides ReadFrom and WriteTo, which would copy through buffers of their own.
	_, err := io.CopyBuffer(struct{ io.Writer }{dst}, struct{ io.Reader }{src}, buf)
	if err != nil {
		c.reset(err.Error())
		return
	}
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// forget removes c, unless another conn has since taken its id.
func (t *tunnelImpl) forget(c *streamConn) {
	t.conns.Compute(c.id, func(cur *streamConn, loaded bool) (*streamConn, bool) {
		if !loaded {
			return nil, true
		}
		return cur, cur == c
	})
}

func splitAddr(s string) Addr {
	host, portStr, _ := net.SplitHostPort(s)
	port, _ := strconv.Atoi(portStr)
	return Addr{Host: host, Port: port}
}

func tunnelAddress(a Addr) *bridgev1.TunnelAddress {
	return &bridgev1.TunnelAddress{Ip: a.Host, Port: int32(a.Port)}
}
