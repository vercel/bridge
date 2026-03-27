package e2esandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"github.com/vercel/bridge/pkg/flow/sandbox"
	"github.com/vercel/bridge/pkg/flow/vercel"
)

type DispatcherSuite struct {
	suite.Suite
	sandboxClient    sandbox.Client
	vercelClient     vercel.Client
	sandboxID        string
	routes           []sandbox.Route
	ctx              context.Context
	cancel           context.CancelFunc
	bridgeBin        string
	protectionBypass string
}

type vercelProjectLink struct {
	ProjectID   string `json:"projectId"`
	OrgID       string `json:"orgId"`
	ProjectName string `json:"projectName"`
}

func TestDispatcherSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping dispatcher sandbox e2e tests in short mode")
	}
	suite.Run(t, new(DispatcherSuite))
}

func (s *DispatcherSuite) SetupSuite() {
	s.ctx, s.cancel = context.WithTimeout(context.Background(), 3*time.Minute)
	t := s.T()

	token := resolveVercelToken(t)
	project := readVercelProjectLink(t, filepath.Join(projectRoot(t), "flow", "dispatcher", ".vercel", "project.json"))

	s.sandboxClient = sandbox.New(sandbox.Config{
		Token:  token,
		TeamID: teamID,
	})
	s.vercelClient = vercel.New(vercel.Config{
		Token:  token,
		TeamID: project.OrgID,
	})

	s.protectionBypass = os.Getenv("VERCEL_AUTOMATION_BYPASS_SECRET")
	if s.protectionBypass == "" {
		proj, err := s.vercelClient.GetProject(s.ctx, project.ProjectID)
		if err == nil {
			for secret, entry := range proj.ProtectionBypass {
				if entry.Scope == "automation-bypass" {
					s.protectionBypass = secret
					t.Logf("Resolved protection bypass from project API")
					break
				}
			}
		} else {
			t.Logf("Failed to get project for protection bypass: %v", err)
		}
	}

	s.bridgeBin = s.buildBridge()

	sbx, routes, err := s.sandboxClient.Create(s.ctx, sandbox.CreateOpts{
		ProjectID: projectID,
		Ports:     []int{8081},
		Runtime:   "node24",
		Timeout:   5 * time.Minute,
		Env: map[string]string{
			"ALL_PROXY": "socks5h://127.0.0.1:1080",
			"NO_PROXY":  "127.0.0.1,localhost",
		},
	})
	require.NoError(t, err, "failed to create sandbox")

	s.sandboxID = sbx.ID
	s.routes = routes

	t.Logf("Sandbox created: %s", sbx.ID)
	for _, route := range routes {
		t.Logf("  Port %d -> %s", route.Port, route.URL)
	}

	s.uploadBridge()
}

func (s *DispatcherSuite) TearDownSuite() {
	if s.sandboxID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = s.sandboxClient.Stop(ctx, s.sandboxID)
		s.T().Logf("Sandbox stopped: %s", s.sandboxID)
	}
	if s.cancel != nil {
		s.cancel()
	}
	if s.bridgeBin != "" {
		_ = os.Remove(s.bridgeBin)
	}
}

func (s *DispatcherSuite) TestDispatcherPreviewFlow() {
	t := s.T()

	project := readVercelProjectLink(t, filepath.Join(projectRoot(t), "flow", "dispatcher", ".vercel", "project.json"))
	gitSource := currentGitSource(t)
	interceptorURL := s.interceptorURL()

	deployment, err := s.vercelClient.CreateDeployment(s.ctx, vercel.CreateDeploymentOpts{
		Name:      project.ProjectName,
		Project:   project.ProjectName,
		GitSource: gitSource,
		Env:            map[string]string{"BRIDGE_SERVER_ADDR": interceptorURL},
		ForceNew:       true,
		SkipAutoDetect: true,
	})
	require.NoError(t, err, "failed to create dispatcher deployment")

	t.Logf("Dispatcher deployment created: id=%s url=%s", deployment.ID, deployment.URL)

	t.Logf("Waiting for dispatcher deployment %s to be ready...", deployment.ID)
	deployment, err = s.vercelClient.WaitDeployment(s.ctx, deployment.ID, 5*time.Second)
	require.NoError(t, err, "failed while waiting for dispatcher deployment")
	require.Equalf(t, vercel.DeploymentStateReady, deployment.ReadyState, "dispatcher deployment not ready: code=%s message=%s", deployment.ErrorCode, deployment.ErrorMessage)
	t.Logf("Dispatcher deployment ready: %s", deployment.URL)

	previewURL := deployment.URL
	if !strings.HasPrefix(previewURL, "http://") && !strings.HasPrefix(previewURL, "https://") {
		previewURL = "https://" + previewURL
	}

	s.startInterceptor(previewURL)

	if !s.pollSandboxPort("127.0.0.1:8081", 15*time.Second) {
		s.dumpSandboxDiagnostics()
		t.Fatal("interceptor port 8081 did not open")
	}
	if !s.pollSandboxPort("127.0.0.1:1080", 15*time.Second) {
		s.dumpSandboxDiagnostics()
		t.Fatal("SOCKS port 1080 did not open")
	}

	s.startSimpleAppServer()
	s.waitForAppServer()

	require.Eventually(t, func() bool {
		resp, body, err := previewGET(previewURL+"/api/health", s.protectionBypass)
		if err != nil {
			t.Logf("preview request failed: %v", err)
			return false
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			if s.protectionBypass == "" {
				t.Skip("preview deployment is protected and no bypass secret available")
			}
			t.Logf("preview request still protected: status=%d body=%s", resp.StatusCode, string(body))
			return false
		}
		if resp.StatusCode != http.StatusOK {
			t.Logf("preview request not ready: status=%d body=%s", resp.StatusCode, string(body))
			return false
		}

		var payload struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Logf("failed to decode preview response: %v body=%s", err, string(body))
			return false
		}

		return payload.Status == "ok"
	}, 60*time.Second, 3*time.Second, "dispatcher preview did not forward /api/health to the sandbox userservice")

	s.dumpSandboxDiagnostics()

	out, err := s.execInSandbox("curl", "-sf", "--max-time", "20", "--proxy", "socks5h://127.0.0.1:1080", "http://example.com")
	require.NoError(t, err, "curl through dispatcher-backed SOCKS proxy failed")
	require.NotEmpty(t, out)
}

func (s *DispatcherSuite) buildBridge() string {
	t := s.T()
	t.Log("Building bridge binary for Linux...")

	tmpFile, err := os.CreateTemp("", "bridge-dispatcher-e2e-*")
	require.NoError(t, err)
	require.NoError(t, tmpFile.Close())

	outPath := tmpFile.Name()
	cmd := exec.CommandContext(s.ctx, "go", "build", "-ldflags=-s -w", "-o", outPath, "./cmd/bridge")
	cmd.Dir = projectRoot(t)
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")

	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "failed to build bridge: %s", string(out))

	t.Logf("Bridge binary built: %s", outPath)
	return outPath
}

func (s *DispatcherSuite) uploadBridge() {
	t := s.T()
	t.Log("Uploading bridge binary to sandbox...")

	cmd := exec.CommandContext(s.ctx, "npx", "sandbox", "copy",
		s.bridgeBin,
		s.sandboxID+":/vercel/sandbox/bridge",
		"--team", teamID,
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "failed to upload bridge binary: %s", string(out))

	_, err = s.sandboxClient.RunCommandAndWait(s.ctx, s.sandboxID, sandbox.RunCommandOpts{
		Command: "chmod",
		Args:    []string{"+x", "/vercel/sandbox/bridge"},
	})
	require.NoError(t, err, "failed to chmod bridge binary")
}

func (s *DispatcherSuite) startInterceptor(previewURL string) {
	t := s.T()
	t.Logf("Starting interceptor against %s ...", previewURL)

	bypassFlag := ""
	if s.protectionBypass != "" {
		bypassFlag = fmt.Sprintf(" --protection-bypass %s", s.protectionBypass)
	}
	_, err := s.sandboxClient.RunCommand(s.ctx, s.sandboxID, sandbox.RunCommandOpts{
		Command: "bash",
		Args: []string{"-lc", fmt.Sprintf(
			"/vercel/sandbox/bridge intercept --server-addr %s --intercept-mode socks --socks-addr :1080 --app-port 3000 --addr :8081%s > /tmp/bridge-interceptor.log 2>&1",
			previewURL, bypassFlag,
		)},
	})
	require.NoError(t, err, "failed to start interceptor")
}

func (s *DispatcherSuite) startSimpleAppServer() {
	t := s.T()
	t.Log("Starting simple app server on port 3000...")

	_, err := s.sandboxClient.RunCommand(s.ctx, s.sandboxID, sandbox.RunCommandOpts{
		Command: "node",
		Args: []string{"-e", `
const http = require("http");
const url = require("url");
const server = http.createServer((req, res) => {
  const pathname = url.parse(req.url).pathname;
  if (pathname === "/api/health") {
    res.writeHead(200, {"Content-Type": "application/json"});
    res.end(JSON.stringify({status: "ok"}));
    return;
  }
  res.writeHead(404);
  res.end("not found");
});
server.listen(3000, () => console.log("app server listening on :3000"));
`},
	})
	require.NoError(t, err, "failed to start simple app server")
}

func (s *DispatcherSuite) waitForAppServer() {
	t := s.T()
	t.Log("Waiting for app server health endpoint...")

	require.Eventually(t, func() bool {
		_, err := s.execInSandbox("curl", "-sf", "http://127.0.0.1:3000/api/health")
		return err == nil
	}, 30*time.Second, 2*time.Second, "app server did not become healthy")
}

func (s *DispatcherSuite) pollSandboxPort(addr string, timeout time.Duration) bool {
	s.T().Helper()

	host, port, _ := strings.Cut(addr, ":")
	deadline := time.After(timeout)
	for {
		select {
		case <-deadline:
			return false
		default:
		}
		_, err := s.execInSandbox("bash", "-lc", fmt.Sprintf("echo > /dev/tcp/%s/%s", host, port))
		if err == nil {
			return true
		}
		time.Sleep(time.Second)
	}
}

func (s *DispatcherSuite) dumpSandboxDiagnostics() {
	t := s.T()
	t.Helper()

	if ps, err := s.execInSandbox("ps", "aux"); err == nil {
		t.Logf("=== sandbox processes ===\n%s", ps)
	}
	if logs, err := s.execInSandbox("cat", "/tmp/bridge-interceptor.log"); err == nil {
		t.Logf("=== bridge interceptor log ===\n%s", logs)
	}
	if logs, err := s.execInSandbox("bash", "-lc", "cat /home/vercel-sandbox/.bridge/logs/bridge.log 2>/dev/null || echo 'no log file'"); err == nil {
		t.Logf("=== bridge.log ===\n%s", logs)
	}
}

func (s *DispatcherSuite) interceptorURL() string {
	s.T().Helper()

	for _, route := range s.routes {
		if route.Port == 8081 {
			return route.URL
		}
	}
	s.T().Fatal("no route found for interceptor port 8081")
	return ""
}

func (s *DispatcherSuite) execInSandbox(command string, args ...string) (string, error) {
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

func readVercelProjectLink(t *testing.T, path string) vercelProjectLink {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("skipping: failed to read linked Vercel project file %s: %v", path, err)
	}

	var project vercelProjectLink
	require.NoError(t, json.Unmarshal(data, &project))
	require.NotEmpty(t, project.ProjectName)
	require.NotEmpty(t, project.OrgID)
	return project
}

func currentGitSource(t *testing.T) *vercel.GitSource {
	t.Helper()

	branch := strings.TrimSpace(runGit(t, "rev-parse", "--abbrev-ref", "HEAD"))
	sha := strings.TrimSpace(runGit(t, "rev-parse", "HEAD"))
	origin := strings.TrimSpace(runGit(t, "remote", "get-url", "origin"))

	if !strings.Contains(origin, "github.com:vercel/bridge") && !strings.Contains(origin, "github.com/vercel/bridge") {
		t.Skipf("skipping: origin remote %q is not vercel/bridge", origin)
	}

	return &vercel.GitSource{
		Type: "github",
		Org:  "vercel",
		Repo: "bridge",
		Ref:  branch,
		SHA:  sha,
	}
}

func runGit(t *testing.T, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = projectRoot(t)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s failed: %s", strings.Join(args, " "), string(out))
	return string(out)
}

func previewGET(rawURL string, bypass string) (*http.Response, []byte, error) {
	if bypass != "" {
		sep := "?"
		if strings.Contains(rawURL, "?") {
			sep = "&"
		}
		rawURL += sep + "x-vercel-protection-bypass=" + bypass + "&x-vercel-set-bypass-cookie=true"
	}

	client := &http.Client{
		Timeout: 20 * time.Second,
	}

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, err
	}

	if bypass != "" {
		req.Header.Set("x-vercel-protection-bypass", bypass)
		req.Header.Set("x-vercel-set-bypass-cookie", "true")
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		resp.Body.Close()
		return nil, nil, readErr
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, body, nil
}

