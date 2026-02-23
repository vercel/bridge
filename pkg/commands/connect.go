package commands

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os/signal"
	"strings"
	"syscall"

	"github.com/urfave/cli/v3"
	"github.com/vercel/bridge/pkg/sessions"
	"github.com/vercel/bridge/pkg/sshproxy"
)

func Connect() *cli.Command {
	return &cli.Command{
		Name:  "connect",
		Usage: "Connect to a sandbox",
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:  "local-port",
				Usage: "Local port for SSH proxy (random if not specified)",
			},
		},
		Arguments: []cli.Argument{
			&cli.StringArg{
				Name:      "target",
				UsageText: "The name or URL of a sandbox",
				Config: cli.StringConfig{
					TrimSpace: true,
				},
			},
		},
		Action: runConnect,
	}
}

func runConnect(ctx context.Context, c *cli.Command) error {
	arg := c.StringArg("target")
	localPort := c.Int("local-port")

	store, err := sessions.NewStore()
	if err != nil {
		return fmt.Errorf("failed to initialize session store: %w", err)
	}

	var sandboxURL string
	var name string

	// Check if it's a URL or an alias
	if strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://") {
		sandboxURL = arg
		// Extract name from URL if possible
		u, err := url.Parse(arg)
		if err != nil {
			return fmt.Errorf("invalid URL: %w", err)
		}
		name = strings.Split(u.Host, ".")[0]
	} else {
		// Look up in sessions
		session, ok := store.Get(arg)
		if !ok {
			return fmt.Errorf("session %q not found", arg)
		}
		sandboxURL = session.URL
		name = arg
	}

	slog.Info("connecting to sandbox", "name", name, "tunnel", sandboxURL)

	// Create SSH proxy
	proxy, err := sshproxy.New(ctx, sshproxy.Config{
		Name:      name,
		TunnelURL: sandboxURL,
		LocalPort: localPort,
	})
	if err != nil {
		return fmt.Errorf("failed to create SSH proxy: %w", err)
	}
	defer proxy.Close()

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("Sandbox connected: %s\n", name)
	fmt.Printf("SSH: ssh %s\n", proxy.Host())
	fmt.Printf("Local proxy listening on %s\n", proxy.Addr())

	return proxy.Serve(ctx)
}
