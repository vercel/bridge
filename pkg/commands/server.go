package commands

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os/signal"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"
	"github.com/vercel-eddie/bridge/pkg/mutagen"
	"github.com/vercel-eddie/bridge/pkg/proxy"
	"github.com/vercel-eddie/bridge/pkg/sshserver"
)

func Server() *cli.Command {
	return &cli.Command{
		Name:  "server",
		Usage: "Start the SSH server",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "name",
				Usage:   "Name of the sandbox",
				Sources: cli.EnvVars("REACH_NAME"),
			},
			&cli.StringFlag{
				Name:    "addr",
				Usage:   "Address to bind the server to",
				Value:   ":3000",
				Sources: cli.EnvVars("BRIDGE_ADDR"),
			},
			&cli.IntFlag{
				Name:    "ssh-port",
				Usage:   "SSH port to listen on",
				Value:   2222,
				Sources: cli.EnvVars("SSH_PORT"),
			},
			&cli.DurationFlag{
				Name:    "idle-timeout",
				Usage:   "Idle timeout for connections",
				Value:   30 * time.Minute,
				Sources: cli.EnvVars("SSH_IDLE_TIMEOUT"),
			},
			&cli.DurationFlag{
				Name:    "max-timeout",
				Usage:   "Max timeout for connections",
				Value:   2 * time.Hour,
				Sources: cli.EnvVars("SSH_MAX_TIMEOUT"),
			},
		},
		Action: runServer,
	}
}

func runServer(ctx context.Context, c *cli.Command) error {
	// Install mutagen agent if not already installed
	// This is needed for file sync between devcontainer and sandbox
	if err := mutagen.InstallAgent(); err != nil {
		slog.Warn("Failed to install mutagen agent", "error", err)
		// Continue anyway - sync just won't work
	} else {
		slog.Info("Mutagen agent ready", "path", mutagen.AgentPath())
	}

	name := c.String("name")
	addr := c.String("addr")
	sshPort := c.Int("ssh-port")

	// Parse host from addr for SSH server binding
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid addr %q: %w", addr, err)
	}
	if host == "" {
		host = "0.0.0.0"
	}

	cfg := sshserver.Config{
		Host:            host,
		Port:            sshPort,
		IdleTimeout:     c.Duration("idle-timeout"),
		MaxTimeout:      c.Duration("max-timeout"),
		AgentForwarding: true,
		SessionHandler:  sshserver.ShellHandler(),
	}

	if err := cfg.Validate(); err != nil {
		return err
	}

	srv, err := sshserver.New(cfg)
	if err != nil {
		return err
	}

	// Use WebSocket server for tunneling
	wsServer := proxy.NewWSServer(proxy.WSServerConfig{
		Addr:   addr,
		Dialer: &proxy.TCPDialer{Addr: fmt.Sprintf("localhost:%d", sshPort)},
		Name:   name,
	})

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 2)
	go func() {
		errCh <- srv.Start()
	}()
	go func() {
		errCh <- wsServer.Start()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
		wsServer.Shutdown(shutdownCtx)
		return nil
	}
}
