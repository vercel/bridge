package commands

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
)

// fakeInterceptor controls both the GetStatus snapshot (for callers that
// still rely on the threshold-based view) and the synchronous Probe
// response that bridge dev now polls.
type fakeInterceptor struct {
	status atomic.Value // *bridgev1.GetStatusResponse
	probe  atomic.Value // *bridgev1.ProbeResponse
}

func (f *fakeInterceptor) GetStatus(_ context.Context, _ *bridgev1.GetStatusRequest, _ ...grpc.CallOption) (*bridgev1.GetStatusResponse, error) {
	v := f.status.Load()
	if v == nil {
		return &bridgev1.GetStatusResponse{}, nil
	}
	return v.(*bridgev1.GetStatusResponse), nil
}

func (f *fakeInterceptor) Probe(_ context.Context, _ *bridgev1.ProbeRequest, _ ...grpc.CallOption) (*bridgev1.ProbeResponse, error) {
	v := f.probe.Load()
	if v == nil {
		return &bridgev1.ProbeResponse{}, nil
	}
	return v.(*bridgev1.ProbeResponse), nil
}

func passing() *bridgev1.ProbeCheckResult  { return &bridgev1.ProbeCheckResult{Passed: true} }
func failing() *bridgev1.ProbeCheckResult  { return &bridgev1.ProbeCheckResult{Passed: false, Error: "boom"} }

// newClosedExitCh returns an exit channel that delivers the given code
// immediately. Use to simulate "the dev command has already exited".
func newClosedExitCh(code int32) <-chan int32 {
	ch := make(chan int32, 1)
	ch <- code
	return ch
}

// newOpenExitCh returns an exit channel that never delivers. Use to
// simulate "the dev command is still running".
func newOpenExitCh() <-chan int32 {
	return make(chan int32)
}

// TestWaitForDevHealthy_HealthyResolvesImmediately verifies that when probes
// already pass the synchronous check, we return success right away with the
// PID populated.
func TestWaitForDevHealthy_HealthyResolvesImmediately(t *testing.T) {
	inter := &fakeInterceptor{}
	inter.probe.Store(&bridgev1.ProbeResponse{Liveness: passing(), Readiness: passing()})

	resp, err := waitForDevHealthy(context.Background(), newOpenExitCh(), inter, 12345, 2*time.Second)
	require.NoError(t, err)
	assert.Equal(t, int32(12345), resp.GetPid())
	assert.Equal(t, bridgev1.DevCommandReason_healthy, resp.GetReason())
	assert.Nil(t, resp.ExitCode)
}

// TestWaitForDevHealthy_ExitWinsOverHealth confirms the documented precedence:
// if the dev command exited (even with a non-zero code) on the same tick that
// probes pass, the exit wins. Otherwise a crashed-then-restarted-by-
// supervisor server would be reported as healthy when it's actually dying.
func TestWaitForDevHealthy_ExitWinsOverHealth(t *testing.T) {
	inter := &fakeInterceptor{}
	inter.probe.Store(&bridgev1.ProbeResponse{Liveness: passing()})

	resp, err := waitForDevHealthy(context.Background(), newClosedExitCh(2), inter, 100, 2*time.Second)
	require.Error(t, err)
	assert.Equal(t, bridgev1.DevCommandReason_exited, resp.GetReason())
	require.NotNil(t, resp.ExitCode)
	assert.Equal(t, int32(2), *resp.ExitCode)
}

// TestWaitForDevHealthy_TimeoutWhenProbesFail verifies the third exit
// condition: probes keep failing and the dev command keeps running, so
// we hit the timeout and report it.
func TestWaitForDevHealthy_TimeoutWhenProbesFail(t *testing.T) {
	inter := &fakeInterceptor{}
	inter.probe.Store(&bridgev1.ProbeResponse{Liveness: failing(), Readiness: failing()})

	resp, err := waitForDevHealthy(context.Background(), newOpenExitCh(), inter, 100, 200*time.Millisecond)
	require.Error(t, err)
	assert.Equal(t, bridgev1.DevCommandReason_timeout, resp.GetReason())
	assert.Equal(t, int32(100), resp.GetPid())
}

// TestWaitForDevHealthy_RequiresAllConfiguredProbes ensures a half-passing
// state (liveness up, readiness still failing) is NOT considered healthy.
func TestWaitForDevHealthy_RequiresAllConfiguredProbes(t *testing.T) {
	inter := &fakeInterceptor{}
	inter.probe.Store(&bridgev1.ProbeResponse{Liveness: passing(), Readiness: failing()})

	resp, err := waitForDevHealthy(context.Background(), newOpenExitCh(), inter, 100, 200*time.Millisecond)
	require.Error(t, err)
	assert.Equal(t, bridgev1.DevCommandReason_timeout, resp.GetReason())
}

// TestWaitForDevHealthy_NoProbesReturnsHealthy verifies the "default to
// healthy when no probes are declared" semantics: many source deployments
// don't bother configuring probes, and we don't want bridge dev to sit
// forever when there's nothing to actually check.
func TestWaitForDevHealthy_NoProbesReturnsHealthy(t *testing.T) {
	inter := &fakeInterceptor{}
	inter.probe.Store(&bridgev1.ProbeResponse{}) // no probes configured

	resp, err := waitForDevHealthy(context.Background(), newOpenExitCh(), inter, 100, 2*time.Second)
	require.NoError(t, err)
	assert.Equal(t, bridgev1.DevCommandReason_healthy, resp.GetReason())
}

// TestWaitForDevHealthy_HealthyAfterPolling exercises the polling loop:
// probes start failing and flip to passing mid-wait. We must observe the
// transition and return success.
func TestWaitForDevHealthy_HealthyAfterPolling(t *testing.T) {
	inter := &fakeInterceptor{}
	inter.probe.Store(&bridgev1.ProbeResponse{Liveness: failing()})

	// Flip to passing shortly after the first poll.
	go func() {
		time.Sleep(devHealthPollInterval + 50*time.Millisecond)
		inter.probe.Store(&bridgev1.ProbeResponse{Liveness: passing()})
	}()

	resp, err := waitForDevHealthy(context.Background(), newOpenExitCh(), inter, 100, 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, bridgev1.DevCommandReason_healthy, resp.GetReason())
}

// TestWaitForDevHealthy_ContextCancelStops returns parent's context error
// instead of hanging when the caller cancels (e.g. ^C in the terminal).
func TestWaitForDevHealthy_ContextCancelStops(t *testing.T) {
	inter := &fakeInterceptor{}
	inter.probe.Store(&bridgev1.ProbeResponse{Liveness: failing()})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(devHealthPollInterval + 50*time.Millisecond)
		cancel()
	}()

	_, err := waitForDevHealthy(ctx, newOpenExitCh(), inter, 100, 10*time.Second)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestWaitForDevHealthy_ExitDuringPollingDetected handles a mid-wait exit.
// Probes never flip passing, the dev command exits with code 7 partway
// through, and we report exit (not timeout).
func TestWaitForDevHealthy_ExitDuringPollingDetected(t *testing.T) {
	inter := &fakeInterceptor{}
	inter.probe.Store(&bridgev1.ProbeResponse{Liveness: failing()})

	exitCh := make(chan int32, 1)
	go func() {
		time.Sleep(devHealthPollInterval + 50*time.Millisecond)
		exitCh <- 7
	}()

	resp, err := waitForDevHealthy(context.Background(), exitCh, inter, 100, 5*time.Second)
	require.Error(t, err)
	assert.Equal(t, bridgev1.DevCommandReason_exited, resp.GetReason())
	require.NotNil(t, resp.ExitCode)
	assert.Equal(t, int32(7), *resp.ExitCode)
}
