package e2esandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/api/go/bridge/v1/bridgev1connect"
	"github.com/vercel/bridge/pkg/flow/sandbox"
	"github.com/vercel/bridge/pkg/flow/vercel"
)

const (
	teamID    = "vercel-labs"
	projectID = "vercel-sandbox-default-project"
)

// SandboxSuite runs e2e tests using a Vercel Sandbox as the remote environment
// instead of Kubernetes. The bridge server runs in HTTP protocol mode inside
// the sandbox, and tests exercise it via the sandbox's public URL.
type SandboxSuite struct {
	suite.Suite
	client    sandbox.Client
	sandboxID string
	routes    []sandbox.Route
	ctx       context.Context
	cancel    context.CancelFunc
	bridgeBin string // path to the cross-compiled Linux binary
}

func TestSandboxSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping sandbox e2e tests in short mode")
	}
	suite.Run(t, new(SandboxSuite))
}

func (s *SandboxSuite) SetupSuite() {
	s.ctx, s.cancel = context.WithTimeout(context.Background(), 5*time.Minute)
	t := s.T()

	token := resolveVercelToken(t)

	s.client = sandbox.New(sandbox.Config{
		Token:  token,
		TeamID: teamID,
	})

	// Build the bridge binary for Linux.
	s.bridgeBin = s.buildBridge()

	// Create a sandbox with a port for the HTTP bridge server.
	sbx, routes, err := s.client.Create(s.ctx, sandbox.CreateOpts{
		ProjectID: projectID,
		Ports:     []int{9090},
		Timeout:   5 * time.Minute,
	})
	require.NoError(t, err, "failed to create sandbox")
	s.sandboxID = sbx.ID
	s.routes = routes

	t.Logf("Sandbox created: %s", sbx.ID)
	for _, r := range routes {
		t.Logf("  Port %d -> %s", r.Port, r.URL)
	}

	// Upload the bridge binary to the sandbox.
	s.uploadBridge()

	// Start the bridge server in HTTP mode.
	s.startBridgeServer()
}

func (s *SandboxSuite) TearDownSuite() {
	if s.sandboxID != "" {
		// Use a fresh context since s.ctx may be cancelled.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = s.client.Stop(ctx, s.sandboxID)
		s.T().Logf("Sandbox stopped: %s", s.sandboxID)
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.bridgeBin != "" {
		os.Remove(s.bridgeBin)
	}
}

// buildBridge cross-compiles the bridge binary for Linux.
func (s *SandboxSuite) buildBridge() string {
	t := s.T()
	t.Log("Building bridge binary for Linux...")

	tmpFile, err := os.CreateTemp("", "bridge-e2e-*")
	require.NoError(t, err)
	tmpFile.Close()
	outPath := tmpFile.Name()

	// Vercel Sandbox runs Amazon Linux on x86_64.
	cmd := exec.CommandContext(s.ctx, "go", "build", "-ldflags=-s -w", "-o", outPath, "./cmd/bridge")
	cmd.Dir = projectRoot(t)
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "failed to build bridge: %s", string(out))

	t.Logf("Bridge binary built: %s", outPath)
	return outPath
}

// uploadBridge copies the bridge binary into the sandbox using the CLI
// since the API's write endpoint expects a gzipped tarball.
func (s *SandboxSuite) uploadBridge() {
	t := s.T()
	t.Log("Uploading bridge binary to sandbox...")

	cmd := exec.CommandContext(s.ctx, "npx", "sandbox", "copy",
		s.bridgeBin,
		s.sandboxID+":/vercel/sandbox/bridge",
		"--team", teamID,
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "failed to upload bridge binary: %s", string(out))

	// Make it executable.
	_, err = s.client.RunCommandAndWait(s.ctx, s.sandboxID, sandbox.RunCommandOpts{
		Command: "chmod",
		Args:    []string{"+x", "/vercel/sandbox/bridge"},
	})
	require.NoError(t, err, "failed to chmod bridge binary")

	t.Log("Bridge binary uploaded and ready")
}

// startBridgeServer starts the bridge server in HTTP mode inside the sandbox.
func (s *SandboxSuite) startBridgeServer() {
	t := s.T()
	t.Log("Starting bridge server in HTTP mode...")

	// Start the server in the background (non-blocking).
	_, err := s.client.RunCommand(s.ctx, s.sandboxID, sandbox.RunCommandOpts{
		Command: "/vercel/sandbox/bridge",
		Args:    []string{"server", "--protocol", "http", "--addr", ":9090"},
	})
	require.NoError(t, err, "failed to start bridge server")

	// Wait for the server to be ready by polling /healthz.
	s.waitForHealthz()
}

// waitForHealthz polls the bridge server's health endpoint until it responds.
func (s *SandboxSuite) waitForHealthz() {
	t := s.T()
	serverURL := s.serverURL()
	healthURL := serverURL + "/healthz"

	t.Logf("Waiting for bridge server at %s ...", healthURL)

	require.Eventually(t, func() bool {
		cmd, err := s.client.RunCommandAndWait(s.ctx, s.sandboxID, sandbox.RunCommandOpts{
			Command: "curl",
			Args:    []string{"-sf", "-o", "/dev/null", "-w", "%{http_code}", "http://localhost:9090/healthz"},
		})
		if err != nil {
			return false
		}
		return cmd.ExitCode != nil && *cmd.ExitCode == 0
	}, 30*time.Second, 1*time.Second, "bridge server did not become healthy")

	t.Log("Bridge server is healthy")
}

// serverURL returns the public URL for the bridge server's port 9090.
func (s *SandboxSuite) serverURL() string {
	for _, r := range s.routes {
		if r.Port == 9090 {
			return r.URL
		}
	}
	s.T().Fatal("no route found for port 9090")
	return ""
}

// TestResolveDNS tests DNS resolution through the bridge server's Connect RPC
// endpoint. This exercises the full path: client → Vercel proxy → bridge
// HTTP server → Connect RPC → system DNS resolver.
func (s *SandboxSuite) TestResolveDNS() {
	t := s.T()

	client := bridgev1connect.NewBridgeProxyServiceClient(
		http.DefaultClient,
		s.serverURL(),
	)

	resp, err := client.ResolveDNSQuery(s.ctx, connect.NewRequest(&bridgev1.ProxyResolveDNSRequest{
		Hostname: "google.com",
	}))
	require.NoError(t, err)
	require.Empty(t, resp.Msg.GetError(), "DNS resolution returned error: %s", resp.Msg.GetError())
	require.NotEmpty(t, resp.Msg.GetAddresses(), "DNS resolution returned no addresses")

	t.Logf("Resolved google.com → %v", resp.Msg.GetAddresses())
}

// resolveVercelToken returns a Vercel API token from VERCEL_TOKEN env var or
// the Vercel CLI's auth.json config file.
func resolveVercelToken(t *testing.T) string {
	t.Helper()

	token, err := vercel.ResolveToken()
	if errors.Is(err, vercel.ErrTokenNotFound) {
		t.Skip("skipping: VERCEL_TOKEN not set and no Vercel CLI auth.json found")
	}
	require.NoError(t, err)
	return token
}

// projectRoot returns the path to the bridge repository root.
func projectRoot(t *testing.T) string {
	t.Helper()
	// Walk up from this test file's location to find go.mod.
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(dir + "/go.mod"); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find project root (go.mod)")
		}
		dir = parent
	}
}

// helperFormatJSON pretty-prints JSON for test output.
func helperFormatJSON(data []byte) string {
	var out bytes.Buffer
	if err := json.Indent(&out, data, "", "  "); err != nil {
		return string(data)
	}
	return out.String()
}

// execInSandbox runs a command inside the sandbox and returns stdout.
func (s *SandboxSuite) execInSandbox(command string, args ...string) (string, error) {
	fullArgs := append([]string{"sandbox", "exec", s.sandboxID, "--team", teamID, "--"}, command)
	fullArgs = append(fullArgs, args...)

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(s.ctx, "npx", fullArgs...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return stdout.String(), fmt.Errorf("%w: %s", err, stderr.String())
	}
	return stdout.String(), nil
}
