package intercept

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/vercel/bridge/pkg/container"
)

// WaitForReady polls the intercept log inside the container until it sees
// "Intercept ready" or "Intercept crashed". The caller controls the timeout
// via the context.
func WaitForReady(ctx context.Context, ct container.Client, containerID string) error {
	const (
		poll    = 500 * time.Millisecond
		logPath = "/tmp/bridge-intercept.log"
	)

	for {
		if ctx.Err() != nil {
			log := ct.ReadFile(ctx, containerID, logPath)
			if log == "" {
				return fmt.Errorf("container failed to start (no logs available): %w", ctx.Err())
			}
			return fmt.Errorf("container failed to start:\n%s\n%w", logTail(log, 10), ctx.Err())
		}

		log := ct.ReadFile(ctx, containerID, logPath)
		if strings.Contains(log, "Intercept ready") {
			return nil
		}
		if strings.Contains(log, "Intercept crashed") {
			return fmt.Errorf("container failed to start:\n%s", logTail(log, 10))
		}

		time.Sleep(poll)
	}
}

// logTail returns the last n lines of s.
func logTail(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
