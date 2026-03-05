package commands

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/conntrack"
	bridgedns "github.com/vercel/bridge/pkg/dns"
	"github.com/vercel/bridge/pkg/ippool"
	"github.com/vercel/bridge/pkg/k8s/k8spf"
	"github.com/vercel/bridge/pkg/plumbing"
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
			&cli.StringSliceFlag{
				Name:  "copy-files",
				Usage: "File paths to copy from the bridge proxy into the devcontainer (also appends from $COPY_FILES)",
			},
		},
		Action: runIntercept,
	}
}

func runIntercept(ctx context.Context, c *cli.Command) error {
	err := doIntercept(ctx, c)
	if err != nil {
		slog.Error("Intercept crashed", "error", err)
	}
	return err
}

func doIntercept(ctx context.Context, c *cli.Command) error {
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

	// Parse copy-files from both the flag and the COPY_FILES env var, merging them.
	var copyFiles []string
	for _, src := range append(c.StringSlice("copy-files"), os.Getenv("COPY_FILES")) {
		for _, part := range strings.Split(src, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				copyFiles = append(copyFiles, part)
			}
		}
	}

	if u, err := user.Current(); err == nil {
		slog.Info("Intercept process starting",
			"user", u.Username,
			"home", u.HomeDir,
		)
	} else {
		slog.Info("Intercept process starting", "user_lookup_error", err)
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

	// Copy files from the proxy pod if requested.
	if len(copyFiles) > 0 {
		if err := copyFilesFromProxy(ctx, client, copyFiles); err != nil {
			slog.Warn("Failed to copy files from proxy", "error", err)
		}
	}

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
	var dnsPortForIPTables int
	if dns != nil {
		dnsPortForIPTables = dns.Port()
	}
	proxyComp, err := StartProxy(ProxyConfig{
		Tunnel:    tun,
		ProxyPort: proxyPort,
		Registry:  registry,
		DNSPort:   dnsPortForIPTables,
	})
	if err != nil {
		return fmt.Errorf("failed to start proxy listener: %w", err)
	}

	slog.Info("Bridge intercept starting",
		"server_addr", serverAddr,
		"proxy_port", proxyComp.Port(),
	)

	// Set up iptables (TCP redirect for proxy CIDR, UDP redirect for DNS)
	if err := proxyComp.SetupIptables(); err != nil {
		return fmt.Errorf("failed to setup iptables: %w", err)
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

	// Test hook: simulate a crash after full initialization.
	if os.Getenv("__TEST_FAIL_INTERCEPT") == "true" {
		return fmt.Errorf("injected test failure")
	}

	// Intercept is fully initialized.
	slog.Info("Intercept ready")

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

func (r *grpcDNSResolver) ResolveDNS(ctx context.Context, hostname string) (*bridgedns.DNSResolveResult, error) {
	resp, err := r.client.ResolveDNSQuery(ctx, &bridgev1.ProxyResolveDNSRequest{
		Hostname: hostname,
	})
	if err != nil {
		return nil, fmt.Errorf("ResolveDNSQuery RPC: %w", err)
	}
	return &bridgedns.DNSResolveResult{
		Addresses: resp.GetAddresses(),
		Error:     resp.GetError(),
	}, nil
}

// copyFilesFromProxy calls the CopyFiles RPC and writes each file to the local
// filesystem at the same absolute path with relaxed permissions (0666/0777).
func copyFilesFromProxy(ctx context.Context, client bridgev1.BridgeProxyServiceClient, paths []string) error {
	slog.Info("Copying files from proxy pod", "paths", paths)

	resp, err := client.CopyFiles(ctx, &bridgev1.CopyFilesRequest{Paths: paths})
	if err != nil {
		return fmt.Errorf("CopyFiles RPC: %w", err)
	}

	for _, f := range resp.GetFiles() {
		if f.Error != "" {
			slog.Warn("Failed to copy file", "path", f.Path, "error", f.Error)
			continue
		}

		// Ensure parent directory exists.
		if err := os.MkdirAll(filepath.Dir(f.Path), 0777); err != nil {
			slog.Warn("Failed to create directory", "path", filepath.Dir(f.Path), "error", err)
			continue
		}

		// Write with relaxed permissions so all users can access.
		if err := os.WriteFile(f.Path, f.Content, 0666); err != nil {
			slog.Warn("Failed to write file", "path", f.Path, "error", err)
			continue
		}

		// Restore modification time.
		if f.ModTime > 0 {
			modTime := time.Unix(f.ModTime, 0)
			os.Chtimes(f.Path, modTime, modTime)
		}

		slog.Info("Copied file", "path", f.Path, "size", len(f.Content))
	}

	return nil
}
