package commands

import (
	"fmt"
	"log/slog"
	"net"

	"github.com/vercel/bridge/pkg/socks"
	"github.com/vercel/bridge/pkg/tunnel"
)

// SocksComponent manages a SOCKS5 proxy that feeds connections into a tunnel.
// This is the sandbox-friendly alternative to ProxyComponent (iptables).
type SocksComponent struct {
	server *socks.Server
	tunnel tunnel.Tunnel
}

// SocksConfig holds configuration for starting the SOCKS proxy.
type SocksConfig struct {
	Tunnel    tunnel.Tunnel
	SocksAddr string // e.g. ":1080"
}

// StartSocks creates and starts a SOCKS5 proxy that tunnels connections.
func StartSocks(cfg SocksConfig) (*SocksComponent, error) {
	c := &SocksComponent{tunnel: cfg.Tunnel}

	server, err := socks.New(cfg.SocksAddr, c.handleConnect)
	if err != nil {
		return nil, fmt.Errorf("socks proxy: %w", err)
	}
	c.server = server
	return c, nil
}

// Start begins accepting SOCKS connections in the background.
func (c *SocksComponent) Start() {
	c.server.Start()
}

// Port returns the SOCKS proxy listening port.
func (c *SocksComponent) Port() int {
	return c.server.Port()
}

// Stop closes the SOCKS server.
func (c *SocksComponent) Stop() {
	c.server.Stop()
}

func (c *SocksComponent) handleConnect(conn net.Conn, dest string, hostname string) {
	slog.Info("SOCKS5 tunneling connection", "dest", dest, "hostname", hostname)
	c.tunnel.AddConn(conn, dest, hostname)
}
