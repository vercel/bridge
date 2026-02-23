package e2e

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/vercel/bridge/e2e/testutil"
)

// InterceptSuite exercises the core intercept → proxy → DNS → tunnel data path
// using plain Docker containers (no devcontainers, no Kubernetes).
//
// Architecture:
//
//	"internal" network              "bridge" network
//	┌─────────────────┐
//	│ testserver :8080 │
//	│ alias: testserver│
//	└─────────────────┘
//	                                ┌──────────────────────────┐
//	┌─────────────────┐─────────────│ intercept container       │
//	│ proxy container  │             │ (bridge network ONLY)     │
//	│ (BOTH networks)  │             │ --privileged              │
//	│ bridge server    │             │                           │
//	│ :9090            │             │ bridge intercept          │
//	│                  │             │  --server-addr <proxy-ip> │
//	└─────────────────┘             │  --forward-domains "*"    │
//	                                │                           │
//	                                │ wget testserver:8080 → ok │
//	                                └──────────────────────────┘
//
// The intercept container cannot resolve "testserver" directly (different network).
// Only the proxy can, since it's on both networks. This proves the tunnel works.
type InterceptSuite struct {
	suite.Suite
	ctx    context.Context
	cancel context.CancelFunc

	internalNet *testcontainers.DockerNetwork // testserver + proxy
	bridgeNet   *testcontainers.DockerNetwork // proxy + intercept

	bridgeBin     string
	interceptImg  string
	testServerImg string
}

func (s *InterceptSuite) SetupSuite() {
	if testing.Short() {
		s.T().Skip("skipping e2e test in short mode")
	}

	s.ctx, s.cancel = context.WithTimeout(context.Background(), 10*time.Minute)

	var err error

	// Build the bridge binary for Linux.
	s.bridgeBin, err = testutil.BuildBridge()
	require.NoError(s.T(), err, "failed to build bridge binary")

	// Build the intercept container image (alpine + bridge + iptables).
	s.interceptImg = "bridge-intercept:e2e-test"
	err = testutil.BuildInterceptImage(s.ctx, s.bridgeBin, s.interceptImg)
	require.NoError(s.T(), err, "failed to build intercept image")

	// Build the test server image.
	s.testServerImg = "bridge-testserver:e2e-test"
	err = testutil.BuildTestServerImage(s.ctx, s.testServerImg)
	require.NoError(s.T(), err, "failed to build test server image")

	// Create two Docker networks.
	s.internalNet, err = network.New(s.ctx, network.WithDriver("bridge"))
	require.NoError(s.T(), err, "failed to create internal network")
	slog.Info("Created internal network", "name", s.internalNet.Name)

	s.bridgeNet, err = network.New(s.ctx, network.WithDriver("bridge"))
	require.NoError(s.T(), err, "failed to create bridge network")
	slog.Info("Created bridge network", "name", s.bridgeNet.Name)
}

func (s *InterceptSuite) TearDownSuite() {
	if s.bridgeNet != nil {
		s.bridgeNet.Remove(s.ctx)
	}
	if s.internalNet != nil {
		s.internalNet.Remove(s.ctx)
	}
	// Note: intentionally not calling testutil.CleanupBuild() here.
	// The binary is shared via sync.Once across all suites in the process.
	// Deleting it here would break other suites that run after this one.
	if s.cancel != nil {
		s.cancel()
	}
}

// startProxy starts a proxy container on both networks with the given extra args.
// It registers cleanup to terminate and dump logs on test completion.
func (s *InterceptSuite) startProxy(t *testing.T, cmd []string) (testcontainers.Container, string) {
	proxyC, err := testcontainers.GenericContainer(s.ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:    s.interceptImg,
			Cmd:      cmd,
			Networks: []string{s.internalNet.Name, s.bridgeNet.Name},
			NetworkAliases: map[string][]string{
				s.bridgeNet.Name: {"proxy"},
			},
			WaitingFor: wait.ForLog("gRPC proxy server listening").WithStartupTimeout(30 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err, "failed to start proxy container")
	t.Cleanup(func() {
		if logs, err := proxyC.Logs(s.ctx); err == nil {
			data, _ := io.ReadAll(logs)
			t.Logf("[proxy container logs]\n%s", string(data))
		}
		testcontainers.TerminateContainer(proxyC)
	})

	// Get the proxy container's IP on the bridge network.
	proxyInspect, err := proxyC.Inspect(s.ctx)
	require.NoError(t, err, "failed to inspect proxy container")
	proxyIP := proxyInspect.NetworkSettings.Networks[s.bridgeNet.Name].IPAddress
	require.NotEmpty(t, proxyIP, "proxy container has no IP on bridge network")
	t.Logf("proxy IP on bridge network: %s", proxyIP)

	return proxyC, proxyIP
}

// startIntercept starts an intercept container on the bridge network only.
func (s *InterceptSuite) startIntercept(t *testing.T) testcontainers.Container {
	interceptC, err := testcontainers.GenericContainer(s.ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:      s.interceptImg,
			Cmd:        []string{"sleep", "300"},
			Privileged: true,
			Networks:   []string{s.bridgeNet.Name},
			WaitingFor: wait.ForLog("").WithStartupTimeout(10 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err, "failed to start intercept container")
	t.Cleanup(func() {
		if logs, err := interceptC.Logs(s.ctx); err == nil {
			data, _ := io.ReadAll(logs)
			t.Logf("[intercept container logs]\n%s", string(data))
		}
		testcontainers.TerminateContainer(interceptC)
	})
	return interceptC
}

// startTestServer starts a test server container on the internal network only.
func (s *InterceptSuite) startTestServer(t *testing.T) testcontainers.Container {
	testserverC, err := testcontainers.GenericContainer(s.ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:    s.testServerImg,
			Env:      map[string]string{"PORT": "8080"},
			Networks: []string{s.internalNet.Name},
			NetworkAliases: map[string][]string{
				s.internalNet.Name: {"testserver"},
			},
			WaitingFor: wait.ForLog("testserver listening on :8080").WithStartupTimeout(30 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err, "failed to start testserver")
	t.Cleanup(func() { testcontainers.TerminateContainer(testserverC) })
	return testserverC
}

func (s *InterceptSuite) TestIntercept() {
	t := s.T()

	// 1. Start testserver on the internal network only.
	s.startTestServer(t)
	t.Log("testserver started")

	// 2. Start proxy container on BOTH networks.
	_, proxyIP := s.startProxy(t, []string{"bridge", "server", "--addr", ":9090"})
	t.Log("proxy container started")

	// 3. Start intercept container on the bridge network only (privileged for iptables).
	interceptC := s.startIntercept(t)
	t.Log("intercept container started")

	// Start bridge intercept in background inside the container.
	serverAddr := proxyIP + ":9090"
	exitCode, reader, err := interceptC.Exec(s.ctx, []string{
		"sh", "-c",
		fmt.Sprintf(`bridge intercept --server-addr %s --forward-domains "*" > /tmp/bridge.log 2>&1 &`, serverAddr),
	})
	require.NoError(t, err, "failed to exec bridge intercept")
	io.Copy(io.Discard, reader)
	require.Equal(t, 0, exitCode, "bridge intercept exec failed")

	// Poll for intercept + iptables readiness.
	s.Require().Eventually(func() bool {
		exitCode, reader, err := interceptC.Exec(s.ctx, []string{
			"sh", "-c", "cat /tmp/bridge.log",
		})
		if err != nil || exitCode != 0 {
			return false
		}
		data, _ := io.ReadAll(reader)
		logContent := string(data)
		return strings.Contains(logContent, "iptables rules configured")
	}, 60*time.Second, 2*time.Second, "bridge intercept did not become ready")
	t.Log("Bridge intercept is ready")

	// 4. Verify: wget testserver:8080 through the tunnel.
	// The intercept container can't resolve "testserver" directly (different network).
	// The request goes: DNS → proxy (resolves via Docker DNS on internal network) →
	// iptables redirect → transparent proxy → gRPC tunnel → proxy dials testserver:8080.
	exitCode, reader, err = interceptC.Exec(s.ctx, []string{
		"wget", "-O", "-", "-T", "10", "http://testserver:8080/",
	})
	require.NoError(t, err, "wget exec failed")
	data, _ := io.ReadAll(reader)
	wgetOutput := string(data)
	t.Logf("[wget] exit=%d output=%s", exitCode, strings.TrimSpace(wgetOutput))

	// Dump intercept log on failure for debugging.
	if exitCode != 0 {
		exitCode, reader, err = interceptC.Exec(s.ctx, []string{"cat", "/tmp/bridge.log"})
		if err == nil {
			data, _ := io.ReadAll(reader)
			t.Logf("[intercept-log]\n%s", string(data))
		}
	}

	require.Equal(t, 0, exitCode, "wget returned non-zero exit code")
	require.Contains(t, wgetOutput, "ok", "expected 'ok' from test server")
}

func (s *InterceptSuite) TestIngress() {
	t := s.T()

	// 1. Start proxy with --listen-ports 8080/tcp on BOTH networks.
	proxyC, proxyIP := s.startProxy(t, []string{"bridge", "server", "--addr", ":9090", "-l", "8080/tcp"})
	t.Log("proxy container started with ingress listener on :8080")

	// 2. Start intercept container on the bridge network only.
	interceptC := s.startIntercept(t)
	t.Log("intercept container started")

	// 3. Start testserver (HTTP on :8080) inside the intercept container.
	// Copy the testserver binary from the testserver image first.
	// Instead, exec a small Go-free HTTP server using busybox httpd.
	// The intercept image has busybox-extras with httpd.
	exitCode, reader, err := interceptC.Exec(s.ctx, []string{
		"sh", "-c", `mkdir -p /var/www && echo -n "ok" > /var/www/index.html && httpd -p 8080 -h /var/www && echo "testserver listening on :8080"`,
	})
	require.NoError(t, err, "failed to start httpd in intercept container")
	data, _ := io.ReadAll(reader)
	t.Logf("[httpd start] exit=%d output=%s", exitCode, strings.TrimSpace(string(data)))
	require.Equal(t, 0, exitCode, "httpd start failed")

	// 4. Start bridge intercept with --app-port 8080 in background.
	serverAddr := proxyIP + ":9090"
	exitCode, reader, err = interceptC.Exec(s.ctx, []string{
		"sh", "-c",
		fmt.Sprintf(`bridge intercept --server-addr %s --app-port 8080 > /tmp/bridge.log 2>&1 &`, serverAddr),
	})
	require.NoError(t, err, "failed to exec bridge intercept")
	io.Copy(io.Discard, reader)
	require.Equal(t, 0, exitCode, "bridge intercept exec failed")

	// 5. Poll for tunnel readiness in intercept log.
	s.Require().Eventually(func() bool {
		exitCode, reader, err := interceptC.Exec(s.ctx, []string{
			"sh", "-c", "cat /tmp/bridge.log",
		})
		if err != nil || exitCode != 0 {
			return false
		}
		data, _ := io.ReadAll(reader)
		return strings.Contains(string(data), "Tunnel connected")
	}, 60*time.Second, 2*time.Second, "tunnel did not connect")
	t.Log("Tunnel is connected")

	// 6. Poll for tunnel connected on the proxy side.
	s.Require().Eventually(func() bool {
		logs, err := proxyC.Logs(s.ctx)
		if err != nil {
			return false
		}
		data, _ := io.ReadAll(logs)
		return strings.Contains(string(data), "Tunnel connected")
	}, 30*time.Second, 2*time.Second, "tunnel did not connect on proxy")
	t.Log("Tunnel connected on proxy")

	// 7. Send HTTP request to the proxy's ingress listener port (8080).
	// Use wget from inside the proxy container to hit its own :8080.
	exitCode, reader, err = proxyC.Exec(s.ctx, []string{
		"wget", "-O", "-", "-T", "10", "http://127.0.0.1:8080/",
	})
	require.NoError(t, err, "wget exec failed")
	data, _ = io.ReadAll(reader)
	wgetOutput := string(data)
	t.Logf("[wget ingress] exit=%d output=%s", exitCode, strings.TrimSpace(wgetOutput))

	// Dump logs on failure.
	if exitCode != 0 {
		if logs, err := proxyC.Logs(s.ctx); err == nil {
			data, _ := io.ReadAll(logs)
			t.Logf("[proxy-log]\n%s", string(data))
		}
		exitCode, reader, err = interceptC.Exec(s.ctx, []string{"cat", "/tmp/bridge.log"})
		if err == nil {
			data, _ := io.ReadAll(reader)
			t.Logf("[intercept-log]\n%s", string(data))
		}
	}

	require.Equal(t, 0, exitCode, "wget returned non-zero exit code")
	require.Contains(t, wgetOutput, "ok", "expected 'ok' from test server via ingress tunnel")
}

func TestInterceptSuite(t *testing.T) {
	suite.Run(t, new(InterceptSuite))
}
