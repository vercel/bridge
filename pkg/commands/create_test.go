package commands

import (
	"net"
	"testing"

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

func TestResolveAppPorts_Empty(t *testing.T) {
	cfg := &devcontainer.Config{}
	resolveAppPorts(cfg) // should not panic
	if len(cfg.AppPort) != 0 {
		t.Errorf("expected empty appPort, got %v", cfg.AppPort)
	}
}
