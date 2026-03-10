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
	tunnel     plumbing.Tunnel
	listener   net.Listener
	port       int
	registry   *conntrack.Registry
	dnsPort    int
	excludeGID int
}

// ProxyConfig holds configuration for starting the proxy component.
type ProxyConfig struct {
	Tunnel     plumbing.Tunnel
	ProxyPort  int // 0 = random
	Registry   *conntrack.Registry
	DNSPort    int // If >0, redirect all UDP :53 traffic to this port
	ExcludeGID int // If >0, exclude this GID from DNS redirect
}

// StartProxy creates a transparent proxy listener and initializes the tunnel client.
func StartProxy(cfg ProxyConfig) (*ProxyComponent, error) {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.ProxyPort))
	if err != nil {
		return nil, fmt.Errorf("failed to listen: %w", err)
	}

	addr := listener.Addr().(*net.TCPAddr)

	p := &ProxyComponent{
		tunnel:     cfg.Tunnel,
		listener:   listener,
		port:       addr.Port,
		registry:   cfg.Registry,
		dnsPort:    cfg.DNSPort,
		excludeGID: cfg.ExcludeGID,
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

	// Redirect all outbound UDP :53 traffic to the local DNS server.
	// This catches processes like nginx that use their own resolver directive
	// instead of /etc/resolv.conf. The DNS server's own fallback queries use
	// TCP, so they are not affected by this rule.
	if p.dnsPort > 0 {
		// Exclude bridge's own GID from the DNS redirect. When the gRPC
		// connection drops and reconnects, the k8spf dialer calls
		// resolvePod → K8s API → aws eks get-token, which needs real DNS
		// to reach sts.amazonaws.com. Without this exclusion, the query
		// loops back to our DNS server which depends on the broken tunnel.
		if p.excludeGID > 0 {
			cmds = append(cmds,
				[]string{"iptables", "-t", "nat", "-A", "BRIDGE_INTERCEPT",
					"-p", "udp", "--dport", "53",
					"-m", "owner", "--gid-owner", fmt.Sprintf("%d", p.excludeGID),
					"-j", "RETURN"},
			)
		}
		cmds = append(cmds,
			[]string{"iptables", "-t", "nat", "-A", "BRIDGE_INTERCEPT", "-p", "udp", "--dport", "53", "-j", "REDIRECT", "--to-ports", fmt.Sprintf("%d", p.dnsPort)},
			[]string{"iptables", "-t", "nat", "-A", "OUTPUT", "-p", "udp", "--dport", "53", "-j", "BRIDGE_INTERCEPT"},
		)
	}

	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("iptables %v failed: %w: %s", args[1:], err, output)
		}
	}

	slog.Info("iptables rules configured", "proxy_port", p.port, "proxy_cidr", proxyCIDR)
	return nil
}

// Start begins the transparent proxy accept loop in the background.
func (p *ProxyComponent) Start() {
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
}

// Wait blocks until the context is cancelled.
func (p *ProxyComponent) Wait(ctx context.Context) {
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
		// Remove jump rules from OUTPUT
		{"iptables", "-t", "nat", "-D", "OUTPUT", "-d", proxyCIDR, "-p", "tcp", "-j", "BRIDGE_INTERCEPT"},
	}
	if p.dnsPort > 0 {
		cmds = append(cmds,
			[]string{"iptables", "-t", "nat", "-D", "OUTPUT", "-p", "udp", "--dport", "53", "-j", "BRIDGE_INTERCEPT"},
		)
	}
	cmds = append(cmds,
		// Flush and delete our chain
		[]string{"iptables", "-t", "nat", "-F", "BRIDGE_INTERCEPT"},
		[]string{"iptables", "-t", "nat", "-X", "BRIDGE_INTERCEPT"},
	)

	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		_ = cmd.Run()
	}
	slog.Info("iptables rules cleaned up")
}
