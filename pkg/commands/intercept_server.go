package commands

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync/atomic"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/grpcutil"
	"github.com/vercel/bridge/pkg/probe"
	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// interceptServer exposes a gRPC server for the intercept process with a
// health check endpoint that reports whether the interceptor is initialized,
// plus an InterceptorService that surfaces probe-monitor results.
//
// State is fully lock-free: `ready` is an atomic.Bool and the configured
// probe monitors are held behind an atomic.Pointer to an immutable bundle.
// GetStatus calls Status() on each monitor — which itself reads from an
// atomic — so the whole read path involves zero locks.
type interceptServer struct {
	healthpb.UnimplementedHealthServer
	bridgev1.UnimplementedInterceptorServiceServer

	server *grpc.Server

	ready    atomic.Bool
	monitors atomic.Pointer[monitorSet]
}

// monitorSet is the immutable bundle stored in interceptServer.monitors.
// Each field is nil if the source deployment had no probe of that kind.
type monitorSet struct {
	liveness  probe.Monitor
	readiness probe.Monitor
	startup   probe.Monitor
}

func newInterceptServer(addr string) (*interceptServer, error) {
	s := &interceptServer{}
	s.server = grpcutil.NewServer()
	healthpb.RegisterHealthServer(s.server, s)
	bridgev1.RegisterInterceptorServiceServer(s.server, s)

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	slog.Info("Intercept server listening", "addr", lis.Addr().String())

	go s.server.Serve(lis)
	return s, nil
}

// SetReady marks the intercept as ready.
func (s *interceptServer) SetReady() {
	s.ready.Store(true)
}

// SetMonitors publishes the given probe monitors. nil entries mean no probe
// of that kind was configured on the source deployment. GetStatus reads each
// monitor's current Status() directly — no fan-in goroutine, no channels.
func (s *interceptServer) SetMonitors(liveness, readiness, startup probe.Monitor) {
	s.monitors.Store(&monitorSet{
		liveness:  liveness,
		readiness: readiness,
		startup:   startup,
	})
}

// Stop gracefully stops the gRPC server.
func (s *interceptServer) Stop() {
	s.server.GracefulStop()
}

// Check implements the gRPC health check by reporting the interceptor's
// own readiness state.
func (s *interceptServer) Check(_ context.Context, _ *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	if !s.ready.Load() {
		return &healthpb.HealthCheckResponse{
			Status: healthpb.HealthCheckResponse_NOT_SERVING,
		}, nil
	}
	return &healthpb.HealthCheckResponse{
		Status: healthpb.HealthCheckResponse_SERVING,
	}, nil
}

// GetStatus returns the overall intercept status, including the latest
// probe-monitor results.
func (s *interceptServer) GetStatus(_ context.Context, _ *bridgev1.GetStatusRequest) (*bridgev1.GetStatusResponse, error) {
	resp := &bridgev1.GetStatusResponse{Ready: s.ready.Load()}
	if ms := s.monitors.Load(); ms != nil {
		if ms.liveness != nil {
			resp.Liveness = ms.liveness.Status()
		}
		if ms.readiness != nil {
			resp.Readiness = ms.readiness.Status()
		}
		if ms.startup != nil {
			resp.Startup = ms.startup.Status()
		}
	}
	return resp, nil
}
