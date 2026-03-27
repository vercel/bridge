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
	sandboxClient sandbox.Client
	vercelClient  vercel.Client
	sandboxID     string
	routes        []sandbox.Route
	ctx           context.Context
	cancel        context.CancelFunc
	bridgeBin     string
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
	s.ctx, s.cancel = context.WithTimeout(context.Background(), 20*time.Minute)
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

	s.bridgeBin = s.buildBridge()

	sbx, routes, err := s.sandboxClient.Create(s.ctx, sandbox.CreateOpts{
		ProjectID: projectID,
		Ports:     []int{8081},
		Runtime:   "node24",
		Timeout:   10 * time.Minute,
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
		Name:           project.ProjectName,
		Project:        project.ProjectName,
		Target:         "preview",
		GitSource:      gitSource,
		Env:            map[string]string{"BRIDGE_SERVER_ADDR": interceptorURL},
		ForceNew:       true,
		SkipAutoDetect: true,
	})
	require.NoError(t, err, "failed to create dispatcher deployment")

	t.Logf("Dispatcher deployment created: id=%s url=%s", deployment.ID, deployment.URL)

	deployment, err = s.vercelClient.WaitDeployment(s.ctx, deployment.ID, 5*time.Second)
	require.NoError(t, err, "failed while waiting for dispatcher deployment")
	require.Equalf(t, vercel.DeploymentStateReady, deployment.ReadyState, "dispatcher deployment not ready: code=%s message=%s", deployment.ErrorCode, deployment.ErrorMessage)

	previewURL := deployment.URL
	if !strings.HasPrefix(previewURL, "http://") && !strings.HasPrefix(previewURL, "https://") {
		previewURL = "https://" + previewURL
	}

	s.startInterceptor(previewURL)
	s.waitForSandboxPort("127.0.0.1:8081", 30*time.Second)
	s.waitForSandboxPort("127.0.0.1:1080", 30*time.Second)

	s.cloneRepoInSandbox(gitSource.SHA)
	s.installUserserviceDeps()
	s.startUserservice()
	s.waitForUserservice()

	require.Eventually(t, func() bool {
		resp, body, err := previewGET(previewURL + "/api/health")
		if err != nil {
			t.Logf("preview request failed: %v", err)
			return false
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			if os.Getenv("VERCEL_AUTOMATION_BYPASS_SECRET") == "" {
				t.Skip("preview deployment is protected and VERCEL_AUTOMATION_BYPASS_SECRET is not set")
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
	}, 2*time.Minute, 5*time.Second, "dispatcher preview did not forward /api/health to the sandbox userservice")

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

	_, err := s.sandboxClient.RunCommand(s.ctx, s.sandboxID, sandbox.RunCommandOpts{
		Command: "/vercel/sandbox/bridge",
		Args: []string{
			"intercept",
			"--server-addr", previewURL,
			"--intercept-mode", "socks",
			"--socks-addr", ":1080",
			"--app-port", "3000",
			"--addr", ":8081",
		},
	})
	require.NoError(t, err, "failed to start interceptor")
}

func (s *DispatcherSuite) cloneRepoInSandbox(sha string) {
	t := s.T()
	t.Logf("Cloning bridge repo in sandbox at %s ...", sha)

	_, err := s.execInSandbox("bash", "-lc", fmt.Sprintf(`
set -euo pipefail
rm -rf /tmp/bridge-dispatcher-e2e
git clone https://github.com/vercel/bridge.git /tmp/bridge-dispatcher-e2e
cd /tmp/bridge-dispatcher-e2e
git checkout %s
`, shellQuote(sha)))
	require.NoError(t, err, "failed to clone repo in sandbox")
}

func (s *DispatcherSuite) installUserserviceDeps() {
	t := s.T()
	t.Log("Installing userservice dependencies in sandbox...")

	_, err := s.execInSandbox("bash", "-lc", `
set -euo pipefail
cd /tmp/bridge-dispatcher-e2e/flow/userservice
npm ci
`)
	require.NoError(t, err, "failed to install userservice dependencies")
}

func (s *DispatcherSuite) startUserservice() {
	t := s.T()
	t.Log("Starting userservice via vercel dev in sandbox...")

	_, err := s.sandboxClient.RunCommand(s.ctx, s.sandboxID, sandbox.RunCommandOpts{
		Command: "bash",
		Args: []string{"-lc", `
set -euo pipefail
cd /tmp/bridge-dispatcher-e2e/flow/userservice
export ALL_PROXY=socks5://127.0.0.1:1080
export HTTP_PROXY=socks5://127.0.0.1:1080
export HTTPS_PROXY=socks5://127.0.0.1:1080
export NO_PROXY=127.0.0.1,localhost,::1
npx vercel dev --listen 3000
`},
	})
	require.NoError(t, err, "failed to start userservice")
}

func (s *DispatcherSuite) waitForUserservice() {
	t := s.T()
	t.Log("Waiting for userservice health endpoint...")

	require.Eventually(t, func() bool {
		_, err := s.execInSandbox("curl", "-sf", "http://127.0.0.1:3000/api/health")
		return err == nil
	}, 2*time.Minute, 5*time.Second, "userservice did not become healthy")
}

func (s *DispatcherSuite) waitForSandboxPort(addr string, timeout time.Duration) {
	s.T().Helper()

	require.Eventually(s.T(), func() bool {
		_, err := s.execInSandbox("bash", "-lc", fmt.Sprintf("echo > /dev/tcp/%s", addr))
		return err == nil
	}, timeout, time.Second, "sandbox port %s did not open", addr)
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

func previewGET(url string) (*http.Response, []byte, error) {
	client := &http.Client{
		Timeout: 20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}

	if bypass := os.Getenv("VERCEL_AUTOMATION_BYPASS_SECRET"); bypass != "" {
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

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
