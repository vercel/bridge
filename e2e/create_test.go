package e2e

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"github.com/urfave/cli/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vercel/bridge/e2e/testutil"
	"github.com/vercel/bridge/pkg/commands"
	"github.com/vercel/bridge/pkg/container"
	"github.com/vercel/bridge/pkg/devcontainer"
	"github.com/vercel/bridge/pkg/identity"
	"github.com/vercel/bridge/pkg/intercept"
	"github.com/vercel/bridge/pkg/k8s/kube"
	"github.com/vercel/bridge/pkg/k8s/meta"
)

func bridgeLabels(name string) map[string]string {
	return map[string]string{"bridge.deployment": name}
}

// CreateSuite exercises the full bridge create --connect flow.
type CreateSuite struct {
	suite.Suite
	ctx    context.Context
	cancel context.CancelFunc

	cluster          *testutil.Cluster
	administratorRef string
	userserviceRef   string

	adminPod    corev1.Pod
	bridgeBin   string // path to pre-built linux bridge binary
	projectRoot string

	// Temp dirs / state to restore on teardown.
	origDir        string
	origKubeconfig string
	workspaceDir   string // set by test, used by TearDownSuite for devcontainer cleanup
	bridgeName     string // deployment name, used to stop containers on teardown
}

// createWorkspace creates a temp workspace directory with a devcontainer.json
// that includes all test-specific config: mounts, runArgs, containerEnv.
// It also copies the bridge feature into the workspace so it can be referenced
// as a relative path (the devcontainer CLI rejects absolute feature paths).
func createWorkspace(t *testing.T, bridgeBin, projectRoot string, cluster *testutil.Cluster) string {
	dir, err := os.MkdirTemp(os.TempDir(), "bridge-create-test-*")
	require.NoError(t, err)
	// Resolve symlinks so the devcontainer CLI's workspace-folder label matches
	// regardless of whether the path goes through macOS /var → /private/var.
	dir, err = filepath.EvalSymlinks(dir)
	require.NoError(t, err)

	dcDir := filepath.Join(dir, ".devcontainer")
	require.NoError(t, os.MkdirAll(dcDir, 0755))

	// Copy the bridge feature into .devcontainer/ (the devcontainer CLI
	// requires local features to live under .devcontainer/).
	srcFeature := filepath.Join(projectRoot, "features", "bridge-feature")
	dstFeature := filepath.Join(dcDir, "local-features", "bridge-feature")
	require.NoError(t, copyDir(srcFeature, dstFeature))

	cfg := &devcontainer.Config{
		Image: "mcr.microsoft.com/devcontainers/base:alpine-3.20",
		Mounts: []string{
			fmt.Sprintf("source=%s,target=/usr/local/bin/bridge,type=bind,readonly", bridgeBin),
			fmt.Sprintf("source=%s,target=/tmp/bridge-kubeconfig,type=bind,readonly", cluster.InternalKubeConfigPath),
		},
		RunArgs: []string{
			"--network=" + cluster.Network.Name,
			"--privileged",
		},
		ContainerEnv: map[string]string{
			"KUBECONFIG": "/tmp/bridge-kubeconfig",
		},
	}
	require.NoError(t, cfg.Save(filepath.Join(dcDir, "devcontainer.json")))
	return dir
}

// newBridgeCreateApp returns a configured CLI app and builds the base args for
// `bridge create --connect`. Callers append test-specific args (e.g. deployment
// name, --source, --admin-addr) before the base args.
func (s *CreateSuite) newBridgeCreateApp(reader io.Reader, writer io.Writer, workspaceDir string, extraArgs ...string) (*cli.Command, []string) {
	app := commands.NewApp()
	app.Reader = reader
	app.Writer = writer

	args := []string{"bridge", "create"}
	args = append(args, extraArgs...)
	args = append(args,
		"-n", testutil.UserserviceNamespace,
		"--yes",
		"--connect",
		"--feature-ref", "../local-features/bridge-feature",
		"--container-binary-path", s.bridgeBin,
		"--proxy-image", s.administratorRef,
		"-f", filepath.Join(workspaceDir, ".devcontainer", "devcontainer.json"),
	)
	return app, args
}

func (s *CreateSuite) SetupSuite() {
	if testing.Short() {
		s.T().Skip("skipping e2e test in short mode")
	}

	s.ctx, s.cancel = context.WithTimeout(context.Background(), 5*time.Minute)

	var err error

	// Find project root (needed for local feature path).
	s.projectRoot, err = testutil.FindProjectRoot()
	require.NoError(s.T(), err, "failed to find project root")

	// 1. Setup cluster.
	s.cluster, err = testutil.SetupCluster(s.ctx)
	require.NoError(s.T(), err, "failed to setup cluster")

	// 2. Build and push images.
	bridgeTag := "bridge-cli:test"
	err = testutil.BuildBridgeImage(s.ctx, bridgeTag)
	require.NoError(s.T(), err, "failed to build bridge image")

	s.administratorRef, err = s.cluster.PushImage(s.ctx, bridgeTag, bridgeTag)
	require.NoError(s.T(), err, "failed to push bridge image")

	userserviceTag := "userservice:test"
	err = testutil.BuildUserserviceImage(s.ctx, userserviceTag)
	require.NoError(s.T(), err, "failed to build test server image")

	s.userserviceRef, err = s.cluster.PushImage(s.ctx, userserviceTag, userserviceTag)
	require.NoError(s.T(), err, "failed to push test server image")

	// 3. Deploy administrator.
	adminPod, err := testutil.DeployAdministrator(s.ctx, s.cluster.RestConfig, s.cluster.Clientset, s.administratorRef)
	require.NoError(s.T(), err, "failed to deploy administrator")
	s.adminPod = *adminPod
	slog.Info("Administrator pod", "name", s.adminPod.Name)

	// 4. Deploy test server.
	err = testutil.DeployUserservice(s.ctx, s.cluster.RestConfig, s.cluster.Clientset, s.userserviceRef)
	require.NoError(s.T(), err, "failed to deploy userservice")

	// 6. Build bridge binary for linux.
	s.bridgeBin, err = testutil.BuildBridge()
	require.NoError(s.T(), err, "failed to build bridge binary")

	// Point KUBECONFIG at the test cluster for all tests in the suite.
	s.origKubeconfig = os.Getenv("KUBECONFIG")
	os.Setenv("KUBECONFIG", s.cluster.KubeConfigPath)

	slog.Info("SetupSuite complete")
}

func (s *CreateSuite) TearDownSuite() {
	// Stop devcontainer if we started one.
	if s.bridgeName != "" {
		container.NewDockerClient().StopAll(s.ctx, container.StopAllOpts{Labels: bridgeLabels(s.bridgeName)})
	}
	if s.workspaceDir != "" {
		os.RemoveAll(s.workspaceDir)
	}
	// Restore working directory.
	if s.origDir != "" {
		os.Chdir(s.origDir)
	}
	// Restore KUBECONFIG.
	if s.origKubeconfig != "" {
		os.Setenv("KUBECONFIG", s.origKubeconfig)
	} else {
		os.Unsetenv("KUBECONFIG")
	}
	if s.cluster != nil {
		s.cluster.TearDown(s.ctx)
	}
	testutil.CleanupBuild()
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *CreateSuite) TestFullstackCreate() {
	t := s.T()

	// --- Setup ---

	_, err := identity.EnsureDeviceID()
	require.NoError(t, err)

	s.workspaceDir = createWorkspace(t, s.bridgeBin, s.projectRoot, s.cluster)

	s.origDir, err = os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(s.workspaceDir))

	// --- Create pipes for stdin/stdout ---

	stdinR, stdinW, err := os.Pipe()
	require.NoError(t, err)
	stdoutR, stdoutW, err := os.Pipe()
	require.NoError(t, err)

	// --- Run `bridge create --connect` in a goroutine ---

	adminAddr := fmt.Sprintf("k8spf:///%s.%s:9090", s.adminPod.Name, testutil.AdministratorNamespace)

	app, args := s.newBridgeCreateApp(stdinR, stdoutW, s.workspaceDir,
		testutil.UserserviceName, "--admin-addr", adminAddr)

	errCh := make(chan error, 1)
	go func() {
		defer stdoutW.Close()
		errCh <- app.Run(s.ctx, args)
	}()

	// Log stdout from bridge create in the background.
	go func() {
		scanner := bufio.NewScanner(stdoutR)
		for scanner.Scan() {
			t.Logf("[bridge] %s", scanner.Text())
		}
	}()

	// --- Wait for bridge pod and intercept readiness ---

	deviceID, _ := identity.GetDeviceID()
	bridgeName := identity.BridgeResourceName(deviceID, testutil.UserserviceName)
	s.bridgeName = bridgeName
	bridgeNS := testutil.UserserviceNamespace

	pod, err := kube.WaitForPod(s.ctx, s.cluster.Clientset, bridgeNS, meta.DeploymentSelector(bridgeName), 1*time.Minute)
	require.NoError(t, err, "bridge pod not ready")
	t.Logf("Bridge pod ready: %s", pod.Name)

	ct := container.NewDockerClient()
	containerID, err := container.WaitForID(s.ctx, ct, container.FindOpts{Labels: bridgeLabels(bridgeName)})
	require.NoError(t, err, "container not found")
	require.NoError(t, intercept.WaitForReady(s.ctx, ct, containerID), "intercept not ready")

	// --- Verify network access ---

	generatedConfig := filepath.Join(s.workspaceDir, ".devcontainer",
		fmt.Sprintf("bridge-%s", bridgeName), "devcontainer.json")
	dc := &devcontainer.Client{
		WorkspaceFolder: s.workspaceDir,
		ConfigPath:      generatedConfig,
	}
	targetURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/",
		testutil.UserserviceServiceName, testutil.UserserviceNamespace, testutil.UserservicePort)

	wgetOut, wgetErr := dc.ExecOutput(s.ctx, []string{"wget", "-O", "-", "-T", "10", targetURL})
	t.Logf("[wget] output: %s", strings.TrimSpace(wgetOut))
	require.NoError(t, wgetErr, "wget failed")
	require.Contains(t, wgetOut, "ok", "expected test server response")

	// --- Clean up: close stdin so bash exits ---

	stdinW.Close()

	require.NoError(t, <-errCh, "bridge create --connect failed")
}

// TestCreateFailsOnInterceptCrash verifies that bridge create --connect fails
// with an informative error when the intercept process crashes inside the
// devcontainer. Uses the __TEST_FAIL_INTERCEPT env var to trigger a crash
// after full initialization.
func (s *CreateSuite) TestCreateFailsOnInterceptCrash() {
	t := s.T()

	dir, err := os.MkdirTemp(os.TempDir(), "bridge-crash-test-*")
	require.NoError(t, err)
	dir, err = filepath.EvalSymlinks(dir)
	require.NoError(t, err)

	dcDir := filepath.Join(dir, ".devcontainer")
	require.NoError(t, os.MkdirAll(dcDir, 0755))

	srcFeature := filepath.Join(s.projectRoot, "features", "bridge-feature")
	dstFeature := filepath.Join(dcDir, "local-features", "bridge-feature")
	require.NoError(t, copyDir(srcFeature, dstFeature))

	cfg := &devcontainer.Config{
		Image: "mcr.microsoft.com/devcontainers/base:alpine-3.20",
		Mounts: []string{
			fmt.Sprintf("source=%s,target=/usr/local/bin/bridge,type=bind,readonly", s.bridgeBin),
			fmt.Sprintf("source=%s,target=/tmp/bridge-kubeconfig,type=bind,readonly", s.cluster.InternalKubeConfigPath),
		},
		RunArgs: []string{
			"--network=" + s.cluster.Network.Name,
			"--privileged",
		},
		ContainerEnv: map[string]string{
			"KUBECONFIG":            "/tmp/bridge-kubeconfig",
			"__TEST_FAIL_INTERCEPT": "true",
		},
	}
	require.NoError(t, cfg.Save(filepath.Join(dcDir, "devcontainer.json")))

	origDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { os.Chdir(origDir) })

	_, err = identity.EnsureDeviceID()
	require.NoError(t, err)

	deviceID, _ := identity.GetDeviceID()
	crashBridgeName := identity.BridgeResourceName(deviceID, testutil.UserserviceName)
	t.Cleanup(func() {
		container.NewDockerClient().StopAll(s.ctx, container.StopAllOpts{Labels: bridgeLabels(crashBridgeName)})
		os.RemoveAll(dir)
	})

	adminAddr := fmt.Sprintf("k8spf:///%s.%s:9090", s.adminPod.Name, testutil.AdministratorNamespace)

	stdinR, stdinW, err := os.Pipe()
	require.NoError(t, err)
	defer stdinW.Close()

	app, args := s.newBridgeCreateApp(stdinR, io.Discard, dir,
		testutil.UserserviceName, "--admin-addr", adminAddr)

	err = app.Run(s.ctx, args)

	require.Error(t, err)
	t.Logf("create error: %v", err)
	require.Contains(t, err.Error(), "container failed to start")
}

// TestManifestSourceCreate exercises the --source flag with manifest-based creation
// and verifies that annotations on the Deployment are preserved through the pipeline.
func (s *CreateSuite) TestManifestSourceCreate() {
	t := s.T()

	// --- Write manifest YAML to a temp directory ---

	const manifestYAML = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: myapp
  namespace: userservice
  labels:
    app: myapp
  annotations:
    bridge.dev/test-annotation: e2e-test-value
spec:
  replicas: 1
  selector:
    matchLabels:
      app: myapp
  template:
    metadata:
      labels:
        app: myapp
    spec:
      containers:
        - name: myapp
          image: invalid123
          ports:
            - containerPort: 8080
              protocol: TCP
---
apiVersion: v1
kind: Service
metadata:
  name: svc
  namespace: userservice
spec:
  selector:
    app: myapp
  ports:
    - port: 80
      targetPort: 8080
      protocol: TCP
`

	manifestDir, err := os.MkdirTemp(os.TempDir(), "bridge-manifest-test-*")
	require.NoError(t, err)
	manifestDir, err = filepath.EvalSymlinks(manifestDir)
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(manifestDir) })

	require.NoError(t, os.WriteFile(filepath.Join(manifestDir, "manifest.yaml"), []byte(manifestYAML), 0644))

	// --- Create workspace and set up environment ---

	dir := createWorkspace(t, s.bridgeBin, s.projectRoot, s.cluster)

	origDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { os.Chdir(origDir) })

	_, err = identity.EnsureDeviceID()
	require.NoError(t, err)

	deviceID, _ := identity.GetDeviceID()
	bridgeName := identity.BridgeResourceName(deviceID, "myapp")

	t.Cleanup(func() {
		container.NewDockerClient().StopAll(s.ctx, container.StopAllOpts{Labels: bridgeLabels(bridgeName)})
		os.RemoveAll(dir)
	})

	// --- Run `bridge create --source <manifest-dir>` ---
	// No --admin-addr: the default (k8spf:///administrator.bridge:9090?workload=deployment)
	// should auto-discover the administrator deployed by SetupSuite.

	stdinR, stdinW, err := os.Pipe()
	require.NoError(t, err)
	defer stdinW.Close()

	stdoutR, stdoutW, err := os.Pipe()
	require.NoError(t, err)

	app, args := s.newBridgeCreateApp(stdinR, stdoutW, dir,
		"--source", manifestDir)

	errCh := make(chan error, 1)
	go func() {
		defer stdoutW.Close()
		errCh <- app.Run(s.ctx, args)
	}()

	go func() {
		scanner := bufio.NewScanner(stdoutR)
		for scanner.Scan() {
			t.Logf("[bridge] %s", scanner.Text())
		}
	}()

	// --- Wait for bridge pod readiness ---

	bridgeNS := testutil.UserserviceNamespace

	// Wait for the bridge pod to be ready in the cluster.
	pod, err := kube.WaitForPod(s.ctx, s.cluster.Clientset, bridgeNS, meta.DeploymentSelector(bridgeName), 1*time.Minute)
	require.NoError(t, err, "bridge pod not ready")
	t.Logf("Bridge pod ready: %s", pod.Name)

	ct := container.NewDockerClient()
	containerID, err := container.WaitForID(s.ctx, ct, container.FindOpts{Labels: bridgeLabels(bridgeName)})
	require.NoError(t, err, "container not found")
	require.NoError(t, intercept.WaitForReady(s.ctx, ct, containerID), "intercept not ready")

	// --- Verify annotation preserved on the bridge deployment ---

	dep, err := s.cluster.Clientset.AppsV1().Deployments(bridgeNS).Get(s.ctx, bridgeName, metav1.GetOptions{})
	require.NoError(t, err, "failed to get bridge deployment")
	require.Equal(t, "e2e-test-value", dep.Annotations["bridge.dev/test-annotation"],
		"annotation bridge.dev/test-annotation should be preserved on the bridge deployment")

	// --- Verify network access ---

	generatedConfig := filepath.Join(dir, ".devcontainer",
		fmt.Sprintf("bridge-%s", bridgeName), "devcontainer.json")
	dc := &devcontainer.Client{
		WorkspaceFolder: dir,
		ConfigPath:      generatedConfig,
	}
	targetURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/",
		testutil.UserserviceServiceName, testutil.UserserviceNamespace, testutil.UserservicePort)

	wgetOut, wgetErr := dc.ExecOutput(s.ctx, []string{"wget", "-O", "-", "-T", "10", targetURL})
	t.Logf("[wget] output: %s", strings.TrimSpace(wgetOut))
	require.NoError(t, wgetErr, "wget failed")
	require.Contains(t, wgetOut, "ok", "expected test server response")

	// --- Verify bridge get lists the bridge ---

	getOut := s.runBridgeGet()
	t.Logf("[bridge get] output:\n%s", getOut)
	lines := strings.Split(strings.TrimSpace(getOut), "\n")
	require.GreaterOrEqual(t, len(lines), 2, "expected header + at least 1 bridge entry")
	var found bool
	for _, line := range lines[1:] {
		if strings.HasPrefix(strings.TrimSpace(line), bridgeName) {
			found = true
			break
		}
	}
	require.True(t, found, "expected bridge %q in output:\n%s", bridgeName, getOut)

	// --- Clean up: close stdin so bash exits ---

	stdinW.Close()

	require.NoError(t, <-errCh, "bridge create --source failed")
}

// TestExec verifies the `bridge exec` flow: first creates k8s resources via
// `bridge create` (without --connect), then uses `bridge exec` to auto-start
// the devcontainer and run a command that exercises the network tunnel.
func (s *CreateSuite) TestExec() {
	t := s.T()

	// --- Setup ---

	dir := createWorkspace(t, s.bridgeBin, s.projectRoot, s.cluster)

	origDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { os.Chdir(origDir) })

	_, err = identity.EnsureDeviceID()
	require.NoError(t, err)

	deviceID, _ := identity.GetDeviceID()
	bridgeName := identity.BridgeResourceName(deviceID, testutil.UserserviceName)

	t.Cleanup(func() {
		container.NewDockerClient().StopAll(s.ctx, container.StopAllOpts{Labels: bridgeLabels(bridgeName)})
		os.RemoveAll(dir)
	})

	// --- Step 1: Run `bridge create` WITHOUT --connect to set up k8s resources ---

	adminAddr := fmt.Sprintf("k8spf:///%s.%s:9090", s.adminPod.Name, testutil.AdministratorNamespace)
	baseConfig := filepath.Join(dir, ".devcontainer", "devcontainer.json")

	createApp := commands.NewApp()
	createApp.Reader = strings.NewReader("")
	createApp.Writer = io.Discard

	createArgs := []string{
		"bridge", "create", testutil.UserserviceName,
		"-n", testutil.UserserviceNamespace,
		"--yes",
		"--feature-ref", "../local-features/bridge-feature",
		"--container-binary-path", s.bridgeBin,
		"--proxy-image", s.administratorRef,
		"-f", baseConfig,
		"--admin-addr", adminAddr,
	}
	err = createApp.Run(s.ctx, createArgs)
	require.NoError(t, err, "bridge create failed")
	t.Log("bridge create (no --connect) completed")

	// Verify the generated devcontainer config exists.
	generatedConfig := filepath.Join(dir, ".devcontainer",
		fmt.Sprintf("bridge-%s", bridgeName), "devcontainer.json")
	_, err = os.Stat(generatedConfig)
	require.NoError(t, err, "bridge create should have generated devcontainer config")

	// Wait for the bridge pod to be ready before running exec.
	pod, err := kube.WaitForPod(s.ctx, s.cluster.Clientset, testutil.UserserviceNamespace,
		meta.DeploymentSelector(bridgeName), 1*time.Minute)
	require.NoError(t, err, "bridge pod not ready")
	t.Logf("Bridge pod ready: %s", pod.Name)

	// --- Step 2: Run `bridge exec` to auto-start devcontainer and execute command ---

	targetURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/",
		testutil.UserserviceServiceName, testutil.UserserviceNamespace, testutil.UserservicePort)

	execApp := commands.NewApp()
	execApp.Writer = io.Discard

	// Redirect os.Stdout to capture exec output — execInDevcontainer
	// writes directly to os.Stdout, not to app.Writer.
	origStdout := os.Stdout
	stdoutR, stdoutW, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = stdoutW

	execArgs := []string{
		"bridge", "exec",
		testutil.UserserviceName,
		"--", "wget", "-O", "-", "-T", "10", targetURL,
	}
	execErr := execApp.Run(s.ctx, execArgs)

	stdoutW.Close()
	os.Stdout = origStdout

	output, _ := io.ReadAll(stdoutR)
	t.Logf("[exec] output: %s", strings.TrimSpace(string(output)))

	require.NoError(t, execErr, "bridge exec failed")
	require.Contains(t, string(output), "ok", "expected test server response from bridge exec")
}

// runBridgeGet executes `bridge get` with the given extra args and returns
// the captured stdout. It uses the admin-addr derived from the administrator
// pod deployed by SetupSuite.
func (s *CreateSuite) runBridgeGet(extraArgs ...string) string {
	s.T().Helper()

	var buf bytes.Buffer
	app := commands.NewApp()
	app.Writer = &buf

	args := []string{"bridge", "get"}
	args = append(args, extraArgs...)

	require.NoError(s.T(), app.Run(s.ctx, args), "bridge get failed")
	return buf.String()
}

// TestCreateFailsMissingBinary verifies that bridge create --connect fails
// with a clear error when the linux bridge binary is missing. This is a
// standalone test — no cluster or suite setup needed.
func TestCreateFailsMissingBinary(t *testing.T) {
	// Create a minimal workspace with a devcontainer.json.
	dir, err := os.MkdirTemp(os.TempDir(), "bridge-missing-bin-test-*")
	require.NoError(t, err)
	dir, err = filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })

	dcDir := filepath.Join(dir, ".devcontainer")
	require.NoError(t, os.MkdirAll(dcDir, 0755))

	cfg := &devcontainer.Config{
		Image: "mcr.microsoft.com/devcontainers/base:alpine-3.20",
	}
	require.NoError(t, cfg.Save(filepath.Join(dcDir, "devcontainer.json")))

	origDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { os.Chdir(origDir) })

	// Ensure device identity exists.
	_, err = identity.EnsureDeviceID()
	require.NoError(t, err)

	// Point --container-binary-path at a non-existent file to trigger
	// the pre-flight check before any cluster work happens.
	fakeBin := filepath.Join(dir, "nonexistent", "bridge-linux")

	app := commands.NewApp()
	app.Reader = strings.NewReader("")
	app.Writer = io.Discard

	err = app.Run(context.Background(), []string{
		"bridge", "create", "fake-deploy",
		"-n", "default",
		"--yes",
		"--connect",
		"--container-binary-path", fakeBin,
		"-f", filepath.Join(dcDir, "devcontainer.json"),
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "bridge-linux", "error should mention the missing binary path")
	require.Contains(t, err.Error(), "install", "error should include install instructions")
}

// TestReactor verifies that --server-mock injects a reactor into the bridge
// proxy. Requests matching the reactor route get a mock response; unmatched
// requests are forwarded to the real destination.
func (s *CreateSuite) TestReactor() {
	t := s.T()

	reactorJSON := `{"host":"google.com","routes":[{"match":{"cel":"request.path == '/mocked'"},"action":{"http_response":{"status":200,"headers":{"content-type":"application/json"},"body":{"mocked":true,"source":"bridge-reactor"}}}}]}`

	// --- Setup ---

	dir := createWorkspace(t, s.bridgeBin, s.projectRoot, s.cluster)

	// Write reactor spec to a file so the CLI flag doesn't split on commas.
	reactorFile := filepath.Join(dir, "reactor.json")
	require.NoError(t, os.WriteFile(reactorFile, []byte(reactorJSON), 0644))

	origDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { os.Chdir(origDir) })

	deviceID, _ := identity.GetDeviceID()
	bridgeName := identity.BridgeResourceName(deviceID, testutil.UserserviceName)

	t.Cleanup(func() {
		container.NewDockerClient().StopAll(s.ctx, container.StopAllOpts{Labels: bridgeLabels(bridgeName)})
		os.RemoveAll(dir)
	})

	// --- Run `bridge create --connect --server-mock <json>` ---

	stdinR, stdinW, err := os.Pipe()
	require.NoError(t, err)
	stdoutR, stdoutW, err := os.Pipe()
	require.NoError(t, err)

	adminAddr := fmt.Sprintf("k8spf:///%s.%s:9090", s.adminPod.Name, testutil.AdministratorNamespace)

	app, args := s.newBridgeCreateApp(stdinR, stdoutW, dir,
		testutil.UserserviceName, "--admin-addr", adminAddr, "--server-mocks", reactorFile)

	errCh := make(chan error, 1)
	go func() {
		defer stdoutW.Close()
		errCh <- app.Run(s.ctx, args)
	}()

	go func() {
		scanner := bufio.NewScanner(stdoutR)
		for scanner.Scan() {
			t.Logf("[bridge] %s", scanner.Text())
		}
	}()

	// --- Wait for bridge pod and intercept readiness ---

	bridgeNS := testutil.UserserviceNamespace

	pod, err := kube.WaitForPod(s.ctx, s.cluster.Clientset, bridgeNS, meta.DeploymentSelector(bridgeName), 1*time.Minute)
	require.NoError(t, err, "bridge pod not ready")
	t.Logf("Bridge pod ready: %s", pod.Name)

	ct := container.NewDockerClient()
	containerID, err := container.WaitForID(s.ctx, ct, container.FindOpts{Labels: bridgeLabels(bridgeName)})
	require.NoError(t, err, "container not found")
	require.NoError(t, intercept.WaitForReady(s.ctx, ct, containerID), "intercept not ready")

	// --- Verify reactor: /mocked returns the mock response ---

	generatedConfig := filepath.Join(dir, ".devcontainer",
		fmt.Sprintf("bridge-%s", bridgeName), "devcontainer.json")
	dc := &devcontainer.Client{
		WorkspaceFolder: dir,
		ConfigPath:      generatedConfig,
	}

	// Pre-warm DNS: ensure bridge DNS is resolving google.com through the tunnel.
	dc.ExecOutput(s.ctx, []string{"nslookup", "google.com", "127.0.0.1"})
	time.Sleep(1 * time.Second)

	// Verify DNS is resolving through bridge (should get a proxy IP, not a real IP).
	resolveOut, _ := dc.ExecOutput(s.ctx, []string{"nslookup", "google.com"})
	t.Logf("[nslookup] output: %s", strings.TrimSpace(resolveOut))

	mockedOut, mockedErr := dc.ExecOutput(s.ctx, []string{"wget", "-O", "-", "-T", "10", "http://google.com/mocked"})
	t.Logf("[wget /mocked] output: %s", strings.TrimSpace(mockedOut))
	require.NoError(t, mockedErr, "wget /mocked failed")
	require.Contains(t, mockedOut, "bridge-reactor", "expected reactor mock response")

	// --- Verify passthrough: / forwards to real google.com ---

	realOut, realErr := dc.ExecOutput(s.ctx, []string{"wget", "-O", "-", "-T", "10", "http://google.com/"})
	t.Logf("[wget /] output_len: %d", len(realOut))
	require.NoError(t, realErr, "wget / failed")
	require.Contains(t, strings.ToLower(realOut), "<html", "expected HTML from real google.com")

	// --- Clean up ---

	stdinW.Close()

	require.NoError(t, <-errCh, "bridge create --connect failed")
}

func TestCreateSuite(t *testing.T) {
	suite.Run(t, new(CreateSuite))
}
