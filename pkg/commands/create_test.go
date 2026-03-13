package commands

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vercel/bridge/pkg/devcontainer"
	"github.com/vercel/bridge/pkg/netutil"
)

func TestResolveAppPorts_FreePorts(t *testing.T) {
	// Pick ports we know are free right now.
	freePort1, err := netutil.FindFreePort()
	if err != nil {
		t.Fatalf("FindFreePort: %v", err)
	}
	freePort2, err := netutil.FindFreePort()
	if err != nil {
		t.Fatalf("FindFreePort: %v", err)
	}

	cfg := &devcontainer.Config{
		AppPort: []devcontainer.PortMapping{
			{HostPort: freePort1, ContainerPort: 3000},
			{HostPort: freePort2, ContainerPort: 4000},
		},
	}
	resolveAppPorts(cfg)
	// When ports are free, host ports should stay the same.
	if cfg.AppPort[0].HostPort != freePort1 {
		t.Errorf("appPort[0].HostPort = %d, want %d", cfg.AppPort[0].HostPort, freePort1)
	}
	if cfg.AppPort[1].HostPort != freePort2 {
		t.Errorf("appPort[1].HostPort = %d, want %d", cfg.AppPort[1].HostPort, freePort2)
	}
	// Container ports must always be preserved.
	if cfg.AppPort[0].ContainerPort != 3000 {
		t.Errorf("appPort[0].ContainerPort = %d, want 3000", cfg.AppPort[0].ContainerPort)
	}
	if cfg.AppPort[1].ContainerPort != 4000 {
		t.Errorf("appPort[1].ContainerPort = %d, want 4000", cfg.AppPort[1].ContainerPort)
	}
}

func TestResolveAppPorts_ConflictRemaps(t *testing.T) {
	// Occupy a port on 127.0.0.1 so resolveAppPorts must remap.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	occupiedPort := ln.Addr().(*net.TCPAddr).Port

	cfg := &devcontainer.Config{
		AppPort: []devcontainer.PortMapping{
			{HostPort: occupiedPort, ContainerPort: occupiedPort},
		},
	}
	resolveAppPorts(cfg)
	if cfg.AppPort[0].HostPort == occupiedPort {
		t.Errorf("expected host port to be remapped from %d", occupiedPort)
	}
	if cfg.AppPort[0].ContainerPort != occupiedPort {
		t.Errorf("container port should remain %d, got %d", occupiedPort, cfg.AppPort[0].ContainerPort)
	}
}

func TestResolveAppPorts_CrossCheck(t *testing.T) {
	// Two entries requesting the same host port must resolve to different ports.
	freePort, err := netutil.FindFreePort()
	if err != nil {
		t.Fatalf("FindFreePort: %v", err)
	}

	cfg := &devcontainer.Config{
		AppPort: []devcontainer.PortMapping{
			{HostPort: freePort, ContainerPort: 3000},
			{HostPort: freePort, ContainerPort: 4000},
		},
	}
	resolveAppPorts(cfg)
	if cfg.AppPort[0].HostPort == cfg.AppPort[1].HostPort {
		t.Errorf("both appPorts resolved to the same host port %d", cfg.AppPort[0].HostPort)
	}
	// Container ports must be preserved.
	if cfg.AppPort[0].ContainerPort != 3000 {
		t.Errorf("appPort[0].ContainerPort = %d, want 3000", cfg.AppPort[0].ContainerPort)
	}
	if cfg.AppPort[1].ContainerPort != 4000 {
		t.Errorf("appPort[1].ContainerPort = %d, want 4000", cfg.AppPort[1].ContainerPort)
	}
}

func TestResolveAppPorts_Empty(t *testing.T) {
	cfg := &devcontainer.Config{}
	resolveAppPorts(cfg) // should not panic
	if len(cfg.AppPort) != 0 {
		t.Errorf("expected empty appPort, got %v", cfg.AppPort)
	}
}

func TestRenderStageManifests(t *testing.T) {
	t.Run("unsupported stage returns error", func(t *testing.T) {
		_, err := renderStageManifests(context.Background(), "my-service", "production")
		assert.ErrorContains(t, err, "unsupported stage")
	})

	t.Run("empty service name returns error", func(t *testing.T) {
		_, err := renderStageManifests(context.Background(), "", "staging")
		assert.ErrorContains(t, err, "service name")
	})
}

func TestIsStageSource(t *testing.T) {
	// Stage names: not a filesystem path, no glob chars
	assert.True(t, isStageSource("staging"))

	// Filesystem paths: directories, files, globs
	dir := t.TempDir()
	assert.False(t, isStageSource(dir))

	f := filepath.Join(dir, "app.yaml")
	require.NoError(t, os.WriteFile(f, []byte("test"), 0644))
	assert.False(t, isStageSource(f))

	assert.False(t, isStageSource("_infra/*.yaml"))
	assert.False(t, isStageSource("manifests[0].yaml"))
	assert.False(t, isStageSource("dir?/app.yaml"))
}

func TestWriteEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.env")

	vars := map[string]string{
		"SIMPLE":        "value",
		"WITH_SPACES":   `["East US"]`,
		"WITH_QUOTES":   `say "hello"`,
		"WITH_NEWLINE":  "line1\nline2",
		"EMPTY":         "",
		"HASH_IN_VALUE": "before#after",
	}

	if err := writeEnvFile(path, vars); err != nil {
		t.Fatalf("writeEnvFile: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	content := string(data)

	// Docker --env-file: everything after '=' is the literal value, no quoting.
	expected := map[string]string{
		"SIMPLE":        "value",
		"WITH_SPACES":   `["East US"]`,
		"WITH_QUOTES":   `say "hello"`,
		"WITH_NEWLINE":  "line1 line2",
		"EMPTY":         "",
		"HASH_IN_VALUE": "before#after",
	}

	for k, want := range expected {
		line := k + "=" + want
		if !strings.Contains(content, line+"\n") {
			t.Errorf("missing or incorrect line for %s\nwant: %s\ngot file:\n%s", k, line, content)
		}
	}
}
