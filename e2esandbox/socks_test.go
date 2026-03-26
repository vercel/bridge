package e2esandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"github.com/vercel/bridge/pkg/flow/sandbox"
)

// SocksSuite tests the SOCKS5 proxy mode of the interceptor running inside
// a Vercel Sandbox. It starts a bridge server + interceptor in SOCKS mode,
// then runs a Node script that makes HTTP calls through the SOCKS proxy.
type SocksSuite struct {
	suite.Suite
	client    sandbox.Client
	sandboxID string
	ctx       context.Context
	cancel    context.CancelFunc
	bridgeBin string
}

func TestSocksSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping sandbox SOCKS e2e tests in short mode")
	}
	suite.Run(t, new(SocksSuite))
}

func (s *SocksSuite) SetupSuite() {
	s.ctx, s.cancel = context.WithTimeout(context.Background(), 5*time.Minute)
	t := s.T()

	token := resolveVercelToken(t)
	s.client = sandbox.New(sandbox.Config{
		Token:  token,
		TeamID: teamID,
	})

	// Build bridge binary for linux/amd64.
	s.bridgeBin = s.buildBridge()

	// Create sandbox.
	sbx, _, err := s.client.Create(s.ctx, sandbox.CreateOpts{
		ProjectID: projectID,
		Timeout:   5 * time.Minute,
	})
	require.NoError(t, err)
	s.sandboxID = sbx.ID
	t.Logf("Sandbox created: %s", sbx.ID)

	// Upload bridge binary.
	s.uploadBridge()
}

func (s *SocksSuite) TearDownSuite() {
	if s.sandboxID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.client.Stop(ctx, s.sandboxID)
		s.T().Logf("Sandbox stopped: %s", s.sandboxID)
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.bridgeBin != "" {
		os.Remove(s.bridgeBin)
	}
}

func (s *SocksSuite) buildBridge() string {
	t := s.T()
	t.Log("Building bridge binary for Linux...")

	tmpFile, err := os.CreateTemp("", "bridge-socks-e2e-*")
	require.NoError(t, err)
	tmpFile.Close()
	outPath := tmpFile.Name()

	cmd := exec.CommandContext(s.ctx, "go", "build", "-ldflags=-s -w", "-o", outPath, "./cmd/bridge")
	cmd.Dir = projectRoot(t)
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "failed to build bridge: %s", string(out))

	t.Logf("Bridge binary built: %s", outPath)
	return outPath
}

func (s *SocksSuite) uploadBridge() {
	t := s.T()
	t.Log("Uploading bridge binary to sandbox...")

	cmd := exec.CommandContext(s.ctx, "npx", "sandbox", "copy",
		s.bridgeBin,
		s.sandboxID+":/vercel/sandbox/bridge",
		"--team", teamID,
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "failed to upload bridge: %s", string(out))

	_, err = s.client.RunCommandAndWait(s.ctx, s.sandboxID, sandbox.RunCommandOpts{
		Command: "chmod",
		Args:    []string{"+x", "/vercel/sandbox/bridge"},
	})
	require.NoError(t, err)
	t.Log("Bridge binary uploaded")
}

// TestSOCKSIntercept starts a bridge server and interceptor in SOCKS mode,
// then verifies the interceptor captures traffic from a Node script.
func (s *SocksSuite) TestSOCKSIntercept() {
	t := s.T()

	// Start bridge server in gRPC mode (the interceptor speaks gRPC).
	_, err := s.client.RunCommand(s.ctx, s.sandboxID, sandbox.RunCommandOpts{
		Command: "/vercel/sandbox/bridge",
		Args:    []string{"server", "--addr", ":9090"},
	})
	require.NoError(t, err)

	// Wait for server to be listening.
	require.Eventually(t, func() bool {
		cmd, err := s.client.RunCommandAndWait(s.ctx, s.sandboxID, sandbox.RunCommandOpts{
			Command: "bash",
			Args:    []string{"-c", "echo > /dev/tcp/127.0.0.1/9090"},
		})
		return err == nil && cmd.ExitCode != nil && *cmd.ExitCode == 0
	}, 30*time.Second, 1*time.Second)
	t.Log("Bridge server is listening")

	// Start interceptor in SOCKS mode (background).
	_, err = s.client.RunCommand(s.ctx, s.sandboxID, sandbox.RunCommandOpts{
		Command: "/vercel/sandbox/bridge",
		Args: []string{
			"intercept",
			"--server-addr", "localhost:9090",
			"--intercept-mode", "socks",
			"--socks-addr", ":1080",
			"--app-port", "3000",
			"--addr", ":8081",
		},
	})
	require.NoError(t, err)

	// Wait for the SOCKS proxy to be listening.
	require.Eventually(t, func() bool {
		cmd, err := s.client.RunCommandAndWait(s.ctx, s.sandboxID, sandbox.RunCommandOpts{
			Command: "bash",
			Args:    []string{"-c", "echo > /dev/tcp/127.0.0.1/1080"},
		})
		return err == nil && cmd.ExitCode != nil && *cmd.ExitCode == 0
	}, 15*time.Second, 1*time.Second)
	t.Log("SOCKS proxy is listening on :1080")

	// Use curl with socks5h:// to test the full path:
	// curl → SOCKS proxy → tunnel → bridge server → example.com
	curlResult, err := s.client.RunCommandAndWait(s.ctx, s.sandboxID, sandbox.RunCommandOpts{
		Command: "curl",
		Args:    []string{"-sf", "--max-time", "15", "--proxy", "socks5h://127.0.0.1:1080", "http://example.com"},
	})
	require.NoError(t, err, "curl through SOCKS proxy failed")
	require.NotNil(t, curlResult.ExitCode)
	require.Equal(t, 0, *curlResult.ExitCode, "curl exit code should be 0")
	t.Log("curl through SOCKS proxy succeeded")

	// Read the interceptor's log file to verify the SOCKS proxy handled the connection.
	logResult, err := s.client.RunCommandAndWait(s.ctx, s.sandboxID, sandbox.RunCommandOpts{
		Command: "cat",
		Args:    []string{"/home/vercel-sandbox/.bridge/logs/bridge.log"},
	})
	require.NoError(t, err, "failed to read bridge.log")

	logStream, err := s.client.StreamLogs(s.ctx, s.sandboxID, logResult.ID)
	require.NoError(t, err)
	defer logStream.Close()

	logData := make([]byte, 256*1024)
	n, _ := logStream.Read(logData)
	bridgeLogs := string(logData[:n])
	t.Logf("bridge.log:\n%s", bridgeLogs)

	require.Contains(t, bridgeLogs, "SOCKS5 CONNECT", "bridge.log should show SOCKS5 CONNECT")
	require.Contains(t, bridgeLogs, "example.com", "bridge.log should show the target hostname")
}

// resolveVercelToken is shared with sandbox_test.go via the package.
// If it's already defined there, this file just uses it.

// projectRoot is shared with sandbox_test.go via the package.

// helperFormatJSON pretty-prints JSON for test output (shared).
func helperFormatJSON2(data []byte) string {
	var out bytes.Buffer
	if err := json.Compact(&out, data); err != nil {
		return string(data)
	}
	return out.String()
}
