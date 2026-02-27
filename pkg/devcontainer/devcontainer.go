package devcontainer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Build holds the devcontainer build configuration.
type Build struct {
	Dockerfile string `json:"dockerfile,omitempty"`
	Context    string `json:"context,omitempty"`
}

// Config represents a devcontainer.json configuration.
// Known fields are typed; unknown fields are preserved via Overflow.
type Config struct {
	Name         string                    `json:"name,omitempty"`
	Image        string                    `json:"image,omitempty"`
	Build        *Build                    `json:"build,omitempty"`
	Features     map[string]map[string]any `json:"features,omitempty"`
	ContainerEnv map[string]string         `json:"containerEnv,omitempty"`
	RemoteEnv    map[string]string         `json:"remoteEnv,omitempty"`
	CapAdd       []string                  `json:"capAdd,omitempty"`
	Mounts       []string                  `json:"mounts,omitempty"`
	RunArgs      []string                  `json:"runArgs,omitempty"`
	AppPort      PortMappings              `json:"appPort,omitempty"`

	// Overflow holds unknown fields so we don't lose them on round-trip.
	Overflow map[string]json.RawMessage `json:"-"`
}

// knownKeys is the set of JSON keys managed by the typed fields above.
var knownKeys = map[string]bool{
	"name":         true,
	"image":        true,
	"build":        true,
	"features":     true,
	"containerEnv": true,
	"remoteEnv":    true,
	"capAdd":       true,
	"mounts":       true,
	"runArgs":      true,
	"appPort":      true,
}

// UnmarshalJSON decodes a devcontainer.json, populating typed fields and
// stashing unknown keys in Overflow.
func (c *Config) UnmarshalJSON(data []byte) error {
	// Decode typed fields via an alias to avoid infinite recursion.
	type Alias Config
	if err := json.Unmarshal(data, (*Alias)(c)); err != nil {
		return err
	}

	// Decode all keys into a generic map.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	c.Overflow = make(map[string]json.RawMessage)
	for k, v := range raw {
		if !knownKeys[k] {
			c.Overflow[k] = v
		}
	}
	return nil
}

// MarshalJSON encodes the Config, merging typed fields with any overflow.
func (c Config) MarshalJSON() ([]byte, error) {
	type Alias Config
	typed, err := json.Marshal(Alias(c))
	if err != nil {
		return nil, err
	}

	if len(c.Overflow) == 0 {
		return typed, nil
	}

	// Merge typed fields into overflow map, giving typed fields precedence.
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(typed, &merged); err != nil {
		return nil, err
	}

	for k, v := range c.Overflow {
		if _, exists := merged[k]; !exists {
			merged[k] = v
		}
	}

	return json.Marshal(merged)
}

// ResolveConfigPath finds a devcontainer.json config file. If explicit is
// non-empty it is used directly; otherwise it probes the standard locations:
// .devcontainer/devcontainer.json then .devcontainer.json.
func ResolveConfigPath(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("devcontainer config not found at %s: %w", explicit, err)
		}
		return explicit, nil
	}

	candidates := []string{
		filepath.Join(".devcontainer", "devcontainer.json"),
		".devcontainer.json",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	return "", fmt.Errorf("no devcontainer.json found\n\nA devcontainer.json file is required. " +
		"See https://containers.dev/implementors/json_reference/ to create one")
}

// Load reads and parses a devcontainer.json from the given path.
// Returns an error if the file cannot be read or parsed.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read devcontainer config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse devcontainer config: %w", err)
	}
	return &cfg, nil
}

// Save writes the Config as indented JSON to the given path.
func (c *Config) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal devcontainer config: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write devcontainer config: %w", err)
	}
	return nil
}

// SetFeature adds or updates a feature entry.
func (c *Config) SetFeature(ref string, opts map[string]any) {
	if c.Features == nil {
		c.Features = make(map[string]map[string]any)
	}
	c.Features[ref] = opts
}

// SetMount appends a mount entry to the Mounts slice.
func (c *Config) SetMount(mount string) {
	c.Mounts = append(c.Mounts, mount)
}

// EnsureContainerEnv sets a key in ContainerEnv, creating the map if needed.
func (c *Config) EnsureContainerEnv(k, v string) {
	if c.ContainerEnv == nil {
		c.ContainerEnv = make(map[string]string)
	}
	c.ContainerEnv[k] = v
}

// EnsureRunArgs appends additional docker run arguments.
func (c *Config) EnsureRunArgs(args ...string) {
	c.RunArgs = append(c.RunArgs, args...)
}

// RebaseBuildPaths adjusts the relative "dockerfile" and "context" paths inside
// the Build field so they remain correct when the config is saved to a
// different directory. origDir is the directory containing the original config;
// newDir is where the config will be written.
func (c *Config) RebaseBuildPaths(origDir, newDir string) {
	if c.Build == nil {
		return
	}

	rebase := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		abs := filepath.Clean(filepath.Join(origDir, p))
		rel, err := filepath.Rel(newDir, abs)
		if err != nil {
			return p
		}
		return filepath.Clean(rel)
	}

	c.Build.Dockerfile = rebase(c.Build.Dockerfile)
	c.Build.Context = rebase(c.Build.Context)
}

// PortMapping describes a host-to-container port mapping from the appPort field.
type PortMapping struct {
	HostPort      int
	ContainerPort int
}

// PortMappings is a slice of PortMapping with custom JSON handling for the
// devcontainer spec's polymorphic appPort field (single value or array).
type PortMappings []PortMapping

// UnmarshalJSON decodes appPort from a single value or an array.
func (pm *PortMappings) UnmarshalJSON(data []byte) error {
	// Try as array first.
	var arr []json.RawMessage
	if err := json.Unmarshal(data, &arr); err == nil {
		result := make(PortMappings, 0, len(arr))
		for _, elem := range arr {
			m, err := parsePortValue(elem)
			if err != nil {
				return err
			}
			result = append(result, m)
		}
		*pm = result
		return nil
	}

	// Single value.
	m, err := parsePortValue(data)
	if err != nil {
		return err
	}
	*pm = PortMappings{m}
	return nil
}

// MarshalJSON encodes PortMappings as a JSON array.
func (pm PortMappings) MarshalJSON() ([]byte, error) {
	return json.Marshal([]PortMapping(pm))
}

// parsePortValue parses a single appPort value (int or "host:container" string)
// into a PortMapping. A bare port sets both host and container to the same value.
func parsePortValue(raw json.RawMessage) (PortMapping, error) {
	// Try int.
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return PortMapping{HostPort: n, ContainerPort: n}, nil
	}

	// Try string.
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return PortMapping{}, fmt.Errorf("appPort element must be int or string: %s", string(raw))
	}

	if parts := strings.SplitN(s, ":", 2); len(parts) == 2 {
		h, err1 := strconv.Atoi(parts[0])
		c, err2 := strconv.Atoi(parts[1])
		if err1 == nil && err2 == nil {
			return PortMapping{HostPort: h, ContainerPort: c}, nil
		}
	}

	p, err := strconv.Atoi(s)
	if err != nil {
		return PortMapping{}, fmt.Errorf("appPort: invalid port value %q", s)
	}
	return PortMapping{HostPort: p, ContainerPort: p}, nil
}

// UnmarshalJSON decodes the polymorphic appPort field (int, string, or array).
func (m *PortMapping) UnmarshalJSON(data []byte) error {
	pm, err := parsePortValue(data)
	if err != nil {
		return err
	}
	*m = pm
	return nil
}

// MarshalJSON encodes a PortMapping as "hostPort:containerPort".
func (m PortMapping) MarshalJSON() ([]byte, error) {
	return json.Marshal(fmt.Sprintf("%d:%d", m.HostPort, m.ContainerPort))
}

// EnsureCapAdd idempotently adds capabilities to capAdd.
func (c *Config) EnsureCapAdd(caps ...string) {
	existing := make(map[string]bool, len(c.CapAdd))
	for _, cap := range c.CapAdd {
		existing[cap] = true
	}
	for _, cap := range caps {
		if !existing[cap] {
			c.CapAdd = append(c.CapAdd, cap)
			existing[cap] = true
		}
	}
}
