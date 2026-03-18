package commands

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/proxy"
	"google.golang.org/protobuf/encoding/protojson"
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
			&cli.StringSliceFlag{
				Name:    "server-facades",
				Usage:   "Server facade spec (JSON string or file path). May be repeated.",
				Sources: cli.EnvVars("BRIDGE_SERVER_FACADES"),
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

	// Parse server facade specs.
	var facades []*bridgev1.ServerFacade
	for _, val := range c.StringSlice("server-facades") {
		f, err := parseServerFacade(val)
		if err != nil {
			return fmt.Errorf("invalid server facade spec: %w", err)
		}
		facades = append(facades, f)
	}

	grpcServer := proxy.NewGRPCServer(addr, listenPorts, facades)

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		errCh <- grpcServer.Start()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		grpcServer.Shutdown(shutdownCtx)
		return nil
	}
}

// parseServerFacade parses a server facade spec from a JSON string or file path.
func parseServerFacade(val string) (*bridgev1.ServerFacade, error) {
	data := []byte(val)
	if !strings.HasPrefix(strings.TrimSpace(val), "{") {
		var err error
		data, err = os.ReadFile(val)
		if err != nil {
			return nil, fmt.Errorf("read server facade file %q: %w", val, err)
		}
	}
	var f bridgev1.ServerFacade
	if err := protojson.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse server facade JSON: %w", err)
	}
	return &f, nil
}
