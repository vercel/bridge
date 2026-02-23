package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync/atomic"

	"github.com/vercel/bridge/pkg/bidi"
)

// Dialer establishes connections to a remote server.
type Dialer interface {
	Dial(ctx context.Context) (io.ReadWriteCloser, error)
}

// TCPDialer dials a fixed TCP address.
type TCPDialer struct {
	Addr string
}

// Dial connects to the configured TCP address.
func (d *TCPDialer) Dial(ctx context.Context) (io.ReadWriteCloser, error) {
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", d.Addr)
}

var _ net.Listener = (*TCPProxyListener)(nil)

// TCPProxyListener listens for local TCP connections and forwards them through a dialer.
type TCPProxyListener struct {
	ctx         context.Context
	listener    net.Listener
	dialer      Dialer
	connections atomic.Int64
}

// NewTCPProxyListener creates a new TCP proxy listener that forwards connections through the given dialer.
func NewTCPProxyListener(ctx context.Context, listenAddr string, dialer Dialer) (*TCPProxyListener, error) {
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen: %w", err)
	}

	p := &TCPProxyListener{
		ctx:      ctx,
		listener: listener,
		dialer:   dialer,
	}

	slog.Info("starting tcp proxy", "addr", listener.Addr().String())

	return p, nil
}

// Accept implements net.Listener.
func (p *TCPProxyListener) Accept() (net.Conn, error) {
	return p.listener.Accept()
}

// Close implements net.Listener.
func (p *TCPProxyListener) Close() error {
	slog.Info("shutting down tcp proxy")
	return p.listener.Close()
}

// Addr implements net.Listener.
func (p *TCPProxyListener) Addr() net.Addr {
	return p.listener.Addr()
}

// Port returns the port the proxy is bound to.
func (p *TCPProxyListener) Port() int {
	return p.listener.Addr().(*net.TCPAddr).Port
}

// ActiveConnections returns the number of active connections.
func (p *TCPProxyListener) ActiveConnections() int64 {
	return p.connections.Load()
}

// HandleConn forwards a client connection through the tunnel.
func (p *TCPProxyListener) HandleConn(clientConn net.Conn) {
	p.connections.Add(1)
	defer p.connections.Add(-1)

	remoteAddr := clientConn.RemoteAddr().String()
	slog.Info("client connected to tcp proxy", "remote", remoteAddr)

	defer func() {
		clientConn.Close()
		slog.Info("client disconnected from tcp proxy", "remote", remoteAddr)
	}()

	tunnelConn, err := p.dialer.Dial(p.ctx)
	if err != nil {
		slog.Error("failed to establish tunnel", "error", err, "remote", remoteAddr)
		return
	}
	defer tunnelConn.Close()

	slog.Info("tunnel established", "remote", remoteAddr)

	bidi.New(clientConn, tunnelConn).Wait(p.ctx)
}
