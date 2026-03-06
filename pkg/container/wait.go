package container

import (
	"context"
	"fmt"
	"time"
)

// WaitForID polls for a running container matching opts until one is found or
// the context is cancelled. The caller controls the timeout via the context.
func WaitForID(ctx context.Context, ct Client, opts FindOpts) (string, error) {
	const poll = 500 * time.Millisecond

	for {
		id, err := ct.FindID(ctx, opts)
		if err == nil {
			return id, nil
		}
		if ctx.Err() != nil {
			return "", fmt.Errorf("no container found matching labels %v: %w", opts.Labels, ctx.Err())
		}
		time.Sleep(poll)
	}
}
