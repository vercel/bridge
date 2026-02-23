package commands

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/urfave/cli/v3"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/conntrack"
	bridgedns "github.com/vercel/bridge/pkg/dns"
	"github.com/vercel/bridge/pkg/ippool"
	"github.com/vercel/bridge/pkg/k8s/k8spf"
	"github.com/vercel/bridge/pkg/plumbing"
	"github.com/vercel/bridge/pkg/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func Intercept() *cli.Command {
	return &cli.Command{
		Name:   "intercept",
		Usage:  "Intercept and tunnel traffic (run inside Devcontainer)",
		Hidden: true,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "server-addr",
				Usage:    "Address of the bridge proxy server (e.g. localhost:9090 or k8spf:///pod.ns:9090)",
				Sources:  cli.EnvVars("BRIDGE_SERVER_ADDR"),
				Required: true,
			},
			&cli.IntFlag{
				Name:  "proxy-port",
				Usage: "Port for transparent proxy (0 = random)",
				Value: 0,
			},
			&cli.IntFlag{
				Name:    "app-port",
				Usage:   "Local app port to forward inbound requests to",
				Value:   3000,
				Sources: cli.EnvVars("APP_PORT"),
			},
			&cli.StringSliceFlag{
				Name:    "forward-domains",
				Usage:   "Domain patterns to intercept via DNS (e.g., '*.example.com')",
				Sources: cli.EnvVars("FORWARD_DOMAINS"),
			},
			&cli.IntFlag{
				Name:  "dns-port",
				Usage: "DNS server listen port (default: 53)",
				Value: 53,
			},
		},
		Action: runIntercept,
	}
}

func runIntercept(ctx context.Context, c *cli.Command) error {
	serverAddr := c.String("server-addr")
	proxyPort := c.Int("proxy-port")
	appPort := c.Int("app-port")
	dnsPort := c.Int("dns-port")

	// Parse forward-domains, handling comma-separated values from env vars
	var forwardDomains []string
	for _, d := range c.StringSlice("forward-domains") {
		for _, part := range strings.Split(d, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				forwardDomains = append(forwardDomains, part)
			}
		}
	}

	if len(forwardDomains) > 0 {
		slog.Info("Forward domains configured", "domains", forwardDomains)
	} else {
		slog.Info("No forward domains configured, DNS interception disabled")
	}

	// Connect to the bridge proxy server via gRPC
	builder := k8spf.NewBuilder(k8spf.BuilderConfig{})
	conn, err := grpc.NewClient(serverAddr,
		append(builder.DialOptions(), grpc.WithTransportCredentials(insecure.NewCredentials()))...,
	)
	if err != nil {
		return fmt.Errorf("failed to connect to bridge proxy: %w", err)
	}
	defer conn.Close()
	client := bridgev1.NewBridgeProxyServiceClient(conn)

	// Open the shared tunnel stream. If app-port is set, ingress traffic from
	// the server's --listen-ports will be forwarded to that local port.
	stream, err := client.TunnelNetwork(ctx)
	if err != nil {
		return fmt.Errorf("failed to open tunnel stream: %w", err)
	}

	dialer := plumbing.NewStaticPortDialer(appPort, nil)
	tun := plumbing.NewTunnel(dialer, stream)
	tun.Start(ctx)
	slog.Info("Tunnel connected", "app_port", appPort)

	// Create connection tracking registry
	pool, err := ippool.New(proxyCIDR)
	if err != nil {
		return fmt.Errorf("failed to create IP pool: %w", err)
	}
	registry := conntrack.New(pool)

	// Start DNS (optional)
	var dns *DNSComponent
	var originalResolvConf []byte
	if len(forwardDomains) > 0 {
		// Read the original nameserver before we modify /etc/resolv.conf
		originalNS := readOriginalNameserver()
		exchangeClient := bridgedns.NewTunnelExchangeClient(forwardDomains, &grpcDNSResolver{client: client}, originalNS)
		dns, err = StartDNS(DNSConfig{
			ListenPort: dnsPort,
			Client:     exchangeClient,
			Registry:   registry,
		})
		if err != nil {
			registry.Stop()
			return fmt.Errorf("failed to start DNS: %w", err)
		}

		// Point /etc/resolv.conf at our DNS server
		originalResolvConf, err = updateResolvConf("127.0.0.1")
		if err != nil {
			slog.Warn("Failed to update /etc/resolv.conf", "error", err)
		}
	}

	// Start proxy (transparent proxy + tunnel)
	proxyComp, err := StartProxy(ProxyConfig{
		Tunnel:    tun,
		ProxyPort: proxyPort,
		Registry:  registry,
	})
	if err != nil {
		return fmt.Errorf("failed to start proxy listener: %w", err)
	}

	slog.Info("Bridge intercept starting",
		"server_addr", serverAddr,
		"proxy_port", proxyComp.Port(),
	)

	// Set up iptables (TCP redirect for proxy CIDR)
	if err := proxyComp.SetupIptables(); err != nil {
		slog.Warn("Failed to setup iptables",
			"error", err,
			"hint", "Traffic interception requires NET_ADMIN capability",
		)
	}

	// Create cancellable context for signal handling
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		slog.Info("Shutting down...")
		cancel()
	}()

	// Block until context is cancelled
	proxyComp.Run(ctx)

	// Cleanup in order: DNS → resolv.conf → registry → proxy
	if dns != nil {
		dns.Stop()
	}
	restoreResolvConf(originalResolvConf)
	registry.Stop()
	proxyComp.Stop()

	return nil
}

// readOriginalNameserver reads the first nameserver from /etc/resolv.conf.
func readOriginalNameserver() string {
	data, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return "8.8.8.8:53"
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && fields[0] == "nameserver" {
			return fields[1] + ":53"
		}
	}
	return "8.8.8.8:53"
}

// updateResolvConf prepends a nameserver entry to /etc/resolv.conf and returns
// the original content for later restoration.
func updateResolvConf(nameserverIP string) ([]byte, error) {
	original, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil, err
	}
	newContent := fmt.Sprintf("nameserver %s\n%s", nameserverIP, string(original))
	if err := os.WriteFile("/etc/resolv.conf", []byte(newContent), 0644); err != nil {
		return original, err
	}
	slog.Info("Updated /etc/resolv.conf", "nameserver", nameserverIP)
	return original, nil
}

// restoreResolvConf restores /etc/resolv.conf to its original content.
func restoreResolvConf(original []byte) {
	if original == nil {
		return
	}
	if err := os.WriteFile("/etc/resolv.conf", original, 0644); err != nil {
		slog.Warn("Failed to restore /etc/resolv.conf", "error", err)
	} else {
		slog.Info("Restored /etc/resolv.conf")
	}
}

// grpcDNSResolver wraps a BridgeProxyServiceClient to satisfy the DNS resolver
// interface used by TunnelExchangeClient.
type grpcDNSResolver struct {
	client bridgev1.BridgeProxyServiceClient
}

func (r *grpcDNSResolver) ResolveDNS(ctx context.Context, hostname string) (*tunnel.DNSResolveResult, error) {
	resp, err := r.client.ResolveDNSQuery(ctx, &bridgev1.ProxyResolveDNSRequest{
		Hostname: hostname,
	})
	if err != nil {
		return nil, fmt.Errorf("ResolveDNSQuery RPC: %w", err)
	}
	return &tunnel.DNSResolveResult{
		Addresses: resp.GetAddresses(),
		Error:     resp.GetError(),
	}, nil
}
