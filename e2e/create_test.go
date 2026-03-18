package e2e

import (
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
	origKubeconfig string
	workspaceDir   string // set by test, used by TearDownSuite for devcontainer cleanup
	bridgeName     string // bridge name, used to stop containers on teardown
}

// createWorkspace creates a temp workspace directory with a devcontainer.json
// that includes all test-specific config: mounts, runArgs, containerEnv.
// It also copies the bridge feature into the workspace so it can be referenced
// as a relative path (the devcontainer CLI rejects absolute feature paths).
// The working directory is changed to the workspace and restored on cleanup.
func createWorkspace(t *testing.T, bridgeBin, projectRoot string, cluster *testutil.Cluster) string {
	t.Helper()

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

	origDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() {
		os.Chdir(origDir)
		os.RemoveAll(dir)
	})

	return dir
}

// newBridgeCreateApp returns a configured CLI app and builds the base args for
// `bridge create`. Callers append test-specific args (e.g. deployment name,
// --source, --admin-addr, --connect) before the base args.
func (s *CreateSuite) newBridgeCreateApp(reader io.Reader, writer io.Writer, workspaceDir string, extraArgs ...string) (*cli.Command, []string) {
	app := commands.NewApp()
	app.Reader = reader
	app.Writer = writer

	args := []string{"bridge", "create"}
	args = append(args, extraArgs...)
	args = append(args,
		"-n", testutil.UserserviceNamespace,
		"--yes",
		"--feature-ref", "../local-features/bridge-feature",
		"--container-binary-path", s.bridgeBin,
		"--proxy-image", s.administratorRef,
		"-f", filepath.Join(workspaceDir, ".devcontainer", "devcontainer.json"),
	)
	return app, args
}

// runBridgeCreate runs `bridge create` (without --connect) and waits for the
// bridge pod to be ready.
func (s *CreateSuite) runBridgeCreate(workspaceDir, bridgeName string, extraArgs ...string) {
	s.T().Helper()

	adminAddr := fmt.Sprintf("k8spf:///%s.%s:9090", s.adminPod.Name, testutil.AdministratorNamespace)

	createApp := commands.NewApp()
	createApp.Reader = strings.NewReader("")
	createApp.Writer = io.Discard

	args := []string{"bridge", "create", bridgeName}
	args = append(args, extraArgs...)
	args = append(args,
		"-n", testutil.UserserviceNamespace,
		"--yes",
		"--feature-ref", "../local-features/bridge-feature",
		"--container-binary-path", s.bridgeBin,
		"--proxy-image", s.administratorRef,
		"-f", filepath.Join(workspaceDir, ".devcontainer", "devcontainer.json"),
		"--admin-addr", adminAddr,
	)

	err := createApp.Run(s.ctx, args)
	s.Require().NoError(err, "bridge create failed")
	s.T().Log("bridge create completed")
}

// runBridgeRemove runs `bridge remove` for the given bridge name.
func (s *CreateSuite) runBridgeRemove(bridgeName string) {
	s.T().Helper()

	adminAddr := fmt.Sprintf("k8spf:///%s.%s:9090", s.adminPod.Name, testutil.AdministratorNamespace)

	app := commands.NewApp()
	app.Reader = strings.NewReader("")
	app.Writer = io.Discard

	err := app.Run(s.ctx, []string{"bridge", "remove", bridgeName, "--yes", "--admin-addr", adminAddr})
	s.Require().NoError(err, "bridge remove failed")
	s.T().Logf("bridge remove %s completed", bridgeName)
}

// runBridgeExec runs `bridge exec <bridgeName> -- <cmd...>` and returns stdout.
func (s *CreateSuite) runBridgeExec(bridgeName string, cmd ...string) string {
	s.T().Helper()

	var buf bytes.Buffer
	app := commands.NewApp()
	app.Writer = &buf

	args := []string{"bridge", "exec", bridgeName, "--"}
	args = append(args, cmd...)

	err := app.Run(s.ctx, args)
	s.Require().NoError(err, "bridge exec failed")
	return buf.String()
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

func (s *CreateSuite) TestCreateConnect() {
	t := s.T()

	// --- Setup ---

	_, err := identity.EnsureDeviceID()
	require.NoError(t, err)

	s.workspaceDir = createWorkspace(t, s.bridgeBin, s.projectRoot, s.cluster)

	// --- Create pipes for stdin/stdout ---

	stdinR, stdinW, err := os.Pipe()
	require.NoError(t, err)
	stdoutR, stdoutW, err := os.Pipe()
	require.NoError(t, err)

	// --- Run `bridge create --connect` in a goroutine ---

	adminAddr := fmt.Sprintf("k8spf:///%s.%s:9090", s.adminPod.Name, testutil.AdministratorNamespace)

	app, args := s.newBridgeCreateApp(stdinR, stdoutW, s.workspaceDir,
		testutil.UserserviceName, "--admin-addr", adminAddr, "--connect")

	errCh := make(chan error, 1)
	go func() {
		defer stdoutW.Close()
		errCh <- app.Run(s.ctx, args)
	}()

	// Collect all stdout so we can log it and assert on it.
	var outputBuf bytes.Buffer
	go func() {
		io.Copy(&outputBuf, stdoutR)
	}()

	// --- Wait for intercept readiness ---

	bridgeName := testutil.UserserviceName
	s.bridgeName = bridgeName

	ct := container.NewDockerClient()
	containerID, err := container.WaitForID(s.ctx, ct, container.FindOpts{Labels: bridgeLabels(bridgeName)})
	require.NoError(t, err, "container not found")
	require.NoError(t, intercept.WaitForReady(s.ctx, ct, containerID), "intercept not ready")

	// --- Verify network access via stdin ---

	targetURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/",
		testutil.UserserviceServiceName, testutil.UserserviceNamespace, testutil.UserservicePort)

	fmt.Fprintf(stdinW, "wget -q -O - -T 10 %s\n", targetURL)
	fmt.Fprintln(stdinW, "exit")
	stdinW.Close()

	require.NoError(t, <-errCh, "bridge create --connect failed")

	t.Logf("[stdout] %s", outputBuf.String())
	require.Contains(t, outputBuf.String(), "ok", "expected test server response")
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

	crashBridgeName := testutil.UserserviceName
	t.Cleanup(func() {
		container.NewDockerClient().StopAll(s.ctx, container.StopAllOpts{Labels: bridgeLabels(crashBridgeName)})
		os.RemoveAll(dir)
	})

	adminAddr := fmt.Sprintf("k8spf:///%s.%s:9090", s.adminPod.Name, testutil.AdministratorNamespace)

	stdinR, stdinW, err := os.Pipe()
	require.NoError(t, err)
	defer stdinW.Close()

	app, args := s.newBridgeCreateApp(stdinR, io.Discard, dir,
		testutil.UserserviceName, "--admin-addr", adminAddr, "--connect")

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

	_, err = identity.EnsureDeviceID()
	require.NoError(t, err)

	bridgeName := "myapp"

	t.Cleanup(func() {
		container.NewDockerClient().StopAll(s.ctx, container.StopAllOpts{Labels: bridgeLabels(bridgeName)})
	})

	// --- Run `bridge create --source <manifest-dir>` (no --connect) ---

	s.runBridgeCreate(dir, bridgeName, "--source", manifestDir)

	// --- Verify annotation preserved on the bridge deployment ---

	bridgeNS := testutil.UserserviceNamespace

	deploys, err := s.cluster.Clientset.AppsV1().Deployments(bridgeNS).List(s.ctx, metav1.ListOptions{
		LabelSelector: meta.BridgeNameSelector(bridgeName, ""),
	})
	require.NoError(t, err, "failed to list bridge deployments")
	require.Len(t, deploys.Items, 1, "expected exactly one bridge deployment")
	dep := &deploys.Items[0]
	require.Equal(t, "e2e-test-value", dep.Annotations["bridge.dev/test-annotation"],
		"annotation bridge.dev/test-annotation should be preserved on the bridge deployment")

	// --- Verify network access via exec ---

	targetURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/",
		testutil.UserserviceServiceName, testutil.UserserviceNamespace, testutil.UserservicePort)

	var buf bytes.Buffer
	execApp := commands.NewApp()
	execApp.Writer = &buf

	execArgs := []string{
		"bridge", "exec",
		bridgeName,
		"--", "wget", "-O", "-", "-T", "10", targetURL,
	}
	execErr := execApp.Run(s.ctx, execArgs)
	t.Logf("[wget] output: %s", strings.TrimSpace(buf.String()))
	require.NoError(t, execErr, "wget failed")
	require.Contains(t, buf.String(), "ok", "expected test server response")

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
}

// TestExec verifies the `bridge exec` flow: first creates k8s resources via
// `bridge create` (without --connect), then uses `bridge exec` to auto-start
// the devcontainer and run a command that exercises the network tunnel.
func (s *CreateSuite) TestExec() {
	t := s.T()

	// --- Setup ---

	dir := createWorkspace(t, s.bridgeBin, s.projectRoot, s.cluster)

	_, err := identity.EnsureDeviceID()
	require.NoError(t, err)

	bridgeName := testutil.UserserviceName

	t.Cleanup(func() {
		container.NewDockerClient().StopAll(s.ctx, container.StopAllOpts{Labels: bridgeLabels(bridgeName)})
	})

	// --- Step 1: Run `bridge create` WITHOUT --connect ---

	s.runBridgeCreate(dir, bridgeName)

	// Verify the generated devcontainer config exists.
	generatedConfig := filepath.Join(dir, ".devcontainer",
		fmt.Sprintf("bridge-%s", bridgeName), "devcontainer.json")
	_, err = os.Stat(generatedConfig)
	require.NoError(t, err, "bridge create should have generated devcontainer config")

	// --- Step 2: Run `bridge exec` to auto-start devcontainer and execute command ---

	targetURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/",
		testutil.UserserviceServiceName, testutil.UserserviceNamespace, testutil.UserservicePort)

	var buf bytes.Buffer
	execApp := commands.NewApp()
	execApp.Writer = &buf

	execArgs := []string{
		"bridge", "exec",
		bridgeName,
		"--", "wget", "-O", "-", "-T", "10", targetURL,
	}
	execErr := execApp.Run(s.ctx, execArgs)
	t.Logf("[exec] output: %s", strings.TrimSpace(buf.String()))

	require.NoError(t, execErr, "bridge exec failed")
	require.Contains(t, buf.String(), "ok", "expected test server response from bridge exec")
}

// TestNameFlag verifies that --name creates a distinct bridge from the default.
// Two bridges targeting the same source workload should be independent: removing
// the first should not affect the second's ability to serve requests.
func (s *CreateSuite) TestNameFlag() {
	t := s.T()

	dir := createWorkspace(t, s.bridgeBin, s.projectRoot, s.cluster)

	_, err := identity.EnsureDeviceID()
	require.NoError(t, err)

	defaultName := testutil.UserserviceName
	customName := "custom-bridge"

	t.Cleanup(func() {
		ct := container.NewDockerClient()
		ct.StopAll(s.ctx, container.StopAllOpts{Labels: bridgeLabels(defaultName)})
		ct.StopAll(s.ctx, container.StopAllOpts{Labels: bridgeLabels(customName)})
	})

	targetURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/",
		testutil.UserserviceServiceName, testutil.UserserviceNamespace, testutil.UserservicePort)

	// --- Create two bridges targeting the same source workload ---

	s.runBridgeCreate(dir, defaultName)
	s.runBridgeCreate(dir, defaultName, "--name", customName)

	// Both should be listed as separate bridges.
	getOut := s.runBridgeGet()
	t.Logf("[bridge get]\n%s", getOut)
	require.Contains(t, getOut, defaultName, "default bridge should be listed")
	require.Contains(t, getOut, customName, "custom bridge should be listed")

	// Verify the default bridge serves requests.
	out1 := s.runBridgeExec(defaultName, "wget", "-q", "-O", "-", "-T", "10", targetURL)
	require.Contains(t, out1, "ok", "default bridge should serve requests")

	// Stop the default bridge's devcontainer so ports are freed.
	container.NewDockerClient().StopAll(s.ctx, container.StopAllOpts{Labels: bridgeLabels(defaultName)})

	// Remove the default bridge.
	s.runBridgeRemove(defaultName)

	// Verify only the custom bridge remains.
	getOut = s.runBridgeGet()
	t.Logf("[bridge get after remove]\n%s", getOut)
	// Check the NAME column: each line starts with the bridge name.
	lines := strings.Split(strings.TrimSpace(getOut), "\n")
	var names []string
	for _, line := range lines[1:] { // skip header
		if name := strings.Fields(strings.TrimSpace(line)); len(name) > 0 {
			names = append(names, name[0])
		}
	}
	require.NotContains(t, names, defaultName, "default bridge should be gone")
	require.Contains(t, names, customName, "custom bridge should still be listed")

	// The custom bridge should still serve requests.
	out2 := s.runBridgeExec(customName, "wget", "-q", "-O", "-", "-T", "10", targetURL)
	require.Contains(t, out2, "ok", "custom bridge should serve requests after default is removed")
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

// TestServerFacade verifies that --server-facade injects a server facade into the bridge
// proxy. Requests matching the facade route get a mock response; unmatched
// requests are forwarded to the real destination.
func (s *CreateSuite) TestServerFacade() {
	t := s.T()

	facadeJSON := `{"host":"google.com","routes":[{"match":{"cel":"request.path == '/mocked'"},"action":{"http_response":{"status":200,"headers":{"content-type":"application/json"},"body":{"mocked":true,"source":"bridge-facade"}}}}]}`

	// --- Setup ---

	dir := createWorkspace(t, s.bridgeBin, s.projectRoot, s.cluster)

	// Write facade spec to a file so the CLI flag doesn't split on commas.
	facadeFile := filepath.Join(dir, "facade.json")
	require.NoError(t, os.WriteFile(facadeFile, []byte(facadeJSON), 0644))

	bridgeName := testutil.UserserviceName

	t.Cleanup(func() {
		container.NewDockerClient().StopAll(s.ctx, container.StopAllOpts{Labels: bridgeLabels(bridgeName)})
	})

	// --- Run `bridge create` with --server-facade (no --connect) ---

	s.runBridgeCreate(dir, bridgeName, "--server-facade", facadeFile)

	// --- Verify facade via exec: /mocked returns the mock response ---

	execApp := commands.NewApp()

	// Pre-warm DNS: ensure bridge DNS is resolving google.com through the tunnel.
	execApp.Writer = io.Discard
	_ = execApp.Run(s.ctx, []string{"bridge", "exec", bridgeName, "--", "nslookup", "google.com", "127.0.0.1"})

	time.Sleep(1 * time.Second)

	// Verify DNS is resolving through bridge.
	var buf bytes.Buffer
	execApp.Writer = &buf
	_ = execApp.Run(s.ctx, []string{"bridge", "exec", bridgeName, "--", "nslookup", "google.com"})
	t.Logf("[nslookup] output: %s", strings.TrimSpace(buf.String()))

	// Verify /mocked returns the facade response.
	buf.Reset()
	mockedErr := execApp.Run(s.ctx, []string{"bridge", "exec", bridgeName, "--", "wget", "-O", "-", "-T", "10", "http://google.com/mocked"})
	t.Logf("[wget /mocked] output: %s", strings.TrimSpace(buf.String()))
	require.NoError(t, mockedErr, "wget /mocked failed")
	require.Contains(t, buf.String(), "bridge-facade", "expected facade mock response")

	// --- Verify passthrough: / forwards to real google.com ---

	buf.Reset()
	realErr := execApp.Run(s.ctx, []string{"bridge", "exec", bridgeName, "--", "wget", "-O", "-", "-T", "10", "http://google.com/"})
	t.Logf("[wget /] output_len: %d", buf.Len())
	require.NoError(t, realErr, "wget / failed")
	require.Contains(t, strings.ToLower(buf.String()), "<html", "expected HTML from real google.com")
}

func TestCreateSuite(t *testing.T) {
	suite.Run(t, new(CreateSuite))
}
