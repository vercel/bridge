package commands

import (
	"context"
	"log/slog"
	"strings"

	"github.com/urfave/cli/v3"
	"github.com/vercel/bridge/pkg/identity"
	"github.com/vercel/bridge/pkg/logging"
)

var Version = "dev"

// NewApp returns the root CLI command with all subcommands registered.
func NewApp() *cli.Command {
	return &cli.Command{
		Name:    "bridge",
		Usage:   "Bridge CLI",
		Version: Version,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "log-level",
				Usage:   "Log level (debug, info, warn, error)",
				Value:   "info",
				Sources: cli.EnvVars("LOG_LEVEL"),
			},
			&cli.StringSliceFlag{
				Name:    "log-path",
				Usage:   "Additional log destinations: \"stdout\", \"stderr\", or a file path (repeatable)",
				Sources: cli.EnvVars("LOG_PATH"),
			},
		},
		Before: func(ctx context.Context, command *cli.Command) (context.Context, error) {
			level := parseLogLevel(command.String("log-level"))
			logPaths := command.StringSlice("log-path")
			cleanup, err := logging.Setup(level, logPaths)
			if err != nil {
				slog.Warn("Failed to set up log file", "error", err)
			}
			_ = cleanup

			// Ensure device identity exists on every command invocation.
			deviceID, err := identity.EnsureDeviceID()
			if err != nil {
				slog.Warn("Failed to ensure device identity", "error", err)
			} else {
				slog.Debug("Device identity", "device_id", deviceID)
			}

			return ctx, nil
		},
		Commands: []*cli.Command{
			Server(),
			Intercept(),
			Create(),
			Administrator(),
		},
	}
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
