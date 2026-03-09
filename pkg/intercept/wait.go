package intercept

import (
	"context"
	"time"

	"github.com/vercel/bridge/pkg/container"
	"github.com/vercel/bridge/pkg/grpcutil"
)

// WaitForReady connects to the intercept gRPC server inside the container and
// polls the standard health check until it reports SERVING. The caller controls
// the timeout via the context.
func WaitForReady(ctx context.Context, ct container.Client, containerID string) error {
	conn, err := Connect(ctx, ct, containerID)
	if err != nil {
		return err
	}
	defer conn.Close()

	return grpcutil.WaitForHealthy(ctx, conn, 500*time.Millisecond)
}
