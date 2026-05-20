package commands

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"
	"google.golang.org/protobuf/types/known/timestamppb"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/container"
	"github.com/vercel/bridge/pkg/devcontainer"
	"github.com/vercel/bridge/pkg/interact"
	"github.com/vercel/bridge/pkg/intercept"
	"github.com/vercel/bridge/pkg/k8s/meta"
	"github.com/vercel/bridge/pkg/session"
)

const (
	// defaultDevTimeout is how long `bridge dev` waits for the app to become
	// healthy before giving up. Chosen to comfortably cover npm/JVM-style
	// startups without making a hung server hang the terminal forever.
	defaultDevTimeout = 120 * time.Second

	// devHealthPollInterval is how often we re-check GetStatus while waiting
	// for the dev command to become healthy.
	devHealthPollInterval = 500 * time.Millisecond

	// devKillGracePeriod is how long we give a previous dev command to exit
	// gracefully under SIGTERM before escalating to SIGKILL.
	devKillGracePeriod = 3 * time.Second
)

const devUsageText = `bridge dev <bridge name>

Starts the long-running command declared as "devCommand" in the bridge's
devcontainer.json (e.g. "pnpm dev", "go run ./cmd/server"), then polls the
interceptor's GetStatus until the source deployment's probes report the local
app as healthy.

Any previously-started dev command for the same bridge is killed first so
re-running bridge dev replaces the running server cleanly.

With --output=json, emits a CommandResult envelope (see "bridge --help").
Run "bridge schema dev-response" for the response payload schema.

Exits when, in order of precedence:
  1. The dev command exits (returns that exit code)
  2. All configured probes report healthy (success, response includes pid)
  3. --timeout elapses (returns an error)`

// Dev returns the CLI command for running the devcontainer's devCommand
// under health-check supervision.
func Dev() *cli.Command {
	return &cli.Command{
		Name:      "dev",
		Usage:     "Run the devcontainer's devCommand and wait for probes to report healthy",
		UsageText: devUsageText,
		Flags: []cli.Flag{
			&cli.DurationFlag{
				Name:    "timeout",
				Usage:   "How long to wait for the dev command to become healthy before failing",
				Value:   defaultDevTimeout,
				Sources: cli.EnvVars("BRIDGE_DEV_TIMEOUT"),
			},
		},
		Arguments: []cli.Argument{
			&cli.StringArg{
				Name:      "name",
				UsageText: "Name of the bridge to run the dev command in",
				Config:    cli.StringConfig{TrimSpace: true},
			},
		},
		Action: runDev,
	}
}

func runDev(ctx context.Context, c *cli.Command) error {
	bridgeName := c.StringArg("name")
	if bridgeName == "" {
		return fmt.Errorf("usage: bridge dev <bridge name>")
	}
	timeout := c.Duration("timeout")
	if timeout <= 0 {
		timeout = defaultDevTimeout
	}

	w := c.Root().Writer

	sess, err := session.Load(bridgeName)
	if err != nil {
		return fmt.Errorf("no bridge session found for %q — run: bridge create %s", bridgeName, bridgeName)
	}

	cfg, err := devcontainer.Load(sess.DevcontainerConfigPath)
	if err != nil {
		return fmt.Errorf("load devcontainer config: %w", err)
	}
	devCommand, err := cfg.DevCommand()
	if err != nil {
		return err
	}
	if devCommand == "" {
		return fmt.Errorf("no devCommand specified in %s — add a top-level \"devCommand\" field", sess.DevcontainerConfigPath)
	}

	ct := container.NewDockerClient()
	containerID, err := ct.FindID(ctx, container.FindOpts{Labels: map[string]string{labelBridgeDeployment: bridgeName}})
	if err != nil {
		return fmt.Errorf("no running devcontainer for bridge %q — start one with: bridge exec %s -- true", bridgeName, bridgeName)
	}
	if err := intercept.WaitForReady(ctx, ct, containerID); err != nil {
		return fmt.Errorf("interceptor is not healthy: %w", err)
	}

	dcConfigPath, err := ct.InspectLabel(ctx, containerID, meta.LabelDevcontainerConfigFile)
	if err != nil || dcConfigPath == "" {
		return fmt.Errorf("could not determine devcontainer config path for bridge %q", bridgeName)
	}

	dc := newDevcontainerClient(c, dcConfigPath)

	// Kill any previous dev command we tracked in the session. "Rerun
	// replaces the previous" — see usage text. The PID belongs to a local
	// devcontainer CLI process; when it dies the docker exec session dies
	// with it and Docker propagates SIGTERM into the container.
	if prevPid := sess.GetDevPid(); prevPid > 0 {
		slog.Info("Killing previous dev command", "pid", prevPid, "command", sess.GetDevCommand())
		// FindProcess on Unix never returns an error; on Windows it might,
		// but we ignore it — kill is best-effort and the session is cleared
		// regardless so the next invocation doesn't retry.
		if prev, err := os.FindProcess(int(prevPid)); err == nil {
			_ = prev.Signal(syscall.SIGTERM)
			deadline := time.Now().Add(devKillGracePeriod)
			for time.Now().Before(deadline) {
				if err := prev.Signal(syscall.Signal(0)); err != nil {
					break // process is gone
				}
				time.Sleep(100 * time.Millisecond)
			}
			_ = prev.Kill()
		}
		sess.DevPid = 0
		sess.DevStartedAt = nil
		_ = session.Write(sess)
	}

	conn, err := intercept.Connect(ctx, ct, containerID)
	if err != nil {
		return fmt.Errorf("connect to interceptor: %w", err)
	}
	defer conn.Close()
	interceptor := bridgev1.NewInterceptorServiceClient(conn)

	logPath, err := devLogPath(bridgeName)
	if err != nil {
		return fmt.Errorf("prepare log file: %w", err)
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	// We don't Close logFile here — the subprocess keeps writing to it after
	// we return. The OS reclaims the fd when our process exits.

	// Background context so the subprocess outlives the CLI invocation.
	// Cancellation flows through waitForDevHealthy via ctx instead, and we
	// signal the subprocess explicitly on the failure path below.
	cmd := dc.NewExec(context.Background(), []string{"sh", "-c", devCommand})
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Setsid puts the child in its own session so a SIGHUP from our exit
	// can't cascade to it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start dev command: %w", err)
	}
	pid := int32(cmd.Process.Pid)
	slog.Info("Dev command started", "pid", pid, "command", devCommand, "log", logPath)

	// Track exit code in the background. exitCh fires when the subprocess
	// exits, whether the wait was triggered by us killing it or by a natural
	// exit. After we return from runDev the goroutine is abandoned; the OS
	// reaps the subprocess via init reparenting.
	exitCh := make(chan int32, 1)
	go func() {
		_ = cmd.Wait()
		exitCh <- int32(cmd.ProcessState.ExitCode())
	}()

	p := interact.NewPrinter(w)
	p.Printlnf("Started: %s (pid %d)", devCommand, pid)
	p.Printlnf("Logs:    %s", logPath)
	p.Muted("Waiting for probes to report healthy...")

	// Stream the log file into the same viewport bridge create uses for
	// `devcontainer up` output. The subprocess writes directly to the
	// regular file (so it survives our exit); the viewport tails the file
	// and renders lines while we wait. NewViewport already returns a
	// no-op writing to io.Discard in JSON mode, so no IsJSON branch here.
	// We cancel the viewport's context once waitForDevHealthy returns so
	// the program tears down cleanly before we print the final result.
	viewerCtx, cancelViewer := context.WithCancel(ctx)
	viewerDone := make(chan struct{})
	go func() {
		defer close(viewerDone)
		tail, openErr := openTailReader(viewerCtx, logPath)
		if openErr != nil {
			return
		}
		defer tail.Close()
		vp := interact.NewViewport(w, interact.ViewportOpts{Title: fmt.Sprintf("Running: %s", devCommand)})
		_ = vp.Run(viewerCtx, tail)
	}()

	resp, runErr := waitForDevHealthy(ctx, exitCh, interceptor, pid, timeout)

	cancelViewer()
	<-viewerDone

	// Pre-populate context fields so the response carries them on every
	// path — including failure paths, so the developer can still see the
	// PID and log location in the JSON envelope or error message.
	resp.BridgeName = bridgeName
	resp.Command = devCommand
	resp.LogPath = logPath

	// Persist the PID regardless of outcome. On success the next `bridge
	// dev` knows what to kill before launching a fresh server; on failure
	// the PID is still useful for forensics, and the next invocation will
	// no-op on the (now dead) PID before starting over.
	sess.DevPid = pid
	sess.DevCommand = devCommand
	sess.DevStartedAt = timestamppb.Now()
	if err := session.Write(sess); err != nil {
		slog.Warn("Failed to persist dev session state", "error", err)
	}

	if runErr != nil {
		// If the dev command exited on its own (reason == exited), the
		// process is already gone — nothing to kill. Otherwise (timeout,
		// context cancel) the process is still alive but not useful, so
		// signal it and use the same exitCh to learn when it's down,
		// escalating to SIGKILL if it doesn't go quietly.
		if resp.GetReason() != bridgev1.DevCommandReason_exited {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-exitCh:
			case <-time.After(devKillGracePeriod):
				_ = cmd.Process.Kill()
			}
		}
		if interact.IsJSON() {
			// JSON consumers get the structured response (including the PID)
			// plus the error message in the same envelope. Exit code is 0,
			// matching the pattern used by `bridge remove`.
			return writeResult(w, resp, fmt.Sprintf("%s (pid %d, logs at %s)", runErr, pid, logPath))
		}
		return fmt.Errorf("%w (pid %d, logs at %s)", runErr, pid, logPath)
	}

	if interact.IsJSON() {
		return writeResult(w, resp, "")
	}
	p.Success(fmt.Sprintf("Healthy (pid %d)", resp.GetPid()))
	return nil
}

// waitForDevHealthy races three signals: the dev command exiting, all probes
// reporting healthy, and the timeout expiring. The dev process's PID is
// pre-populated on the response so the caller only has to set
// context-level fields (bridge name, command, log path).
func waitForDevHealthy(
	parent context.Context,
	exitCh <-chan int32,
	interceptor bridgev1.InterceptorServiceClient,
	pid int32,
	timeout time.Duration,
) (*bridgev1.DevCommandResponse, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(devHealthPollInterval)
	defer ticker.Stop()

	resp := &bridgev1.DevCommandResponse{Pid: pid}

	// First check: if the dev command exited (or probes are healthy)
	// immediately, return without waiting a tick.
	if r, err := evaluateDevState(parent, exitCh, interceptor, resp); r != nil || err != nil {
		return r, err
	}

	for {
		select {
		case <-parent.Done():
			return resp, parent.Err()
		case code := <-exitCh:
			resp.Reason = bridgev1.DevCommandReason_exited
			resp.ExitCode = &code
			return resp, fmt.Errorf("dev command exited with code %d before becoming healthy", code)
		case <-deadline.C:
			resp.Reason = bridgev1.DevCommandReason_timeout
			return resp, fmt.Errorf("timed out after %s waiting for probes to report healthy", timeout)
		case <-ticker.C:
			if r, err := evaluateDevState(parent, exitCh, interceptor, resp); r != nil || err != nil {
				return r, err
			}
		}
	}
}

// evaluateDevState checks the two non-timeout exit conditions in their
// documented precedence: exit-first, then health. Returns (nil, nil) when no
// terminal condition has been reached yet.
func evaluateDevState(
	ctx context.Context,
	exitCh <-chan int32,
	interceptor bridgev1.InterceptorServiceClient,
	resp *bridgev1.DevCommandResponse,
) (*bridgev1.DevCommandResponse, error) {
	select {
	case code := <-exitCh:
		resp.Reason = bridgev1.DevCommandReason_exited
		resp.ExitCode = &code
		return resp, fmt.Errorf("dev command exited with code %d before becoming healthy", code)
	default:
	}

	if healthy, statusErr := allProbesHealthy(ctx, interceptor); statusErr == nil && healthy {
		resp.Reason = bridgev1.DevCommandReason_healthy
		return resp, nil
	}
	return nil, nil
}

// allProbesHealthy fires a fresh, synchronous probe check via the
// interceptor's Probe RPC and reports whether every configured probe
// passed THIS check. Using Probe (not GetStatus) avoids stale state from
// previous dev sessions — the threshold-tracked Health surfaced by
// GetStatus would still report HEALTHY immediately after we killed and
// restarted the dev process, which would make bridge dev return success
// before the new server is actually up.
//
// When the source deployment declares no probes at all, we treat that as
// healthy immediately — there's nothing concrete to wait on and most dev
// flows don't bother adding probes to the source.
func allProbesHealthy(ctx context.Context, c bridgev1.InterceptorServiceClient) (bool, error) {
	resp, err := c.Probe(ctx, &bridgev1.ProbeRequest{})
	if err != nil {
		return false, err
	}
	configured := 0
	passed := 0
	for _, r := range []*bridgev1.ProbeCheckResult{resp.GetLiveness(), resp.GetReadiness(), resp.GetStartup()} {
		if r == nil {
			continue
		}
		configured++
		if r.GetPassed() {
			passed++
		}
	}
	if configured == 0 {
		return true, nil
	}
	return passed == configured, nil
}

// openTailReader returns a tail-style io.ReadCloser over `path`: Read returns
// available bytes immediately, and on EOF it waits briefly and retries
// instead of returning io.EOF — so the reader behaves like `tail -f` until
// ctx is cancelled. Used to feed the dev command's log file into
// interact.Viewport while we wait for probes to become healthy.
func openTailReader(ctx context.Context, path string) (io.ReadCloser, error) {
	// The file may not exist yet for a tick after the subprocess starts;
	// retry briefly so we don't lose the very first lines.
	var (
		f   *os.File
		err error
	)
	for i := 0; i < 20; i++ {
		f, err = os.Open(path)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if f == nil {
		return nil, err
	}
	return &tailReader{ctx: ctx, f: f}, nil
}

type tailReader struct {
	ctx context.Context
	f   *os.File
}

func (t *tailReader) Read(p []byte) (int, error) {
	for {
		if err := t.ctx.Err(); err != nil {
			return 0, io.EOF
		}
		n, err := t.f.Read(p)
		if n > 0 {
			return n, nil
		}
		if err == io.EOF {
			select {
			case <-t.ctx.Done():
				return 0, io.EOF
			case <-time.After(100 * time.Millisecond):
			}
			continue
		}
		return n, err
	}
}

func (t *tailReader) Close() error { return t.f.Close() }

// devLogPath returns the path bridge dev should redirect the dev command's
// stdout/stderr to. Lives under ~/.bridge/logs/ so it's discoverable from
// the developer's machine and survives container restarts.
func devLogPath(bridgeName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".bridge", "logs")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("dev-%s.log", bridgeName)), nil
}

// newDevcontainerClient builds a CLIClient pointed at the devcontainer's
// workspace folder, wired with the root command's IO streams. The workspace
// folder is inferred from the devcontainer config path by walking up two
// levels (matching how bridge exec resolves it).
func newDevcontainerClient(c *cli.Command, configPath string) devcontainer.Client {
	workspaceFolder, _ := filepath.Abs(filepath.Dir(filepath.Dir(filepath.Dir(configPath))))
	root := c.Root()
	return &devcontainer.CLIClient{
		WorkspaceFolder: workspaceFolder,
		ConfigPath:      configPath,
		Stdin:           root.Reader,
		Stdout:          root.Writer,
		Stderr:          root.ErrWriter,
	}
}
