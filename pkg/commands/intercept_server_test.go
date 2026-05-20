package commands

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/probe"
)

// newTestInterceptServer binds the interceptServer to a free local port and
// returns the server. Tests call GetStatus / SetReady / SetMonitors directly.
func newTestInterceptServer(t *testing.T) *interceptServer {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := lis.Addr().String()
	lis.Close()

	srv, err := newInterceptServer(addr)
	require.NoError(t, err)
	t.Cleanup(srv.Stop)
	return srv
}

func TestInterceptServer_GetStatusBeforeReady(t *testing.T) {
	srv := newTestInterceptServer(t)

	resp, err := srv.GetStatus(context.Background(), &bridgev1.GetStatusRequest{})
	require.NoError(t, err)
	assert.False(t, resp.GetReady())
	assert.Nil(t, resp.GetLiveness())
	assert.Nil(t, resp.GetReadiness())
	assert.Nil(t, resp.GetStartup())
}

func TestInterceptServer_SetMonitorsExposesPendingStatusBeforeStart(t *testing.T) {
	srv := newTestInterceptServer(t)

	m, err := probe.NewMonitor(probe.Config{
		Probe: &bridgev1.Probe{
			Handler: &bridgev1.Probe_Exec{Exec: &bridgev1.ExecAction{Command: []string{"true"}}},
		},
	})
	require.NoError(t, err)
	srv.SetMonitors(m, nil, nil)

	resp, err := srv.GetStatus(context.Background(), &bridgev1.GetStatusRequest{})
	require.NoError(t, err)
	assert.NotNil(t, resp.GetLiveness())
	assert.Equal(t, bridgev1.ProbeHealth_PROBE_HEALTH_PENDING, resp.GetLiveness().GetHealth())
	assert.Nil(t, resp.GetReadiness())
	assert.Nil(t, resp.GetStartup())
}

func TestInterceptServer_GetStatusReflectsMonitorTransitions(t *testing.T) {
	srv := newTestInterceptServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m, err := probe.NewMonitor(probe.Config{
		Probe: &bridgev1.Probe{
			PeriodSeconds:    1,
			SuccessThreshold: 1,
			Handler:          &bridgev1.Probe_Exec{Exec: &bridgev1.ExecAction{Command: []string{"true"}}},
		},
	})
	require.NoError(t, err)
	srv.SetMonitors(m, nil, nil)
	m.Start(ctx)
	srv.SetReady()

	// Poll until the monitor reports HEALTHY via the Status() path.
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := srv.GetStatus(context.Background(), &bridgev1.GetStatusRequest{})
		require.NoError(t, err)
		assert.True(t, resp.GetReady())
		if resp.GetLiveness().GetHealth() == bridgev1.ProbeHealth_PROBE_HEALTH_HEALTHY {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for healthy liveness; last=%+v", resp.GetLiveness())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestInterceptServer_AllThreeMonitorsRouteIndependently(t *testing.T) {
	srv := newTestInterceptServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mk := func() probe.Monitor {
		m, err := probe.NewMonitor(probe.Config{
			Probe: &bridgev1.Probe{
				PeriodSeconds: 1,
				Handler:       &bridgev1.Probe_Exec{Exec: &bridgev1.ExecAction{Command: []string{"true"}}},
			},
		})
		require.NoError(t, err)
		return m
	}
	liveness, readiness, startup := mk(), mk(), mk()
	srv.SetMonitors(liveness, readiness, startup)
	liveness.Start(ctx)
	readiness.Start(ctx)
	startup.Start(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := srv.GetStatus(context.Background(), &bridgev1.GetStatusRequest{})
		require.NoError(t, err)
		if resp.GetLiveness().GetHealth() == bridgev1.ProbeHealth_PROBE_HEALTH_HEALTHY &&
			resp.GetReadiness().GetHealth() == bridgev1.ProbeHealth_PROBE_HEALTH_HEALTHY &&
			resp.GetStartup().GetHealth() == bridgev1.ProbeHealth_PROBE_HEALTH_HEALTHY {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out; got=%+v", resp)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
