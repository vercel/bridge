package tunnel

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"

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

	// Start runs the backend, dialing a local conn for each the peer opens.
	Start(ctx context.Context)

	// Close stops the backend, which ends every conn.
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
	dialer   plumbing.ContextDialer
	hijacker mitm.Hijacker
	backend  Backend
	bufs     sync.Pool
	logger   *slog.Logger
	done     chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
}

func (t *tunnelImpl) Done() <-chan struct{} { return t.done }

func (t *tunnelImpl) Start(ctx context.Context) {
	stop := context.AfterFunc(ctx, t.cancel)
	go func() {
		defer stop()
		defer close(t.done)
		defer t.cancel()
		if err := t.backend.Run(t.ctx, t.accept); err != nil {
			t.logger.Info("Tunnel: backend stopped", "error", err)
		}
	}()
}

func (t *tunnelImpl) Close() error {
	t.cancel()
	return nil
}

func (t *tunnelImpl) AddConn(conn net.Conn, destOverride string, hostname string) {
	srcAddr := conn.RemoteAddr().String()
	dstAddr := conn.LocalAddr().String()
	if destOverride != "" {
		dstAddr = destOverride
	}
	info := &ConnInfo{Source: splitAddr(srcAddr), Dest: splitAddr(dstAddr), Hostname: hostname}
	logger := t.logger.With("src", srcAddr, "dst", dstAddr, "hostname", hostname)
	logger.Debug("Tunnel: adding connection")

	go func() {
		remote, err := t.backend.Open(t.ctx, info)
		if err != nil {
			logger.Info("Tunnel: open failed", "error", err)
			_ = conn.Close()
			return
		}
		t.pump(conn, remote)
	}()
}

// accept dials a conn the peer opened, giving the hijacker first shot at it.
func (t *tunnelImpl) accept(remote net.Conn, info *ConnInfo) {
	go func() {
		local, err := t.dial(info)
		if err != nil {
			t.logger.Info("Tunnel: connect failed", "dest", info.Dest.String(), "hostname", info.Hostname, "error", err)
			_ = remote.Close()
			return
		}
		t.pump(local, remote)
	}()
}

func (t *tunnelImpl) dial(info *ConnInfo) (net.Conn, error) {
	if t.hijacker != nil {
		msg := &bridgev1.TunnelNetworkMessage{
			Source:   tunnelAddress(info.Source),
			Dest:     tunnelAddress(info.Dest),
			Hostname: info.Hostname,
		}
		if t.hijacker.ShouldHijack(msg) {
			conn, err := t.hijacker.Hijack(t.ctx, msg)
			if err == nil {
				return conn, nil
			}
			t.logger.Info("Tunnel: hijack failed, falling back to dialer", "hostname", info.Hostname, "error", err)
		}
	}
	return t.dialer.DialContext(t.ctx, "tcp", info.Dest.String())
}

// pump copies between local and remote in both directions, each through a
// buffer from the pool, then closes both. Either side's EOF half-closes the
// other; a failure either way closes both, which resets remote.
func (t *tunnelImpl) pump(local, remote net.Conn) {
	toRemote := t.bufs.Get().(*[]byte)
	fromRemote := t.bufs.Get().(*[]byte)
	defer func() {
		t.bufs.Put(toRemote)
		t.bufs.Put(fromRemote)
	}()

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

	if m, ok := remote.(plumbing.MeteredConn); ok {
		st := m.Stats()
		t.logger.Debug("Tunnel: connection closed",
			"remote", remote.RemoteAddr().String(),
			"bytes_sent", st.BytesWritten,
			"bytes_received", st.BytesRead,
			"write_time", st.WriteTime,
		)
	}
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

func splitAddr(s string) Addr {
	host, portStr, _ := net.SplitHostPort(s)
	port, _ := strconv.Atoi(portStr)
	return Addr{Host: host, Port: port}
}

func tunnelAddress(a Addr) *bridgev1.TunnelAddress {
	return &bridgev1.TunnelAddress{Ip: a.Host, Port: int32(a.Port)}
}
