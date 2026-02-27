package devcontainer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTrip_PreservesUnknownFields(t *testing.T) {
	input := `{
  "name": "my-dev",
  "image": "ubuntu:22.04",
  "features": {
    "ghcr.io/example/feat:1": {
      "key": "val"
    }
  },
  "capAdd": ["NET_ADMIN"],
  "build": {"dockerfile": "Dockerfile"},
  "mounts": ["source=vol,target=/data,type=volume"],
  "postCreateCommand": "echo hello"
}`

	var cfg Config
	if err := json.Unmarshal([]byte(input), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if cfg.Name != "my-dev" {
		t.Errorf("name = %q, want %q", cfg.Name, "my-dev")
	}
	if cfg.Image != "ubuntu:22.04" {
		t.Errorf("image = %q, want %q", cfg.Image, "ubuntu:22.04")
	}
	if len(cfg.CapAdd) != 1 || cfg.CapAdd[0] != "NET_ADMIN" {
		t.Errorf("capAdd = %v, want [NET_ADMIN]", cfg.CapAdd)
	}

	// mounts is now a typed field.
	if len(cfg.Mounts) != 1 || cfg.Mounts[0] != "source=vol,target=/data,type=volume" {
		t.Errorf("mounts = %v, want [source=vol,target=/data,type=volume]", cfg.Mounts)
	}

	// build is now a typed field.
	if cfg.Build == nil || cfg.Build.Dockerfile != "Dockerfile" {
		t.Errorf("build.dockerfile = %v, want Dockerfile", cfg.Build)
	}

	// Overflow must contain the unknown keys.
	if _, ok := cfg.Overflow["postCreateCommand"]; !ok {
		t.Errorf("overflow missing key %q", "postCreateCommand")
	}

	// Round-trip marshal.
	out, err := json.MarshalIndent(&cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Re-parse and verify everything survived.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}

	for _, key := range []string{"name", "image", "features", "capAdd", "build", "mounts", "postCreateCommand"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("round-tripped JSON missing key %q", key)
		}
	}
}

func TestSetFeature(t *testing.T) {
	cfg := &Config{}
	cfg.SetFeature("ghcr.io/example/feat:1", map[string]any{"version": "1.0"})

	if len(cfg.Features) != 1 {
		t.Fatalf("features len = %d, want 1", len(cfg.Features))
	}
	opts := cfg.Features["ghcr.io/example/feat:1"]
	if opts["version"] != "1.0" {
		t.Errorf("version = %v, want 1.0", opts["version"])
	}

	// Overwrite
	cfg.SetFeature("ghcr.io/example/feat:1", map[string]any{"version": "2.0"})
	if cfg.Features["ghcr.io/example/feat:1"]["version"] != "2.0" {
		t.Errorf("version after overwrite = %v, want 2.0", cfg.Features["ghcr.io/example/feat:1"]["version"])
	}
}

func TestEnsureCapAdd_Idempotent(t *testing.T) {
	cfg := &Config{CapAdd: []string{"NET_ADMIN"}}
	cfg.EnsureCapAdd("NET_ADMIN", "SYS_PTRACE")

	if len(cfg.CapAdd) != 2 {
		t.Fatalf("capAdd len = %d, want 2", len(cfg.CapAdd))
	}

	cfg.EnsureCapAdd("NET_ADMIN")
	if len(cfg.CapAdd) != 2 {
		t.Errorf("capAdd len after re-add = %d, want 2", len(cfg.CapAdd))
	}
}

func TestLoadSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devcontainer.json")

	original := &Config{
		Name:  "test",
		Image: "alpine:3",
	}
	original.SetFeature("ghcr.io/x/y:1", map[string]any{"a": "b"})
	original.EnsureCapAdd("NET_ADMIN")

	if err := original.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if loaded.Name != "test" {
		t.Errorf("name = %q, want %q", loaded.Name, "test")
	}
	if loaded.Image != "alpine:3" {
		t.Errorf("image = %q, want %q", loaded.Image, "alpine:3")
	}
	if len(loaded.Features) != 1 {
		t.Errorf("features len = %d, want 1", len(loaded.Features))
	}
	if len(loaded.CapAdd) != 1 || loaded.CapAdd[0] != "NET_ADMIN" {
		t.Errorf("capAdd = %v, want [NET_ADMIN]", loaded.CapAdd)
	}
}

func TestLoad_NonExistent(t *testing.T) {
	_, err := Load("/nonexistent/path/devcontainer.json")
	if err == nil {
		t.Error("expected error for non-existent file")
	}
}

func TestLoad_ExistingThenModify(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devcontainer.json")

	// Write a config with extra fields
	existing := `{
  "name": "existing",
  "postCreateCommand": "npm install",
  "capAdd": ["SYS_PTRACE"]
}`
	if err := os.WriteFile(path, []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Modify
	cfg.Name = "updated"
	cfg.SetFeature("ghcr.io/x/feat:1", map[string]any{"v": "1"})
	cfg.EnsureCapAdd("NET_ADMIN")

	if err := cfg.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Reload and verify
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	if reloaded.Name != "updated" {
		t.Errorf("name = %q, want %q", reloaded.Name, "updated")
	}
	if len(reloaded.CapAdd) != 2 {
		t.Errorf("capAdd = %v, want [SYS_PTRACE NET_ADMIN]", reloaded.CapAdd)
	}

	// postCreateCommand must survive
	if _, ok := reloaded.Overflow["postCreateCommand"]; !ok {
		t.Error("postCreateCommand lost on round-trip")
	}
}

func TestRebaseBuildPaths(t *testing.T) {
	input := `{
  "name": "test",
  "build": {
    "dockerfile": "../Dockerfile",
    "context": ".."
  }
}`
	var cfg Config
	if err := json.Unmarshal([]byte(input), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Simulate moving from .devcontainer/ to .devcontainer/bridge-foo/
	cfg.RebaseBuildPaths(".devcontainer", filepath.Join(".devcontainer", "bridge-foo"))

	wantDockerfile := filepath.Join("..", "..", "Dockerfile")
	if cfg.Build.Dockerfile != wantDockerfile {
		t.Errorf("dockerfile = %q, want %q", cfg.Build.Dockerfile, wantDockerfile)
	}
	wantContext := filepath.Join("..", "..")
	if cfg.Build.Context != wantContext {
		t.Errorf("context = %q, want %q", cfg.Build.Context, wantContext)
	}
}

func TestRebaseBuildPaths_NoBuild(t *testing.T) {
	cfg := &Config{}
	// Should not panic when there's no build field.
	cfg.RebaseBuildPaths(".devcontainer", filepath.Join(".devcontainer", "bridge-foo"))
}

func TestRebaseBuildPaths_AbsolutePaths(t *testing.T) {
	input := `{
  "build": {
    "dockerfile": "/absolute/Dockerfile",
    "context": "/absolute/dir"
  }
}`
	var cfg Config
	if err := json.Unmarshal([]byte(input), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	cfg.RebaseBuildPaths(".devcontainer", filepath.Join(".devcontainer", "bridge-foo"))

	if cfg.Build.Dockerfile != "/absolute/Dockerfile" {
		t.Errorf("dockerfile = %q, want /absolute/Dockerfile", cfg.Build.Dockerfile)
	}
	if cfg.Build.Context != "/absolute/dir" {
		t.Errorf("context = %q, want /absolute/dir", cfg.Build.Context)
	}
}

func TestAppPort_UnmarshalSingleInt(t *testing.T) {
	input := `{"appPort": 3000}`
	var cfg Config
	if err := json.Unmarshal([]byte(input), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(cfg.AppPort) != 1 {
		t.Fatalf("appPort len = %d, want 1", len(cfg.AppPort))
	}
	if cfg.AppPort[0].HostPort != 3000 || cfg.AppPort[0].ContainerPort != 3000 {
		t.Errorf("appPort[0] = %+v, want {3000, 3000}", cfg.AppPort[0])
	}
}

func TestAppPort_UnmarshalSingleString(t *testing.T) {
	input := `{"appPort": "8080:3000"}`
	var cfg Config
	if err := json.Unmarshal([]byte(input), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(cfg.AppPort) != 1 {
		t.Fatalf("appPort len = %d, want 1", len(cfg.AppPort))
	}
	if cfg.AppPort[0].HostPort != 8080 || cfg.AppPort[0].ContainerPort != 3000 {
		t.Errorf("appPort[0] = %+v, want {8080, 3000}", cfg.AppPort[0])
	}
}

func TestAppPort_UnmarshalArray(t *testing.T) {
	input := `{"appPort": [3000, "8080:4000", 5000]}`
	var cfg Config
	if err := json.Unmarshal([]byte(input), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []PortMapping{
		{HostPort: 3000, ContainerPort: 3000},
		{HostPort: 8080, ContainerPort: 4000},
		{HostPort: 5000, ContainerPort: 5000},
	}
	if len(cfg.AppPort) != len(want) {
		t.Fatalf("appPort len = %d, want %d", len(cfg.AppPort), len(want))
	}
	for i, w := range want {
		if cfg.AppPort[i] != w {
			t.Errorf("appPort[%d] = %+v, want %+v", i, cfg.AppPort[i], w)
		}
	}
}

func TestAppPort_MarshalJSON(t *testing.T) {
	cfg := &Config{
		Name: "test",
		AppPort: []PortMapping{
			{HostPort: 3000, ContainerPort: 3000},
			{HostPort: 8080, ContainerPort: 4000},
		},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if _, ok := raw["appPort"]; !ok {
		t.Error("marshalled JSON missing appPort key")
	}
}

func TestAppPort_OmittedWhenEmpty(t *testing.T) {
	cfg := &Config{Name: "test"}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if _, ok := raw["appPort"]; ok {
		t.Error("appPort should be omitted when empty")
	}
}

func TestAppPort_RoundTrip(t *testing.T) {
	input := `{"name": "test", "appPort": [3000, "9090:8080"]}`
	var cfg Config
	if err := json.Unmarshal([]byte(input), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	data, err := json.Marshal(&cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var cfg2 Config
	if err := json.Unmarshal(data, &cfg2); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if len(cfg2.AppPort) != 2 {
		t.Fatalf("appPort len = %d, want 2", len(cfg2.AppPort))
	}
	want0 := PortMapping{HostPort: 3000, ContainerPort: 3000}
	want1 := PortMapping{HostPort: 9090, ContainerPort: 8080}
	if cfg2.AppPort[0] != want0 {
		t.Errorf("appPort[0] = %+v, want %+v", cfg2.AppPort[0], want0)
	}
	if cfg2.AppPort[1] != want1 {
		t.Errorf("appPort[1] = %+v, want %+v", cfg2.AppPort[1], want1)
	}
}
