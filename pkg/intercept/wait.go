package intercept

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/vercel/bridge/pkg/devcontainer"
)

// WaitForReady polls the intercept log inside the devcontainer until it
// sees "Intercept ready" or "Intercept crashed". If the devcontainer is not
// yet running, it retries until the container appears. Returns an error if
// the timeout elapses or the intercept crashes.
func WaitForReady(ctx context.Context, dc *devcontainer.Client) error {
	const (
		timeout = 60 * time.Second
		poll    = 500 * time.Millisecond
		logPath = "/tmp/bridge-intercept.log"
	)
	deadline := time.Now().Add(timeout)

	var containerID string

	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if containerID == "" {
			id, err := dc.ContainerID(ctx)
			if err != nil {
				time.Sleep(poll)
				continue
			}
			containerID = id
		}

		log := readLog(ctx, containerID, logPath)
		if strings.Contains(log, "Intercept ready") {
			return nil
		}
		if strings.Contains(log, "Intercept crashed") {
			return fmt.Errorf("container failed to start:\n%s", logTail(log, 10))
		}

		time.Sleep(poll)
	}

	if containerID == "" {
		return fmt.Errorf("devcontainer not found within %s", timeout)
	}

	log := readLog(ctx, containerID, logPath)
	if log == "" {
		return fmt.Errorf("container failed to start within %s (no logs available)", timeout)
	}
	return fmt.Errorf("container failed to start within %s:\n%s", timeout, logTail(log, 10))
}

func readLog(ctx context.Context, containerID, logPath string) string {
	cmd := exec.CommandContext(ctx, "docker", "exec", containerID, "cat", logPath)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// logTail returns the last n lines of s.
func logTail(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
