package commands

import (
	"context"
	"log/slog"
	"runtime"
	"strings"

	"github.com/urfave/cli/v3"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/identity"
	"github.com/vercel/bridge/pkg/interact"
	"github.com/vercel/bridge/pkg/logging"
)

var Version = "dev"
var BridgeFeatureTag = ""

func rootUsageText() string {
	if interact.IsJSON() {
		return `bridge <command> [flags]

Example:
  bridge create my-api && bridge exec my-api -- npm test

JSON output (--output=json):
  All commands emit a CommandResult envelope to stdout:

    {"error": "", "response": { ... }}

  On success, "error" is empty and "response" contains the command payload.
  On failure, "error" has the message and "response" may be absent.

  Run "bridge schema command-result" for the envelope schema. Each command
  has its own response schema (e.g. "bridge schema create-response").`
	}
	return `bridge <command> [flags]

Example:
  bridge create -c my-api -n production`
}

// NewApp returns the root CLI command with all subcommands registered.
func NewApp() *cli.Command {
	return &cli.Command{
		Name:      "bridge",
		Usage:     "Develop locally with the network, DNS, IAM, and environment of a Kubernetes deployment",
		UsageText: rootUsageText(),
		Version:   Version,
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
			&cli.StringFlag{
				Name:    "output",
				Usage:   "Output format: \"pretty\" or \"json\"",
				Value:   defaultOutputFormat(),
				Sources: cli.EnvVars("BRIDGE_OUTPUT"),
			},
			&cli.BoolFlag{
				Name:    "quiet",
				Usage:   "Suppress all log output to stdout/stderr",
				Sources: cli.EnvVars("BRIDGE_QUIET"),
			},
		},
		Before: func(ctx context.Context, command *cli.Command) (context.Context, error) {
			// Set global output format before logging setup so log paths
			// can be derived from it.
			output := command.String("output")
			interact.SetOutputFormat(output)

			level := parseLogLevel(command.String("log-level"))
			logPaths := command.StringSlice("log-paths")
			quiet := command.Bool("quiet")

			if quiet {
				// Suppress all console log output; only the rolling file remains.
				logPaths = nil
			} else if len(logPaths) == 0 && output == interact.OutputJSON {
				// json mode gets stdout + file so structured logs stream to the agent;
				// pretty mode gets only the file so the terminal stays clean.
				logPaths = []string{"stdout"}
			}

			cleanup, err := logging.Setup(level, logPaths, command.Root().Writer, command.Root().ErrWriter)
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
			ServerProxy(),
			Intercept(),
			Create(),
			Exec(),
			Get(),
			Remove(),
			Administrator(),
			Debug(),
			Update(),
			Schema(),
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

// FuncSource returns a ValueSource that calls fn to obtain a value.
func FuncSource(fn func() string) cli.ValueSource {
	return &funcValueSource{fn: fn}
}

// deviceInfo returns a DeviceInfo populated with the current OS, architecture,
// and bridge CLI version.
func deviceInfo() *bridgev1.DeviceInfo {
	return &bridgev1.DeviceInfo{
		Os:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		BridgeVersion: Version,
	}
}

// defaultOutputFormat returns "json" for agents and "pretty" for humans.
func defaultOutputFormat() string {
	if interact.IsJSON() {
		return interact.OutputJSON
	}
	return interact.OutputPretty
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
