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
var BridgeFeatureTag = ""

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
				Sources: cli.EnvVars("BRIDGE_LOG_LEVEL"),
			},
			&cli.StringSliceFlag{
				Name:    "log-paths",
				Usage:   "Additional log destinations: \"stdout\", \"stderr\", or a file path (repeatable)",
				Sources: cli.EnvVars("BRIDGE_LOG_PATHS"),
			},
		},
		Before: func(ctx context.Context, command *cli.Command) (context.Context, error) {
			level := parseLogLevel(command.String("log-level"))
			logPaths := command.StringSlice("log-paths")
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
			Get(),
			Remove(),
			Administrator(),
			Debug(),
			Update(),
		},
	}
}

// funcValueSource is a cli.ValueSource backed by an arbitrary function.
// This lets us set a flag's default from computed values (e.g. linuxBinaryPath)
// while still allowing env-var and explicit-flag overrides.
type funcValueSource struct {
	fn func() string
}

func (f *funcValueSource) Lookup() (string, bool) {
	if v := f.fn(); v != "" {
		return v, true
	}
	return "", false
}

func (f *funcValueSource) String() string   { return "func" }
func (f *funcValueSource) GoString() string { return "&funcValueSource{}" }

// Func returns a ValueSource that calls fn to obtain a value.
func Func(fn func() string) cli.ValueSource {
	return &funcValueSource{fn: fn}
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
