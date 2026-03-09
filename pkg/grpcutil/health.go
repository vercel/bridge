package grpcutil

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// WaitForHealthy polls the standard gRPC health check on conn until the
// service reports SERVING or the context is cancelled.
func WaitForHealthy(ctx context.Context, conn *grpc.ClientConn, interval time.Duration) error {
	client := healthpb.NewHealthClient(conn)

	for {
		resp, err := client.Check(ctx, &healthpb.HealthCheckRequest{})
		if err == nil && resp.GetStatus() == healthpb.HealthCheckResponse_SERVING {
			return nil
		}

		if ctx.Err() != nil {
			if err != nil {
				return fmt.Errorf("health check failed: %w", err)
			}
			return fmt.Errorf("health check timed out (status: %s)", resp.GetStatus())
		}

		time.Sleep(interval)
	}
}
