package commands

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli/v3"
	"github.com/vercel/bridge/pkg/identity"
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
		},
		Before: func(ctx context.Context, command *cli.Command) (context.Context, error) {
			level := parseLogLevel(command.String("log-level"))
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
				Level:     level,
				AddSource: true,
				ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
					if a.Key == slog.SourceKey {
						if src, ok := a.Value.Any().(*slog.Source); ok {
							dir := filepath.Base(filepath.Dir(src.File))
							file := filepath.Base(src.File)
							a.Value = slog.StringValue(fmt.Sprintf("%s/%s:%d", dir, file, src.Line))
						}
					}
					return a
				},
			})))

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
			Connect(),
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
