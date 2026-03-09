package intercept

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/vercel/bridge/pkg/container"
	"github.com/vercel/bridge/pkg/k8s/meta"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Connect returns a gRPC client connection to the intercept server running
// inside the given container. It reads BRIDGE_INTERCEPT_ADDR from the
// container's environment and dials localhost on that port.
func Connect(ctx context.Context, ct container.Client, containerID string) (*grpc.ClientConn, error) {
	out, err := ct.Exec(ctx, containerID, "sh", "-c", "echo $"+meta.EnvInterceptorAddr)
	if err != nil {
		return nil, fmt.Errorf("%s not set in container: %w", meta.EnvInterceptorAddr, err)
	}

	addr := strings.TrimSpace(out)
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid %s %q: %w", meta.EnvInterceptorAddr, addr, err)
	}

	target := net.JoinHostPort("localhost", port)
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to intercept server at %s: %w", target, err)
	}
	return conn, nil
}
