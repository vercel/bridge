package commands

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os/exec"

	"github.com/vercel/bridge/pkg/conntrack"
	"github.com/vercel/bridge/pkg/netutil"
	"github.com/vercel/bridge/pkg/plumbing"
)

const (
	// proxyCIDR is the CIDR block for proxy IP allocation (used with DNS interception).
	proxyCIDR = "10.128.0.0/16"
)

// ProxyComponent manages the transparent proxy listener, tunnel client, and iptables rules.
type ProxyComponent struct {
	tunnel   plumbing.Tunnel
	listener net.Listener
	port     int
	registry *conntrack.Registry
}

// ProxyConfig holds configuration for starting the proxy component.
type ProxyConfig struct {
	Tunnel    plumbing.Tunnel
	ProxyPort int // 0 = random
	Registry  *conntrack.Registry
}

// StartProxy creates a transparent proxy listener and initializes the tunnel client.
func StartProxy(cfg ProxyConfig) (*ProxyComponent, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.ProxyPort))
	if err != nil {
		return nil, fmt.Errorf("failed to listen: %w", err)
	}

	addr := listener.Addr().(*net.TCPAddr)

	p := &ProxyComponent{
		tunnel:   cfg.Tunnel,
		listener: listener,
		port:     addr.Port,
		registry: cfg.Registry,
	}

	return p, nil
}

// Port returns the port the transparent proxy is listening on.
func (p *ProxyComponent) Port() int {
	return p.port
}

// SetupIptables creates the BRIDGE_INTERCEPT chain and adds TCP redirect rules
// for traffic destined for the proxy CIDR.
func (p *ProxyComponent) SetupIptables() error {
	if _, err := exec.LookPath("iptables"); err != nil {
		return fmt.Errorf("iptables not found: %w", err)
	}

	cmds := [][]string{
		// Create a new chain for our rules
		{"iptables", "-t", "nat", "-N", "BRIDGE_INTERCEPT"},

		// TCP interception: redirect traffic to our proxy CIDR
		{"iptables", "-t", "nat", "-A", "BRIDGE_INTERCEPT", "-d", proxyCIDR, "-p", "tcp", "-j", "REDIRECT", "--to-ports", fmt.Sprintf("%d", p.port)},

		// Jump to our chain from OUTPUT for TCP
		{"iptables", "-t", "nat", "-A", "OUTPUT", "-d", proxyCIDR, "-p", "tcp", "-j", "BRIDGE_INTERCEPT"},
	}

	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		if output, err := cmd.CombinedOutput(); err != nil {
			slog.Debug("iptables command failed",
				"command", args,
				"error", err,
				"output", string(output),
			)
		}
	}

	slog.Info("iptables rules configured", "proxy_port", p.port, "proxy_cidr", proxyCIDR)
	return nil
}

// Run starts the transparent proxy accept loop and blocks until the context
// is cancelled.
func (p *ProxyComponent) Run(ctx context.Context) {
	slog.Info("Transparent proxy listening", "port", p.port)

	go func() {
		for {
			conn, err := p.listener.Accept()
			if err != nil {
				slog.Error("Accept error", "error", err)
				return
			}
			p.handleOutbound(conn)
		}
	}()

	<-ctx.Done()
}

// Stop cleans up iptables rules and closes the listener.
// The tunnel client is owned by the orchestrator and closed separately.
func (p *ProxyComponent) Stop() {
	p.cleanupIptables()
	if p.listener != nil {
		_ = p.listener.Close()
	}
}

func (p *ProxyComponent) handleOutbound(clientConn net.Conn) {
	origDst, err := netutil.OriginalDest(clientConn)
	if err != nil {
		slog.Error("Failed to get original destination", "error", err)
		clientConn.Close()
		return
	}

	// If we have a registry, resolve the proxy IP back to the real IP
	destination := origDst
	hostname := ""
	if p.registry != nil {
		host, port, err := net.SplitHostPort(origDst)
		if err == nil {
			proxyIP := net.ParseIP(host)
			if proxyIP != nil {
				if entry := p.registry.LookupAndMark(proxyIP); entry != nil {
					destination = net.JoinHostPort(entry.ResolvedIP.String(), port)
					hostname = entry.Hostname
				}
			}
		}
	}

	slog.Info("Intercepted outbound connection",
		"source", clientConn.RemoteAddr(),
		"orig_dst", origDst,
		"destination", destination,
		"hostname", hostname,
	)

	// Hand the connection to the tunnel; it owns the conn from here.
	p.tunnel.AddConn(clientConn, destination)
}

func (p *ProxyComponent) cleanupIptables() {
	cmds := [][]string{
		// Remove TCP jump rule
		{"iptables", "-t", "nat", "-D", "OUTPUT", "-d", proxyCIDR, "-p", "tcp", "-j", "BRIDGE_INTERCEPT"},
		// Flush and delete our chain
		{"iptables", "-t", "nat", "-F", "BRIDGE_INTERCEPT"},
		{"iptables", "-t", "nat", "-X", "BRIDGE_INTERCEPT"},
	}

	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		_ = cmd.Run()
	}
	slog.Info("iptables rules cleaned up")
}
