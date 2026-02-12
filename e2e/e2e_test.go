package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

// E2ESuite is the base test suite for e2e tests
type E2ESuite struct {
	suite.Suite
	ctx          context.Context
	cancel       context.CancelFunc
	env          *Environment
	testHostname string
}

// SetupSuite runs once before all tests in the suite
func (s *E2ESuite) SetupSuite() {
	if testing.Short() {
		s.T().Skip("skipping e2e test in short mode")
	}

	s.ctx, s.cancel = context.WithTimeout(context.Background(), 5*time.Minute)

	// Generate a unique hostname for DNS forwarding tests
	id := make([]byte, 4)
	_, _ = rand.Read(id)
	s.testHostname = fmt.Sprintf("vercel-bridge-test-%s.com", hex.EncodeToString(id))

	var err error
	s.env, err = SetupEnvironment(s.ctx, EnvironmentConfig{
		ForwardDomains:         []string{s.testHostname},
		DevcontainerPrivileged: true,
	})
	require.NoError(s.T(), err, "failed to setup environment")

	// Add the test hostname to the dispatcher's /etc/hosts pointing to itself.
	dispatcherIP, err := s.env.Dispatcher.ContainerIP(s.ctx, s.env.Network.Name)
	require.NoError(s.T(), err, "failed to get dispatcher container IP")

	addHostCmd := []string{"sh", "-c",
		fmt.Sprintf("echo '%s %s' >> /etc/hosts", dispatcherIP, s.testHostname),
	}
	exitCode, reader, err := s.env.Dispatcher.Container.Exec(s.ctx, addHostCmd)
	require.NoError(s.T(), err, "failed to add /etc/hosts entry to dispatcher")
	_, _ = io.ReadAll(reader)
	require.Equal(s.T(), 0, exitCode, "failed to add /etc/hosts entry to dispatcher")

	s.T().Logf("Test hostname: %s -> %s (dispatcher)", s.testHostname, dispatcherIP)
}

// TearDownSuite runs once after all tests in the suite
func (s *E2ESuite) TearDownSuite() {
	if s.env != nil {
		s.env.TearDown(s.ctx, s.T())
	}
	if s.cancel != nil {
		s.cancel()
	}
	CleanupBuild()
}

// TestHealth verifies the sandbox responds to health checks
func (s *E2ESuite) TestHealth() {
	resp, err := http.Get(s.env.Sandbox.URL() + "/health")
	s.Require().NoError(err, "health check request failed")
	defer resp.Body.Close()

	s.Equal(http.StatusOK, resp.StatusCode)
	s.Equal("test-sandbox", resp.Header.Get("X-Bridge-Name"))
}

// TestDevcontainerVersion verifies the devcontainer can run bridge commands
func (s *E2ESuite) TestDevcontainerVersion() {
	exitCode, output, err := s.env.Devcontainer.Exec(s.ctx, []string{"bridge", "--version"})
	s.Require().NoError(err)
	s.Equal(0, exitCode, "expected exit code 0")
	s.Contains(output, "bridge version")
}

// TestSSHConnection verifies that SSH works through the tunnel
func (s *E2ESuite) TestSSHConnection() {
	sshCmd := []string{
		"sh", "-c",
		"ssh bridge.test-sandbox echo 'hello from sandbox'",
	}
	exitCode, output, err := s.env.Devcontainer.Exec(s.ctx, sshCmd)

	s.T().Logf("SSH output:\n%s", output)
	s.T().Logf("SSH exit code: %d", exitCode)

	s.Require().NoError(err, "SSH command failed")
	s.Require().Equal(0, exitCode, "SSH command returned non-zero exit code")
	s.Contains(output, "hello from sandbox", "unexpected SSH output")
}

// TestMutagenSync verifies that mutagen syncs files between devcontainer and sandbox
func (s *E2ESuite) TestMutagenSync() {
	// Create a test file in the devcontainer
	exitCode, _, err := s.env.Devcontainer.Exec(s.ctx, []string{"sh", "-c", "echo 'hello from devcontainer' > test.txt"})
	s.Require().NoError(err, "failed to create test file")
	s.Require().Equal(0, exitCode, "failed to create test file")

	// Wait for sync
	time.Sleep(2 * time.Second)

	// Debug: list sandbox directory contents
	_, lsOutput, _ := s.env.Sandbox.Exec(s.ctx, []string{"ls", "-la", "/vercel/sandbox"})
	s.T().Logf("Sandbox /vercel/sandbox contents:\n%s", lsOutput)

	// Check that the file exists in the sandbox
	exitCode, fileContent, err := s.env.Sandbox.Exec(s.ctx, []string{"cat", "/vercel/sandbox/test.txt"})
	s.Require().NoError(err, "failed to read file from sandbox")
	s.Require().Equal(0, exitCode, "file not found in sandbox")
	s.Contains(fileContent, "hello from devcontainer", "file content mismatch")
}

// TestDispatcherForward verifies that a request to the dispatcher is forwarded
// through the tunnel to the devcontainer app.
func (s *E2ESuite) TestDispatcherForward() {
	// Start a simple HTTP server on port 3000 inside the devcontainer.
	// Use busybox httpd without -f so it daemonizes and survives the exec session.
	startServer := []string{
		"sh", "-c",
		"mkdir -p /tmp/www && echo 'hello from devcontainer app' > /tmp/www/index.html && httpd -p 3000 -h /tmp/www",
	}
	exitCode, output, err := s.env.Devcontainer.Exec(s.ctx, startServer)
	s.Require().NoError(err, "failed to start HTTP server")
	s.Require().Equal(0, exitCode, "failed to start HTTP server: %s", output)

	// Verify the server is listening before sending traffic through the tunnel.
	exitCode, output, err = s.env.Devcontainer.Exec(s.ctx, []string{"wget", "-q", "-O", "-", "http://127.0.0.1:3000/index.html"})
	s.Require().NoError(err, "httpd not reachable")
	s.Require().Equal(0, exitCode, "httpd not reachable: %s", output)

	// Send a request to the dispatcher — this triggers the tunnel pairing
	// (dispatcher connects to sandbox on first request) and forwards
	// through: dispatcher → sandbox → devcontainer intercept → local app.
	// Retry a few times since the first request triggers tunnel setup.
	var resp *http.Response
	for attempt := 1; attempt <= 5; attempt++ {
		resp, err = http.Get(s.env.Dispatcher.URL() + "/index.html")
		if err == nil && resp.StatusCode == http.StatusOK {
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		s.T().Logf("Attempt %d: status=%d err=%v", attempt, resp.StatusCode, err)
		time.Sleep(2 * time.Second)
	}

	s.Require().NoError(err, "request to dispatcher failed")
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err, "failed to read response body")

	s.T().Logf("Dispatcher response status: %d", resp.StatusCode)
	s.T().Logf("Dispatcher response body: %s", string(body))

	s.Equal(http.StatusOK, resp.StatusCode)
	s.Contains(string(body), "hello from devcontainer app")
}

// TestDNSForwarding verifies that an HTTP request from the devcontainer to a
// hostname that only resolves on the dispatcher reaches the dispatcher's health
// endpoint through the tunnel.
func (s *E2ESuite) TestDNSForwarding() {
	url := fmt.Sprintf("http://%s:8080/__bridge/health", s.testHostname)
	cmd := []string{"wget", "-q", "-O", "-", "-T", "5", url}

	var exitCode int
	var output string
	var err error

	for attempt := 1; attempt <= 10; attempt++ {
		exitCode, output, err = s.env.Devcontainer.Exec(s.ctx, cmd)
		if err == nil && exitCode == 0 {
			break
		}
		s.T().Logf("Attempt %d: exitCode=%d err=%v output=%s", attempt, exitCode, err, output)

		if attempt == 10 {
			logs, _ := s.env.InterceptLogs(s.ctx)
			s.T().Logf("Intercept logs:\n%s", logs)
		}

		time.Sleep(2 * time.Second)
	}

	s.Require().NoError(err)
	s.Require().Equal(0, exitCode, "wget to %s failed: %s", url, output)
	s.T().Logf("DNS forward health response: %s", output)
}

// TestE2ESuite runs the e2e test suite
func TestE2ESuite(t *testing.T) {
	suite.Run(t, new(E2ESuite))
}
