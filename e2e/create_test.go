package e2e

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vercel/bridge/e2e/testutil"
	"github.com/vercel/bridge/pkg/commands"
	"github.com/vercel/bridge/pkg/devcontainer"
	"github.com/vercel/bridge/pkg/identity"
)

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
	srcFeature := filepath.Join(projectRoot, "features", "bridge")
	dstFeature := filepath.Join(dcDir, "local-features", "bridge")
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
	bridgeTag := "bridge:test"
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

	slog.Info("SetupSuite complete")
}

func (s *CreateSuite) TearDownSuite() {
	// Stop devcontainer if we started one.
	if s.workspaceDir != "" {
		dc := &devcontainer.Client{WorkspaceFolder: s.workspaceDir}
		dc.Stop(s.ctx)
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

	s.origKubeconfig = os.Getenv("KUBECONFIG")
	os.Setenv("KUBECONFIG", s.cluster.KubeConfigPath)

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

	app := commands.NewApp()
	app.Reader = stdinR
	app.Writer = stdoutW

	errCh := make(chan error, 1)
	go func() {
		defer stdoutW.Close()
		errCh <- app.Run(s.ctx, []string{
			"bridge", "create", testutil.UserserviceName,
			"-n", testutil.UserserviceNamespace,
			"--admin-addr", adminAddr,
			"--yes",
			"--connect",
			"--feature-ref", "../local-features/bridge",
			"-f", filepath.Join(s.workspaceDir, ".devcontainer", "devcontainer.json"),
		})
	}()

	// Log stdout from bridge create in the background.
	go func() {
		scanner := bufio.NewScanner(stdoutR)
		for scanner.Scan() {
			t.Logf("[bridge] %s", scanner.Text())
		}
	}()

	// --- Wait for intercept readiness via devcontainer exec ---

	generatedConfig := filepath.Join(s.workspaceDir, ".devcontainer",
		fmt.Sprintf("bridge-%s", testutil.UserserviceName), "devcontainer.json")
	dcExec := &devcontainer.Client{
		WorkspaceFolder: s.workspaceDir,
		ConfigPath:      generatedConfig,
	}

	deviceID, _ := identity.GetDeviceID()
	bridgeNS := identity.NamespaceForDevice(deviceID)

	s.Eventually(func() bool {
		// Check if the goroutine already failed.
		select {
		case err := <-errCh:
			t.Fatalf("bridge create --connect exited early: %v", err)
		default:
		}

		// Diagnostic: check if generated config exists (means CreateBridge returned).
		if _, err := os.Stat(generatedConfig); err != nil {
			t.Logf("[diag] generated config not yet created: %s", generatedConfig)

			// Check bridge namespace pods while waiting for CreateBridge.
			pods, err := s.cluster.Clientset.CoreV1().Pods(bridgeNS).List(s.ctx, metav1.ListOptions{})
			if err != nil {
				t.Logf("[diag] list pods in %s: %v", bridgeNS, err)
			} else if len(pods.Items) == 0 {
				t.Logf("[diag] no pods in namespace %s", bridgeNS)
			} else {
				for _, p := range pods.Items {
					t.Logf("[diag] pod %s phase=%s labels=%v", p.Name, p.Status.Phase, p.Labels)
					for _, cs := range p.Status.ContainerStatuses {
						t.Logf("[diag]   container %s ready=%v", cs.Name, cs.Ready)
					}
				}
			}
			return false
		}

		out, err := dcExec.ExecOutput(s.ctx, []string{"cat", "/tmp/bridge-intercept.log"})
		if err != nil {
			t.Logf("[diag] exec in devcontainer: %v | output: %s", err, strings.TrimSpace(out))
			return false
		}
		t.Logf("[diag] intercept log: %s", out)
		return strings.Contains(out, "Bridge intercept starting")
	}, 1*time.Minute, 1*time.Second, "intercept not ready")
	t.Log("Bridge intercept is ready")

	// --- Verify network access with a single wget ---

	targetURL := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/",
		testutil.UserserviceServiceName, testutil.UserserviceNamespace, testutil.UserservicePort)

	// Dump full intercept log and DNS diagnostics before attempting wget.
	logOut, _ := dcExec.ExecOutput(s.ctx, []string{"cat", "/tmp/bridge-intercept.log"})
	t.Logf("[intercept-log]\n%s", logOut)

	resolvOut, _ := dcExec.ExecOutput(s.ctx, []string{"cat", "/etc/resolv.conf"})
	t.Logf("[resolv.conf]\n%s", resolvOut)

	// Check if DNS server is actually listening.
	ssOut, _ := dcExec.ExecOutput(s.ctx, []string{"sh", "-c", "ss -ulnp | grep :53 || netstat -ulnp 2>/dev/null | grep :53 || echo 'no listener found on :53'"})
	t.Logf("[dns-listen] %s", strings.TrimSpace(ssOut))

	// Warm up the k8spf gRPC connection with a throwaway DNS query.
	// The first DNS resolution via k8spf triggers lazy port-forward setup which
	// can take >5s. This pre-warms the connection so the real wget doesn't time out.
	warmupOut, _ := dcExec.ExecOutput(s.ctx, []string{"sh", "-c", "nslookup " + testutil.UserserviceServiceName + "." + testutil.UserserviceNamespace + ".svc.cluster.local 127.0.0.1 2>&1 || true"})
	t.Logf("[warmup nslookup] %s", strings.TrimSpace(warmupOut))

	wgetOut, wgetErr := dcExec.ExecOutput(s.ctx, []string{"wget", "-O", "-", "-T", "10", targetURL})
	t.Logf("[wget] output: %s", strings.TrimSpace(wgetOut))
	require.NoError(t, wgetErr, "wget failed")
	require.Contains(t, wgetOut, "ok", "expected test server response")

	// --- Clean up: close stdin so bash exits ---

	stdinW.Close()

	require.NoError(t, <-errCh, "bridge create --connect failed")
}

func TestCreateSuite(t *testing.T) {
	suite.Run(t, new(CreateSuite))
}
