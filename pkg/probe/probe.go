// Package probe runs Kubernetes-style health probes (HTTP, TCP, gRPC, Exec)
// from inside the devcontainer against the developer's local application.
// A Monitor wraps one Probe and exposes its current health via atomic
// accessors, so consumers don't need locks or channels to read its state.
package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"sync/atomic"
	"time"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

var insecureTransport = grpc.WithTransportCredentials(insecure.NewCredentials())

// Defaults match Kubernetes' probe defaults so a probe with zero values
// behaves the same locally as it would in the cluster.
const (
	defaultPeriodSeconds    = 10
	defaultTimeoutSeconds   = 1
	defaultSuccessThreshold = 1
	defaultFailureThreshold = 3
)

// Monitor runs a single probe and exposes its current health and full status.
// Both accessors are safe to call concurrently and are lock-free.
type Monitor interface {
	// Start launches the check loop. The loop exits when ctx is cancelled.
	// Start must be called at most once.
	Start(ctx context.Context)

	// Health returns the current health bit. Lock-free; no allocation.
	Health() bridgev1.ProbeHealth

	// Status returns a snapshot of the latest check result, including
	// counters, message, and last-check timestamp. Lock-free; the pointer
	// is immutable after return. Returns nil if no check has completed yet.
	Status() *bridgev1.ProbeStatus

	// Probe runs the probe one time, synchronously, and returns whether
	// the check passed plus any error. Does NOT update the threshold-based
	// Health/Status surfaced by GetStatus — this is a stateless "is the
	// app responding right now?" check used by callers that need to
	// bypass the success/failure-threshold smoothing (e.g. bridge dev
	// after restarting the dev process).
	Probe(ctx context.Context) error
}

// monitor is the default Monitor implementation. All state shared across
// goroutines lives in atomics: health (int32) for fast reads and status
// (pointer) for the full snapshot.
type monitor struct {
	probe  *bridgev1.Probe
	host   string // localhost
	port   int32  // resolved port (after source_app_port → app_port remap)
	now    func() time.Time
	health atomic.Int32                          // bridgev1.ProbeHealth
	status atomic.Pointer[bridgev1.ProbeStatus]  // latest snapshot
}

// Config configures a Monitor.
type Config struct {
	// Probe is the source probe definition. Must be non-nil and have a handler.
	Probe *bridgev1.Probe

	// Port is the port the probe should target on the local app. The caller
	// resolves the source's port → --app-port remap before constructing the
	// monitor, so the value here is already the absolute local port.
	Port int32

	// Host overrides the target host. Defaults to "localhost".
	Host string

	// Now lets tests inject a deterministic clock. Defaults to time.Now.
	Now func() time.Time
}

// NewMonitor returns a Monitor configured for the given probe. It does not
// start running yet — call Start.
func NewMonitor(cfg Config) (Monitor, error) {
	if cfg.Probe == nil {
		return nil, errors.New("probe is required")
	}
	if cfg.Probe.GetHandler() == nil {
		return nil, errors.New("probe has no handler")
	}
	host := cfg.Host
	if host == "" {
		host = "localhost"
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	m := &monitor{
		probe: cfg.Probe,
		host:  host,
		port:  cfg.Port,
		now:   now,
	}
	m.health.Store(int32(bridgev1.ProbeHealth_PROBE_HEALTH_PENDING))
	m.status.Store(&bridgev1.ProbeStatus{Health: bridgev1.ProbeHealth_PROBE_HEALTH_PENDING})
	return m, nil
}

// Health returns the current health bit.
func (m *monitor) Health() bridgev1.ProbeHealth {
	return bridgev1.ProbeHealth(m.health.Load())
}

// Status returns the latest published status snapshot.
func (m *monitor) Status() *bridgev1.ProbeStatus {
	return m.status.Load()
}

// Probe implements Monitor.Probe — a synchronous, stateless check that
// bypasses the threshold-tracked state used by Status/Health.
func (m *monitor) Probe(ctx context.Context) error {
	timeout := time.Duration(intOrDefault(m.probe.GetTimeoutSeconds(), defaultTimeoutSeconds)) * time.Second
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return runHandler(runCtx, m.probe, m.host, m.port)
}

// Start begins running the probe in a goroutine. The goroutine exits when
// ctx is cancelled.
func (m *monitor) Start(ctx context.Context) {
	go m.run(ctx)
}

func (m *monitor) run(ctx context.Context) {
	// Counters and message are local — only this goroutine touches them.
	// Health and the full status snapshot live in atomics on m.
	var (
		totalChecks        int64
		consecutiveSuccess int32
		consecutiveFailure int32
		lastCheckUnix      int64
		message            string
	)

	if d := time.Duration(m.probe.GetInitialDelaySeconds()) * time.Second; d > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		}
	}

	period := time.Duration(intOrDefault(m.probe.GetPeriodSeconds(), defaultPeriodSeconds)) * time.Second
	successThreshold := intOrDefault(m.probe.GetSuccessThreshold(), defaultSuccessThreshold)
	failureThreshold := intOrDefault(m.probe.GetFailureThreshold(), defaultFailureThreshold)
	timeout := time.Duration(intOrDefault(m.probe.GetTimeoutSeconds(), defaultTimeoutSeconds)) * time.Second

	ticker := time.NewTicker(period)
	defer ticker.Stop()

	check := func() {
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		err := runHandler(runCtx, m.probe, m.host, m.port)

		totalChecks++
		lastCheckUnix = m.now().Unix()

		// Compute what health should be after this check; default to whatever
		// we last published so a non-decisive check leaves it alone.
		next := m.Health()
		if err == nil {
			consecutiveSuccess++
			consecutiveFailure = 0
			message = "OK"
			if consecutiveSuccess >= successThreshold {
				next = bridgev1.ProbeHealth_PROBE_HEALTH_HEALTHY
			}
		} else {
			consecutiveFailure++
			consecutiveSuccess = 0
			message = err.Error()
			if consecutiveFailure >= failureThreshold {
				next = bridgev1.ProbeHealth_PROBE_HEALTH_UNHEALTHY
			}
		}

		// Swap the health bit in; the returned previous value tells us
		// whether this check caused a transition (used for logging /
		// future observers without re-reading the atomic).
		_ = bridgev1.ProbeHealth(m.health.Swap(int32(next)))

		// Always publish the full status — counters and timestamp move
		// even when health doesn't, and Status() consumers want the
		// freshest snapshot.
		m.status.Store(&bridgev1.ProbeStatus{
			Health:               next,
			Message:              message,
			LastCheckUnixSeconds: lastCheckUnix,
			ConsecutiveSuccesses: consecutiveSuccess,
			ConsecutiveFailures:  consecutiveFailure,
			TotalChecks:          totalChecks,
		})
	}

	// Run once immediately, then on each tick. This matches the kubelet's
	// behaviour and avoids developers waiting up to period_seconds for the
	// first result after intercept startup.
	check()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}

// runHandler dispatches to the configured handler implementation.
func runHandler(ctx context.Context, p *bridgev1.Probe, host string, port int32) error {
	switch h := p.GetHandler().(type) {
	case *bridgev1.Probe_HttpGet:
		return runHTTP(ctx, h.HttpGet, host, port)
	case *bridgev1.Probe_TcpSocket:
		return runTCP(ctx, host, port)
	case *bridgev1.Probe_Grpc:
		return runGRPC(ctx, h.Grpc, host, port)
	case *bridgev1.Probe_Exec:
		return runExec(ctx, h.Exec)
	default:
		return fmt.Errorf("unknown probe handler type %T", h)
	}
}

func runHTTP(ctx context.Context, h *bridgev1.HTTPGetAction, host string, port int32) error {
	scheme := "http"
	if h.GetScheme() == "HTTPS" {
		scheme = "https"
	}
	path := h.GetPath()
	if path == "" {
		path = "/"
	} else if path[0] != '/' {
		path = "/" + path
	}

	url := fmt.Sprintf("%s://%s%s", scheme, net.JoinHostPort(host, strconv.Itoa(int(port))), path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	for _, hdr := range h.GetHttpHeaders() {
		req.Header.Add(hdr.GetName(), hdr.GetValue())
	}

	// One-shot client: don't reuse connections, mirror kubelet semantics, and
	// accept self-signed local TLS (common for dev).
	client := &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives: true,
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec — local app, dev only
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Kubernetes considers any 2xx or 3xx code success.
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func runTCP(ctx context.Context, host string, port int32) error {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

func runGRPC(ctx context.Context, h *bridgev1.GRPCAction, host string, port int32) error {
	conn, err := grpc.NewClient(net.JoinHostPort(host, strconv.Itoa(int(port))), insecureTransport)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := healthpb.NewHealthClient(conn)
	resp, err := client.Check(ctx, &healthpb.HealthCheckRequest{Service: h.GetService()})
	if err != nil {
		return err
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		return fmt.Errorf("gRPC health: %s", resp.GetStatus())
	}
	return nil
}

func runExec(ctx context.Context, h *bridgev1.ExecAction) error {
	cmd := h.GetCommand()
	if len(cmd) == 0 {
		return errors.New("exec probe has empty command")
	}
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	out, err := c.CombinedOutput()
	if err != nil {
		// Include trailing output for diagnostics; cap to avoid log spam.
		if n := len(out); n > 0 {
			if n > 256 {
				out = out[:256]
			}
			return fmt.Errorf("%w: %s", err, out)
		}
		return err
	}
	return nil
}

func intOrDefault(v, def int32) int32 {
	if v <= 0 {
		return def
	}
	return v
}
