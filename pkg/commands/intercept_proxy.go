package commands

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os/exec"

	"github.com/vercel-eddie/bridge/pkg/bidi"
	"github.com/vercel-eddie/bridge/pkg/conntrack"
	"github.com/vercel-eddie/bridge/pkg/tunnel"
)

const (
	// proxyCIDR is the CIDR block for proxy IP allocation (used with DNS interception).
	proxyCIDR = "10.128.0.0/16"
)

// ProxyComponent manages the transparent proxy listener, tunnel client, and iptables rules.
type ProxyComponent struct {
	tunnel   *tunnel.Client
	listener net.Listener
	port     int
	registry *conntrack.Registry
}

// ProxyConfig holds configuration for starting the proxy component.
type ProxyConfig struct {
	TunnelClient *tunnel.Client
	ProxyPort    int // 0 = random
	Registry     *conntrack.Registry
}

// StartProxy creates a transparent proxy listener and initializes the tunnel client.
func StartProxy(cfg ProxyConfig) (*ProxyComponent, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.ProxyPort))
	if err != nil {
		return nil, fmt.Errorf("failed to listen: %w", err)
	}

	addr := listener.Addr().(*net.TCPAddr)

	p := &ProxyComponent{
		tunnel:   cfg.TunnelClient,
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

// Run starts the transparent proxy accept loop in a goroutine, then blocks
// on tunnel.ConnectWithReconnect until the context is cancelled.
func (p *ProxyComponent) Run(ctx context.Context) {
	slog.Info("Transparent proxy listening", "port", p.port)

	go func() {
		for {
			conn, err := p.listener.Accept()
			if err != nil {
				slog.Error("Accept error", "error", err)
				return
			}
			go p.handleOutbound(conn)
		}
	}()

	p.tunnel.ConnectWithReconnect(ctx)
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
	defer clientConn.Close()

	sourceAddr := clientConn.RemoteAddr().String()

	origDst, err := getOriginalDst(clientConn)
	if err != nil {
		slog.Error("Failed to get original destination", "error", err)
		return
	}

	// If we have a registry, try to resolve the proxy IP back to a hostname
	destination := origDst
	if p.registry != nil {
		host, port, err := net.SplitHostPort(origDst)
		if err == nil {
			proxyIP := net.ParseIP(host)
			if proxyIP != nil {
				if entry := p.registry.LookupAndMark(proxyIP); entry != nil {
					destination = net.JoinHostPort(entry.Hostname, port)
					defer p.registry.Release(proxyIP)
					slog.Debug("Resolved proxy IP to hostname",
						"proxy_ip", host,
						"hostname", entry.Hostname,
						"real_ip", entry.ResolvedIP,
					)
				}
			}
		}
	}

	slog.Debug("Intercepted outbound connection", "source", sourceAddr, "destination", destination)

	targetConn, err := p.tunnel.DialThroughTunnel(sourceAddr, destination)
	if err != nil {
		slog.Error("Failed to dial through tunnel", "source", sourceAddr, "destination", destination, "error", err)
		return
	}
	defer targetConn.Close()

	slog.Info("Proxying connection", "source", sourceAddr, "destination", destination)

	bidi.New(clientConn, targetConn).Wait(context.Background())
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
