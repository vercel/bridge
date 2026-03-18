package profile

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/urfave/cli/v3"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

const profileDir = ".bridge"
const profileFile = "profile.json"

// Find walks up from startDir looking for .bridge/profile.json.
// Returns the path to the first one found, or empty string if none exists.
func Find(startDir string) string {
	dir := startDir
	for {
		candidate := filepath.Join(dir, profileDir, profileFile)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// Load reads and parses a profile from the given path.
func Load(path string) (*bridgev1.Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read profile: %w", err)
	}
	var p bridgev1.Profile
	if err := protojson.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse profile: %w", err)
	}
	return &p, nil
}

// ApplyCreate evaluates the profile's create rules against the current
// CLI command state and sets any matched flags that weren't explicitly
// provided by the user.
func ApplyCreate(profile *bridgev1.Profile, cmd *cli.Command) error {
	if profile == nil || len(profile.Create) == 0 {
		return nil
	}

	logger := slog.With("component", "profile")

	// Build the CEL evaluation context from the current command state.
	payload := payloadFromCommand(cmd)

	for i, rule := range profile.Create {
		matched, err := evalCEL(rule.Match, payload)
		if err != nil {
			return fmt.Errorf("profile rule %d: CEL error: %w", i, err)
		}
		if !matched {
			continue
		}

		logger.Info("Profile rule matched", "index", i, "match", rule.Match)

		flags := rule.GetCommand()
		if flags == nil {
			continue
		}

		// String flags: set if not already provided.
		setIfUnset(cmd, "name", flags.Name)
		setIfUnset(cmd, "namespace", flags.Namespace)
		setIfUnset(cmd, "source", flags.Source)
		setIfUnset(cmd, "devcontainer-config", flags.DevcontainerConfig)
		setIfUnset(cmd, "devcontainer-up-args", flags.DevcontainerUpArgs)

		// Int flags: set if not already provided and non-zero.
		if flags.Listen != 0 {
			setIfUnset(cmd, "listen", fmt.Sprintf("%d", flags.Listen))
		}

		// Bool flags: set if not already provided and true.
		if flags.Connect {
			setIfUnset(cmd, "connect", "true")
		}

		// Slice flags: always append.
		for _, v := range flags.ServerFacades {
			if err := cmd.Set("server-facade", v); err != nil {
				return fmt.Errorf("profile rule %d: set server-facade: %w", i, err)
			}
		}
	}
	return nil
}

// payloadFromCommand builds a CreateCommandPayload from the current CLI state.
// Note: StringArg bindings are not populated during Before hooks, so we read
// the first positional arg from cmd.Args() directly.
func payloadFromCommand(cmd *cli.Command) *bridgev1.CreateCommandPayload {
	var name string
	if args := cmd.Args(); args != nil && args.Len() > 0 {
		name = args.First()
	}
	return &bridgev1.CreateCommandPayload{
		WorkloadName:       name,
		Name:               cmd.String("name"),
		Namespace:          cmd.String("namespace"),
		ServerFacades:      cmd.StringSlice("server-facade"),
		Source:             cmd.String("source"),
		Listen:             int32(cmd.Int("listen")),
		Connect:            cmd.Bool("connect"),
		DevcontainerConfig: cmd.String("devcontainer-config"),
		DevcontainerUpArgs: cmd.String("devcontainer-up-args"),
	}
}

// evalCEL compiles and evaluates a CEL expression against a CreateCommandPayload.
func evalCEL(expr string, payload *bridgev1.CreateCommandPayload) (bool, error) {
	env, err := cel.NewEnv(
		cel.Types(payload),
		cel.Variable("payload", cel.ObjectType("bridge.v1.CreateCommandPayload")),
	)
	if err != nil {
		return false, fmt.Errorf("create CEL env: %w", err)
	}

	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return false, fmt.Errorf("compile CEL %q: %w", expr, issues.Err())
	}

	prg, err := env.Program(ast)
	if err != nil {
		return false, fmt.Errorf("program CEL: %w", err)
	}

	out, _, err := prg.Eval(map[string]any{
		"payload": payload,
	})
	if err != nil {
		return false, fmt.Errorf("eval CEL: %w", err)
	}
	if out.Type() != types.BoolType {
		return false, fmt.Errorf("CEL expression returned %s, want bool", out.Type())
	}
	return out.Value().(bool), nil
}

// setIfUnset calls cmd.Set only if the flag hasn't been set and the value is non-empty.
func setIfUnset(cmd *cli.Command, name, value string) {
	if value == "" || cmd.IsSet(name) {
		return
	}
	if err := cmd.Set(name, value); err != nil {
		slog.Warn("Profile: failed to set flag", "flag", name, "error", err)
	}
}

// ProfilePath returns the resolved profile path from a string flag or
// by searching upward from the current directory. Returns empty string
// if no profile is found.
func ProfilePath() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	return Find(dir)
}

// LoadFromFlag loads a profile from the given path, or searches upward
// from PWD if path is empty. Returns nil (no error) if no profile exists.
func LoadFromFlag(path string) (*bridgev1.Profile, error) {
	if path == "" {
		path = ProfilePath()
	}
	if path == "" {
		return nil, nil
	}

	p, err := Load(path)
	if err != nil {
		return nil, err
	}

	slog.Debug("Loaded profile", "path", path, "create_rules", len(p.GetCreate()))

	// Resolve relative paths in server_mocks relative to the profile directory.
	profileDir := filepath.Dir(filepath.Dir(path)) // .bridge/profile.json -> repo root
	for _, rule := range p.GetCreate() {
		flags := rule.GetCommand()
		if flags == nil {
			continue
		}
		for i, v := range flags.ServerFacades {
			v = strings.TrimSpace(v)
			if v == "" || strings.HasPrefix(v, "{") {
				continue
			}
			if !filepath.IsAbs(v) {
				flags.ServerFacades[i] = filepath.Join(profileDir, v)
			}
		}
	}

	return p, nil
}
