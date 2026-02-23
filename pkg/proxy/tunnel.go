package proxy

import (
	"context"
	"log/slog"
	"net"
	"time"

	"github.com/vercel/bridge/pkg/bidi"
)

// Tunnel handles forwarding a proxy connection to a target.
type Tunnel struct {
	target      string
	dialTimeout time.Duration
}

// NewTunnel creates a new tunnel with a configured target.
func NewTunnel(target string) *Tunnel {
	return &Tunnel{
		target:      target,
		dialTimeout: 10 * time.Second,
	}
}

// Handle dials the target and pipes data between the client and target.
func (t *Tunnel) Handle(ctx context.Context, conn Conn) {
	remoteAddr := conn.Net.RemoteAddr().String()

	slog.Debug("client connected", "remote", remoteAddr, "target", t.target)
	defer func() {
		conn.Net.Close()
		slog.Debug("client disconnected", "remote", remoteAddr, "target", t.target)
	}()

	targetConn, err := net.DialTimeout("tcp", t.target, t.dialTimeout)
	if err != nil {
		slog.Error("failed to dial target", "error", err, "target", t.target, "remote", remoteAddr)
		return
	}
	defer targetConn.Close()

	slog.Debug("connected to target", "target", t.target, "remote", remoteAddr)

	bidi.New(conn.Net, targetConn).Wait(ctx)
}
