package commands

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/urfave/cli/v3"
	"github.com/vercel-eddie/bridge/pkg/conntrack"
	bridgedns "github.com/vercel-eddie/bridge/pkg/dns"
	"github.com/vercel-eddie/bridge/pkg/ippool"
	"github.com/vercel-eddie/bridge/pkg/tunnel"
)

func Intercept() *cli.Command {
	return &cli.Command{
		Name:  "intercept",
		Usage: "Intercept and tunnel traffic (run inside Devcontainer)",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     "sandbox-url",
				Usage:    "URL of the bridge server in the sandbox",
				Sources:  cli.EnvVars("SANDBOX_URL"),
				Required: true,
			},
			&cli.StringFlag{
				Name:     "function-url",
				Usage:    "URL of the dispatcher function",
				Sources:  cli.EnvVars("FUNCTION_URL"),
				Required: true,
			},
			&cli.StringFlag{
				Name:    "name",
				Usage:   "Name for the sandbox (used for SSH config alias)",
				Sources: cli.EnvVars("SANDBOX_NAME"),
			},
			&cli.IntFlag{
				Name:  "proxy-port",
				Usage: "Port for transparent proxy (0 = random)",
				Value: 0,
			},
			&cli.IntFlag{
				Name:  "ssh-proxy-port",
				Usage: "Local port for SSH proxy (0 = random)",
				Value: 0,
			},
			&cli.StringFlag{
				Name:    "sync-source",
				Usage:   "Local directory to sync from",
				Sources: cli.EnvVars("SYNC_SOURCE"),
				Value:   ".",
			},
			&cli.StringFlag{
				Name:    "sync-target",
				Usage:   "Remote directory on sandbox to sync to (default: root@<name>:/vercel/sandbox)",
				Sources: cli.EnvVars("SYNC_TARGET"),
			},
			&cli.BoolFlag{
				Name:  "no-sync",
				Usage: "Disable file sync",
			},
			&cli.BoolFlag{
				Name:  "no-ssh-proxy",
				Usage: "Disable SSH proxy",
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
	sandboxURL := c.String("sandbox-url")
	functionURL := c.String("function-url")
	name := c.String("name")
	proxyPort := c.Int("proxy-port")
	sshProxyPort := c.Int("ssh-proxy-port")
	syncSource := c.String("sync-source")
	syncTarget := c.String("sync-target")
	noSync := c.Bool("no-sync")
	noSSHProxy := c.Bool("no-ssh-proxy")
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

	// Derive name from sandbox URL if not provided
	if name == "" {
		u, err := url.Parse(sandboxURL)
		if err == nil {
			name = strings.Split(u.Host, ".")[0]
		} else {
			name = "sandbox"
		}
	}

	// Create connection tracking registry
	pool, err := ippool.New(proxyCIDR)
	if err != nil {
		return fmt.Errorf("failed to create IP pool: %w", err)
	}
	registry := conntrack.New(pool)

	// Create tunnel client (shared by DNS exchange client and proxy)
	tunnelClient := tunnel.NewClient(sandboxURL, functionURL, fmt.Sprintf("127.0.0.1:%d", appPort))

	// Start DNS (optional)
	var dns *DNSComponent
	var originalResolvConf []byte
	if len(forwardDomains) > 0 {
		// Read the original nameserver before we modify /etc/resolv.conf
		originalNS := readOriginalNameserver()
		exchangeClient := bridgedns.NewTunnelExchangeClient(forwardDomains, tunnelClient, originalNS)
		dns, err = StartDNS(DNSConfig{
			ListenPort: dnsPort,
			Client:     exchangeClient,
			Registry:   registry,
		})
		if err != nil {
			registry.Stop()
			_ = tunnelClient.Close()
			return fmt.Errorf("failed to start DNS: %w", err)
		}

		// Point /etc/resolv.conf at our DNS server
		originalResolvConf, err = updateResolvConf("127.0.0.1")
		if err != nil {
			slog.Warn("Failed to update /etc/resolv.conf", "error", err)
		}
	}

	// Start proxy (transparent proxy + tunnel)
	proxy, err := StartProxy(ProxyConfig{
		TunnelClient: tunnelClient,
		ProxyPort:    proxyPort,
		Registry:     registry,
	})
	if err != nil {
		_ = tunnelClient.Close()
		return fmt.Errorf("failed to start proxy listener: %w", err)
	}

	slog.Info("Bridge intercept starting",
		"name", name,
		"sandbox_url", sandboxURL,
		"function_url", functionURL,
		"proxy_port", proxy.Port(),
	)

	// Set up iptables (TCP redirect for proxy CIDR)
	if err := proxy.SetupIptables(); err != nil {
		slog.Warn("Failed to setup iptables",
			"error", err,
			"hint", "Traffic interception requires NET_ADMIN capability",
		)
	}

	// Start SSH (optional)
	var ssh *SSHComponent
	if !noSSHProxy {
		ssh, err = StartSSH(ctx, SSHConfig{
			Name:       name,
			SandboxURL: sandboxURL,
			LocalPort:  sshProxyPort,
		})
		if err != nil {
			slog.Warn("Failed to start SSH proxy", "error", err)
		} else {
			fmt.Printf("SSH: ssh %s\n", ssh.Host())
		}
	}

	// Create cancellable context
	ctx, cancel := context.WithCancel(ctx)

	// Signal handler (stops components in reverse order)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	var fileSync *FileSyncComponent

	go func() {
		<-sigChan
		slog.Info("Shutting down...")
		cancel()
		if fileSync != nil {
			fileSync.Stop()
		}
		if ssh != nil {
			ssh.Stop()
		}
		if dns != nil {
			dns.Stop()
		}
		restoreResolvConf(originalResolvConf)
		if registry != nil {
			registry.Stop()
		}
		proxy.Stop()
		_ = tunnelClient.Close()
		os.Exit(0)
	}()

	// Start SSH serve loop
	if ssh != nil {
		sshProxyReady := make(chan struct{})
		go func() {
			close(sshProxyReady)
			if err := ssh.Serve(ctx); err != nil && ctx.Err() == nil {
				slog.Error("SSH proxy error", "error", err)
			}
		}()
		<-sshProxyReady
	}

	// Derive sync target from SSH proxy if not explicitly provided
	if syncTarget == "" && ssh != nil {
		syncTarget = fmt.Sprintf("vercel-sandbox@%s:/vercel/sandbox", ssh.Host())
	}

	// Start file sync (optional, depends on SSH for target)
	if !noSync && syncTarget != "" {
		fileSync, err = StartFileSync(FileSyncConfig{
			SyncName:   "bridge-sync",
			SyncSource: syncSource,
			SyncTarget: syncTarget,
		})
		if err != nil {
			slog.Error("Failed to start file sync", "error", err)
		}
	}

	// Run proxy (blocking)
	proxy.Run(ctx)

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
