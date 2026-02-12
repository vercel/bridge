package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"
)

// Environment holds all containers for a full bridge E2E test.
// It wires up a sandbox (bridge server), dispatcher, and devcontainer (bridge intercept)
// so that requests to the dispatcher flow through the sandbox tunnel to the devcontainer app.
type Environment struct {
	Network      *TestNetwork
	Sandbox      *SandboxContainer
	Dispatcher   *DispatcherContainer
	Devcontainer *DevcontainerContainer

	// DispatcherHosts maps custom hostnames to their generated 192.x.x.x IPs.
	// These hostnames are added to the dispatcher's /etc/hosts so they only
	// resolve inside the dispatcher container.
	DispatcherHosts map[string]string
}

// EnvironmentConfig configures the E2E environment.
type EnvironmentConfig struct {
	// SandboxName is the name for the sandbox (default: "test-sandbox")
	SandboxName string
	// AppPort is the local app port in the devcontainer that receives inbound requests (default: 3000)
	AppPort int
	// DevcontainerPrivileged runs the devcontainer in privileged mode for iptables (default: false)
	DevcontainerPrivileged bool
	// ForwardDomains is a list of domain patterns to intercept via DNS (e.g., "*.example.com")
	ForwardDomains []string
	// DispatcherHosts is a list of custom hostnames to add to the dispatcher's
	// /etc/hosts. Each hostname is assigned a random 192.x.x.x IP. These
	// hostnames only resolve inside the dispatcher container.
	DispatcherHosts []string
}

func (cfg *EnvironmentConfig) setDefaults() {
	if cfg.SandboxName == "" {
		cfg.SandboxName = "test-sandbox"
	}
	if cfg.AppPort == 0 {
		cfg.AppPort = 3000
	}
}

const (
	// Network aliases used for container-to-container communication.
	// These allow all three containers to start in parallel since they
	// don't need to wait for each other's IPs.
	sandboxAlias    = "sandbox"
	dispatcherAlias = "dispatcher"
)

// SetupEnvironment creates the full bridge environment:
//  1. A Docker network for container-to-container communication
//  2. In parallel: a sandbox, dispatcher, and devcontainer
//  3. Bridge intercept running inside the devcontainer
//
// The caller must call TearDown when done.
func SetupEnvironment(ctx context.Context, cfg EnvironmentConfig) (*Environment, error) {
	cfg.setDefaults()

	env := &Environment{}

	// 1. Create a Docker network with a unique name so multiple environments
	// can run in parallel without colliding.
	id := make([]byte, 4)
	_, _ = rand.Read(id)
	networkName := fmt.Sprintf("bridge-e2e-%s", hex.EncodeToString(id))

	network, err := NewTestNetwork(ctx, networkName)
	if err != nil {
		return nil, fmt.Errorf("failed to create network: %w", err)
	}
	env.Network = network

	// Generate random 192.x.x.x IPs for custom dispatcher hostnames.
	if len(cfg.DispatcherHosts) > 0 {
		env.DispatcherHosts = make(map[string]string, len(cfg.DispatcherHosts))
		for _, hostname := range cfg.DispatcherHosts {
			env.DispatcherHosts[hostname] = randomPrivateIP()
		}
	}

	// 2. Start sandbox and dispatcher in parallel.
	// Network aliases let the dispatcher reference the sandbox by hostname.
	sandboxURL := fmt.Sprintf("http://%s:3000", sandboxAlias)
	functionURL := fmt.Sprintf("http://%s:8080", dispatcherAlias)

	var (
		wg          sync.WaitGroup
		sandboxErr  error
		dispatchErr error
	)

	wg.Add(2)

	go func() {
		defer wg.Done()
		sandbox, err := NewSandbox(ctx, SandboxConfig{
			Command:        []string{"bridge", "server", "--name", cfg.SandboxName},
			Network:        network.Name,
			NetworkAliases: []string{sandboxAlias},
		})
		if err != nil {
			sandboxErr = fmt.Errorf("failed to start sandbox: %w", err)
			return
		}
		env.Sandbox = sandbox
	}()

	go func() {
		defer wg.Done()
		dispatcherCfg := DispatcherConfig{
			Env: map[string]string{
				"BRIDGE_SERVER_ADDR": sandboxURL,
			},
			Network:        network.Name,
			NetworkAliases: []string{dispatcherAlias},
		}
		for hostname, ip := range env.DispatcherHosts {
			dispatcherCfg.ExtraHosts = append(dispatcherCfg.ExtraHosts,
				fmt.Sprintf("%s:%s", hostname, ip))
		}
		dispatcher, err := NewDispatcher(ctx, dispatcherCfg)
		if err != nil {
			dispatchErr = fmt.Errorf("failed to start dispatcher: %w", err)
			return
		}
		env.Dispatcher = dispatcher
	}()

	wg.Wait()

	if sandboxErr != nil || dispatchErr != nil {
		env.TearDown(ctx, nil)
		return nil, fmt.Errorf("container startup failed: sandbox=%v, dispatcher=%v",
			sandboxErr, dispatchErr)
	}

	// 3. Start devcontainer now that sandbox and dispatcher are healthy.
	// Bridge intercept needs the sandbox up for the SSH proxy and mutagen sync.
	devcontainer, err := NewDevcontainer(ctx, DevcontainerConfig{
		Network:    network.Name,
		Privileged: cfg.DevcontainerPrivileged,
	})
	if err != nil {
		env.TearDown(ctx, nil)
		return nil, fmt.Errorf("failed to start devcontainer: %w", err)
	}
	env.Devcontainer = devcontainer

	// 4. Start bridge intercept in the background.
	interceptArgs := fmt.Sprintf(
		"bridge intercept "+
			"--sandbox-url %s "+
			"--function-url %s "+
			"--name %s "+
			"--app-port %d",
		sandboxURL,
		functionURL,
		cfg.SandboxName,
		cfg.AppPort,
	)
	for _, d := range cfg.ForwardDomains {
		interceptArgs += fmt.Sprintf(" --forward-domains %s", d)
	}
	interceptCmd := []string{
		"sh", "-c",
		interceptArgs + " > /tmp/intercept.log 2>&1 &",
	}

	exitCode, _, err := devcontainer.Exec(ctx, interceptCmd)
	if err != nil {
		env.TearDown(ctx, nil)
		return nil, fmt.Errorf("failed to exec bridge intercept: %w", err)
	}
	if exitCode != 0 {
		env.TearDown(ctx, nil)
		return nil, fmt.Errorf("bridge intercept exited with code %d", exitCode)
	}

	// 5. Wait for bridge intercept to connect to the sandbox.
	if err := waitForInterceptReady(ctx, env.Devcontainer); err != nil {
		env.TearDown(ctx, nil)
		return nil, fmt.Errorf("bridge intercept not ready: %w", err)
	}

	return env, nil
}

// waitForInterceptReady polls the bridge intercept log until it shows the tunnel is connected.
func waitForInterceptReady(ctx context.Context, dc *DevcontainerContainer) error {
	deadline := time.After(30 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			// Dump logs for debugging
			_, logs, _ := dc.Exec(ctx, []string{"cat", "/tmp/intercept.log"})
			return fmt.Errorf("timed out waiting for bridge intercept to connect.\nIntercept logs:\n%s", logs)
		case <-ticker.C:
			exitCode, output, err := dc.Exec(ctx, []string{"grep", "-q", "Registration sent", "/tmp/intercept.log"})
			if err != nil {
				continue
			}
			if exitCode == 0 {
				_ = output
				return nil
			}
		}
	}
}

// TearDown stops all containers in parallel and cleans up the network.
// If t is non-nil and the test failed, container logs are printed for debugging.
func (e *Environment) TearDown(ctx context.Context, t *testing.T) {
	var wg sync.WaitGroup

	if e.Devcontainer != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if t != nil && t.Failed() {
				logs, err := e.Devcontainer.Logs(ctx)
				if err == nil {
					t.Logf("Devcontainer logs:\n%s", logs)
				}
				_, interceptLogs, _ := e.Devcontainer.Exec(ctx, []string{"cat", "/tmp/intercept.log"})
				t.Logf("Intercept logs:\n%s", interceptLogs)
			}
			e.Devcontainer.Terminate(ctx)
		}()
	}
	if e.Dispatcher != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if t != nil && t.Failed() {
				logs, err := e.Dispatcher.Logs(ctx)
				if err == nil {
					t.Logf("Dispatcher logs:\n%s", logs)
				}
			}
			e.Dispatcher.Terminate(ctx)
		}()
	}
	if e.Sandbox != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if t != nil && t.Failed() {
				logs, err := e.Sandbox.Logs(ctx)
				if err == nil {
					t.Logf("Sandbox logs:\n%s", logs)
				}
			}
			e.Sandbox.Terminate(ctx)
		}()
	}

	wg.Wait()

	if e.Network != nil {
		e.Network.Terminate(ctx)
	}
}

// randomPrivateIP generates a random IP in the 192.x.x.x range.
func randomPrivateIP() string {
	b := func() int {
		n, _ := rand.Int(rand.Reader, big.NewInt(254))
		return int(n.Int64()) + 1
	}
	return fmt.Sprintf("192.%d.%d.%d", b(), b(), b())
}

// InterceptLogs returns the bridge intercept log output from the devcontainer.
func (e *Environment) InterceptLogs(ctx context.Context) (string, error) {
	_, output, err := e.Devcontainer.Exec(ctx, []string{"cat", "/tmp/intercept.log"})
	if err != nil {
		return "", err
	}
	return output, nil
}
