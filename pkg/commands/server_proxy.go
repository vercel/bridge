package commands

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"
	"github.com/vercel/bridge/pkg/proxy"
)

func ServerProxy() *cli.Command {
	return &cli.Command{
		Name:   "server-proxy",
		Usage:  "Start a forwarding proxy that relays BridgeProxyService calls to an upstream bridge server",
		Hidden: true,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "addr",
				Usage:   "Address to listen on for interceptor connections",
				Value:   ":9090",
				Sources: cli.EnvVars("BRIDGE_ADDR"),
			},
			&cli.StringFlag{
				Name:    "protocol",
				Usage:   "Protocol to serve: grpc or http",
				Value:   "grpc",
				Sources: cli.EnvVars("BRIDGE_PROTOCOL"),
			},
			&cli.StringFlag{
				Name:     "upstream-server-addr",
				Usage:    "Address of the upstream bridge server (e.g. https://my-app.vercel.app)",
				Sources:  cli.EnvVars("BRIDGE_UPSTREAM_SERVER_ADDR"),
				Required: true,
			},
			&cli.StringFlag{
				Name:    "upstream-server-protocol",
				Usage:   "Protocol to connect to the upstream: grpc or http",
				Value:   "http",
				Sources: cli.EnvVars("BRIDGE_UPSTREAM_SERVER_PROTOCOL"),
			},
		},
		Action: runServerProxy,
	}
}

func runServerProxy(ctx context.Context, c *cli.Command) error {
	addr := c.String("addr")
	protocol := c.String("protocol")
	upstreamAddr := c.String("upstream-server-addr")
	upstreamProtocol := c.String("upstream-server-protocol")

	fwd, err := proxy.NewForwarder(upstreamAddr, upstreamProtocol)
	if err != nil {
		return fmt.Errorf("failed to create forwarder: %w", err)
	}

	var srv proxy.Server
	switch protocol {
	case "grpc":
		srv = proxy.NewForwarderGRPCServer(addr, fwd)
	case "http":
		srv = proxy.NewForwarderHTTPServer(addr, fwd)
	default:
		return fmt.Errorf("unsupported protocol %q (use grpc or http)", protocol)
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
		return nil
	}
}
