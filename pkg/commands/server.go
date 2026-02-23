package commands

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"
	"github.com/vercel/bridge/pkg/mutagen"
	"github.com/vercel/bridge/pkg/proxy"
	"github.com/vercel/bridge/pkg/sshserver"
)

func Server() *cli.Command {
	return &cli.Command{
		Name:   "server",
		Usage:  "Start the bridge gRPC proxy server",
		Hidden: true,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "addr",
				Usage:   "Address to bind the server to",
				Value:   ":9090",
				Sources: cli.EnvVars("BRIDGE_ADDR"),
			},
			&cli.StringSliceFlag{
				Name:    "listen-ports",
				Aliases: []string{"l"},
				Usage:   `L4 port specs for ingress listeners (e.g. "8080/tcp", "9090/udp", "8080")`,
				Sources: cli.EnvVars("BRIDGE_LISTEN_PORTS"),
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
	addr := c.String("addr")

	// Parse listen-ports flag.
	var listenPorts []proxy.ListenPort
	for _, spec := range c.StringSlice("listen-ports") {
		for _, part := range strings.Split(spec, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			lp, err := proxy.ParseListenPort(part)
			if err != nil {
				return fmt.Errorf("invalid listen-port %q: %w", part, err)
			}
			listenPorts = append(listenPorts, lp)
		}
	}

	// Install mutagen agent if not already installed
	// This is needed for file sync between devcontainer and sandbox
	if err := mutagen.InstallAgent(); err != nil {
		slog.Warn("Failed to install mutagen agent", "error", err)
	} else {
		slog.Info("Mutagen agent ready", "path", mutagen.AgentPath())
	}

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

	// Start gRPC proxy server
	grpcServer := proxy.NewGRPCServer(addr, listenPorts)

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 2)
	go func() {
		errCh <- srv.Start()
	}()
	go func() {
		errCh <- grpcServer.Start()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
		grpcServer.Shutdown(shutdownCtx)
		return nil
	}
}
