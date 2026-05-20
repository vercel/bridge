package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
)

// waitForHealth polls m.Health until it reaches want or the timeout fires.
// Returns the matching Status() snapshot.
func waitForHealth(t *testing.T, m Monitor, want bridgev1.ProbeHealth, timeout time.Duration) *bridgev1.ProbeStatus {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if m.Health() == want {
			return m.Status()
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for health %v; last=%+v", want, m.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func parsePort(t *testing.T, addr string) int32 {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := strconv.Atoi(p)
	require.NoError(t, err)
	return int32(port)
}

func TestMonitor_HTTPHealthy(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		assert.Equal(t, "/healthz", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	port := parsePort(t, srv.Listener.Addr().String())
	m, err := NewMonitor(Config{
		Probe: &bridgev1.Probe{
			PeriodSeconds:    1,
			SuccessThreshold: 1,
			Handler: &bridgev1.Probe_HttpGet{
				HttpGet: &bridgev1.HTTPGetAction{Path: "/healthz", Port: port},
			},
		},
		Port: port,
		Host: "127.0.0.1",
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	got := waitForHealth(t, m, bridgev1.ProbeHealth_PROBE_HEALTH_HEALTHY, 2*time.Second)
	assert.Equal(t, "OK", got.GetMessage())
	assert.True(t, hits.Load() >= 1)
	assert.GreaterOrEqual(t, got.GetConsecutiveSuccesses(), int32(1))
}

func TestMonitor_HTTPUnhealthyAfterFailureThreshold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	port := parsePort(t, srv.Listener.Addr().String())
	m, err := NewMonitor(Config{
		Probe: &bridgev1.Probe{
			PeriodSeconds:    1,
			FailureThreshold: 2,
			Handler: &bridgev1.Probe_HttpGet{
				HttpGet: &bridgev1.HTTPGetAction{Port: port},
			},
		},
		Port: port,
		Host: "127.0.0.1",
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	got := waitForHealth(t, m, bridgev1.ProbeHealth_PROBE_HEALTH_UNHEALTHY, 3*time.Second)
	assert.Contains(t, got.GetMessage(), "HTTP 500")
	assert.GreaterOrEqual(t, got.GetConsecutiveFailures(), int32(2))
}

func TestMonitor_TCPHealthyWhenPortOpen(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer lis.Close()
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	port := parsePort(t, lis.Addr().String())
	m, err := NewMonitor(Config{
		Probe: &bridgev1.Probe{
			PeriodSeconds: 1,
			Handler:       &bridgev1.Probe_TcpSocket{TcpSocket: &bridgev1.TCPSocketAction{Port: port}},
		},
		Port: port,
		Host: "127.0.0.1",
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	waitForHealth(t, m, bridgev1.ProbeHealth_PROBE_HEALTH_HEALTHY, 2*time.Second)
}

func TestMonitor_TCPUnhealthyWhenPortClosed(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := parsePort(t, lis.Addr().String())
	lis.Close()

	m, err := NewMonitor(Config{
		Probe: &bridgev1.Probe{
			PeriodSeconds:    1,
			FailureThreshold: 1,
			TimeoutSeconds:   1,
			Handler:          &bridgev1.Probe_TcpSocket{TcpSocket: &bridgev1.TCPSocketAction{Port: port}},
		},
		Port: port,
		Host: "127.0.0.1",
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	waitForHealth(t, m, bridgev1.ProbeHealth_PROBE_HEALTH_UNHEALTHY, 2*time.Second)
}

func TestMonitor_ExecHealthy(t *testing.T) {
	m, err := NewMonitor(Config{
		Probe: &bridgev1.Probe{
			PeriodSeconds: 1,
			Handler:       &bridgev1.Probe_Exec{Exec: &bridgev1.ExecAction{Command: []string{"true"}}},
		},
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	waitForHealth(t, m, bridgev1.ProbeHealth_PROBE_HEALTH_HEALTHY, 2*time.Second)
}

func TestMonitor_ExecUnhealthy(t *testing.T) {
	m, err := NewMonitor(Config{
		Probe: &bridgev1.Probe{
			PeriodSeconds:    1,
			FailureThreshold: 1,
			Handler:          &bridgev1.Probe_Exec{Exec: &bridgev1.ExecAction{Command: []string{"false"}}},
		},
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	got := waitForHealth(t, m, bridgev1.ProbeHealth_PROBE_HEALTH_UNHEALTHY, 2*time.Second)
	assert.Contains(t, got.GetMessage(), "exit status")
}

// TestMonitor_TransitionsFromUnhealthyBackToHealthy verifies that the monitor
// transitions in both directions, not just into UNHEALTHY.
func TestMonitor_TransitionsFromUnhealthyBackToHealthy(t *testing.T) {
	var serveOK atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if serveOK.Load() {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	port := parsePort(t, srv.Listener.Addr().String())
	m, err := NewMonitor(Config{
		Probe: &bridgev1.Probe{
			PeriodSeconds:    1,
			SuccessThreshold: 1,
			FailureThreshold: 1,
			Handler:          &bridgev1.Probe_HttpGet{HttpGet: &bridgev1.HTTPGetAction{Port: port}},
		},
		Port: port,
		Host: "127.0.0.1",
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	waitForHealth(t, m, bridgev1.ProbeHealth_PROBE_HEALTH_UNHEALTHY, 3*time.Second)

	serveOK.Store(true)

	got := waitForHealth(t, m, bridgev1.ProbeHealth_PROBE_HEALTH_HEALTHY, 3*time.Second)
	assert.Equal(t, "OK", got.GetMessage())
}

// TestMonitor_HealthAndStatusAgree verifies the cheap Health() accessor never
// drifts from the full Status() snapshot.
func TestMonitor_HealthAndStatusAgree(t *testing.T) {
	m, err := NewMonitor(Config{
		Probe: &bridgev1.Probe{
			PeriodSeconds:    1,
			SuccessThreshold: 1,
			Handler:          &bridgev1.Probe_Exec{Exec: &bridgev1.ExecAction{Command: []string{"true"}}},
		},
	})
	require.NoError(t, err)

	// Before Start, both reads return PENDING.
	assert.Equal(t, bridgev1.ProbeHealth_PROBE_HEALTH_PENDING, m.Health())
	assert.Equal(t, bridgev1.ProbeHealth_PROBE_HEALTH_PENDING, m.Status().GetHealth())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	got := waitForHealth(t, m, bridgev1.ProbeHealth_PROBE_HEALTH_HEALTHY, 2*time.Second)
	assert.Equal(t, m.Health(), got.GetHealth())
}

func TestNewMonitor_RejectsNilProbe(t *testing.T) {
	_, err := NewMonitor(Config{Probe: nil})
	assert.Error(t, err)
}

func TestNewMonitor_RejectsNilHandler(t *testing.T) {
	_, err := NewMonitor(Config{Probe: &bridgev1.Probe{}})
	assert.Error(t, err)
}
