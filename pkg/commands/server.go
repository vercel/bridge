package commands

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/fsmount"
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
			&cli.StringSliceFlag{
				Name:    "mount-roots",
				Usage:   "Absolute paths to expose via the bridge filesystem service for FUSE mounting from the devcontainer.",
				Sources: cli.EnvVars("BRIDGE_MOUNT_ROOTS"),
			},
			&cli.IntFlag{
				Name:    "source-app-port",
				Usage:   "Primary application port of the source deployment. The interceptor remaps probe targets matching this port to its --app-port locally.",
				Sources: cli.EnvVars("BRIDGE_SOURCE_APP_PORT"),
			},
			&cli.StringFlag{
				Name:    "liveness-probe",
				Usage:   "Source deployment's liveness probe as protojson-encoded bridge.v1.Probe.",
				Sources: cli.EnvVars("BRIDGE_LIVENESS_PROBE"),
			},
			&cli.StringFlag{
				Name:    "readiness-probe",
				Usage:   "Source deployment's readiness probe as protojson-encoded bridge.v1.Probe.",
				Sources: cli.EnvVars("BRIDGE_READINESS_PROBE"),
			},
			&cli.StringFlag{
				Name:    "startup-probe",
				Usage:   "Source deployment's startup probe as protojson-encoded bridge.v1.Probe.",
				Sources: cli.EnvVars("BRIDGE_STARTUP_PROBE"),
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

	// Parse mount-roots (may be repeated or comma-separated via env).
	var mountRoots []string
	for _, val := range c.StringSlice("mount-roots") {
		for _, part := range strings.Split(val, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				mountRoots = append(mountRoots, part)
			}
		}
	}

	// Auto-discover webhook-injected secret mounts (EKS Pod Identity,
	// kube-api SA auto-mount, Azure Workload Identity, etc.) — they're
	// added at pod-admission time and aren't reflected in the Deployment
	// spec, so the --mount-roots argument from k8s/resources misses them.
	discovered, err := fsmount.DiscoverSecretMounts()
	if err != nil {
		slog.Warn("Failed to discover secret mounts; webhook-injected projected volumes may be inaccessible", "error", err)
	} else if len(discovered) > 0 {
		slog.Info("Discovered webhook-injected secret mounts", "paths", discovered)
		mountRoots = append(mountRoots, discovered...)
	}

	livenessProbe, err := parseProbeFlag(c.String("liveness-probe"))
	if err != nil {
		return fmt.Errorf("invalid liveness-probe: %w", err)
	}
	readinessProbe, err := parseProbeFlag(c.String("readiness-probe"))
	if err != nil {
		return fmt.Errorf("invalid readiness-probe: %w", err)
	}
	startupProbe, err := parseProbeFlag(c.String("startup-probe"))
	if err != nil {
		return fmt.Errorf("invalid startup-probe: %w", err)
	}

	grpcServer := proxy.NewGRPCServer(proxy.Config{
		Addr:           addr,
		ListenPorts:    listenPorts,
		Facades:        facades,
		MountRoots:     mountRoots,
		SourceAppPort:  int32(c.Int("source-app-port")),
		LivenessProbe:  livenessProbe,
		ReadinessProbe: readinessProbe,
		StartupProbe:   startupProbe,
	})

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

// parseProbeFlag decodes a protojson-encoded Probe. Empty input returns nil.
func parseProbeFlag(val string) (*bridgev1.Probe, error) {
	if strings.TrimSpace(val) == "" {
		return nil, nil
	}
	var p bridgev1.Probe
	if err := protojson.Unmarshal([]byte(val), &p); err != nil {
		return nil, fmt.Errorf("parse probe JSON: %w", err)
	}
	return &p, nil
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
