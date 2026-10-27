// Package devtunnel is a tunnel.Backend over a dev tunnel: the Connect stream
// to api-dev-tunnel that `vc dev --tunnel` holds otherwise. The deployment's
// conns reach the app, and the app's outbound conns leave through the
// deployment.
package devtunnel

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/puzpuzpuz/xsync/v3"
	"github.com/segmentio/ksuid"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/api/go/vercel/api/dev_tunnel/v1"
	"github.com/vercel/bridge/pkg/plumbing"
	"github.com/vercel/bridge/pkg/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
)

const (
	// A client numbers its conns from here; the relay numbers its own from 1.
	clientConnIDFloor = uint64(1) << 63

	instanceHeader = "x-vercel-dev-tunnel-instance"

	retryMin = 250 * time.Millisecond
	retryMax = 5 * time.Second
)

var instancePattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// Codes reconnecting can't fix.
var fatal = map[codes.Code]bool{
	codes.FailedPrecondition: true,
	codes.InvalidArgument:    true,
	codes.PermissionDenied:   true,
	codes.Unimplemented:      true,
}

// Config configures a Backend.
type Config struct {
	// RelayURL is api-dev-tunnel's URL, from create-session.
	RelayURL string
	// Token is the client token, from create-session. Without one the
	// backend waits for SetToken.
	Token string
	// Instance tells the relay this process's reconnects from another
	// client's, so a restart can take the tunnel back while its old stream is
	// still open. Random if empty.
	Instance string
	// Port is the only port of the app the deployment's conns may reach.
	Port int
	// RelayDialer connects to the relay. Defaults to a net.Dialer.
	RelayDialer plumbing.ContextDialer
	Logger      *slog.Logger
}

// timings are the relay's protocol intervals, shortened by tests.
type timings struct {
	// The relay needs a heartbeat at least every 30s.
	heartbeat time.Duration
	// The relay heartbeats every 30s; three missed and the stream is dead.
	silent time.Duration
	// The relay holds the deployment's conns this long for a client, and an
	// outbound conn waits as long for a stream.
	liveWait time.Duration
}

var defaultTimings = timings{
	heartbeat: 15 * time.Second,
	silent:    90 * time.Second,
	liveWait:  5 * time.Second,
}

// Backend holds a dev tunnel's stream to its relay, replacing it whenever it
// drops.
type Backend struct {
	cfg      Config
	timings  timings
	logger   *slog.Logger
	instance string
	target   string
	creds    credentials.TransportCredentials

	// Set by Run, before any stream.
	rpc    devtunnel.TunnelServiceClient
	accept func(net.Conn, *tunnel.ConnInfo)
	ctx    context.Context

	// Each open conn, by an id unique across streams.
	conns *xsync.MapOf[string, *relayFrames]

	mu       sync.Mutex
	token    string
	tokenSet chan struct{}
	// The newest stream holding the tunnel, which new outbound conns take.
	live       *stream
	liveCh     chan struct{}
	liveClosed bool
	// Streams holding the tunnel, including one finishing its conns after the
	// relay said to reconnect.
	held       int
	state      bridgev1.TunnelState
	lastErr    string
	liveSince  time.Time
	reconnects int64

	ingressConns, egressConns, ingressTotal, egressTotal atomic.Int64
	bytesSent, bytesReceived                             atomic.Int64
	nextEgress, nextStream                               atomic.Uint64
}

var _ tunnel.Backend = (*Backend)(nil)

// New returns a Backend for cfg. Nothing connects until Run.
func New(cfg Config) (*Backend, error) {
	target, creds, err := relayTarget(cfg.RelayURL)
	if err != nil {
		return nil, err
	}
	instance := cfg.Instance
	if instance == "" {
		instance = "bridge-" + ksuid.New().String()
	}
	if !instancePattern.MatchString(instance) {
		return nil, fmt.Errorf("tunnel instance must be 1 to 64 letters, digits or dashes: %q", instance)
	}
	if cfg.RelayDialer == nil {
		cfg.RelayDialer = &net.Dialer{Timeout: 10 * time.Second}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Backend{
		cfg:      cfg,
		timings:  defaultTimings,
		logger:   logger.With("component", "dev-tunnel", "relay", cfg.RelayURL, "instance", instance),
		instance: instance,
		target:   target,
		creds:    creds,
		conns:    xsync.NewMapOf[string, *relayFrames](),
		token:    cfg.Token,
		tokenSet: make(chan struct{}),
		liveCh:   make(chan struct{}),
		state:    bridgev1.TunnelState_TUNNEL_STATE_CONNECTING,
	}, nil
}

// relayTarget turns the relay's URL into a gRPC target. Run dials it with
// RelayDialer rather than resolving it through gRPC, so an intercept whose DNS
// forwards every domain still reaches the relay directly.
func relayTarget(relayURL string) (string, credentials.TransportCredentials, error) {
	u, err := url.Parse(relayURL)
	if err != nil {
		return "", nil, fmt.Errorf("invalid relay URL %q: %w", relayURL, err)
	}
	if u.Path != "" && u.Path != "/" {
		return "", nil, fmt.Errorf("relay URL %q must have no path", relayURL)
	}
	var creds credentials.TransportCredentials
	port := u.Port()
	switch u.Scheme {
	case "https":
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
		if port == "" {
			port = "443"
		}
	case "http":
		creds = insecure.NewCredentials()
		if port == "" {
			port = "80"
		}
	default:
		return "", nil, fmt.Errorf("relay URL %q must be http or https", relayURL)
	}
	if u.Hostname() == "" {
		return "", nil, fmt.Errorf("relay URL %q has no host", relayURL)
	}
	return "passthrough:///" + net.JoinHostPort(u.Hostname(), port), creds, nil
}

// Run holds the tunnel until ctx ends, replacing its stream whenever it
// drops. It returns an error only if the relay refuses the tunnel for good.
func (b *Backend) Run(ctx context.Context, accept func(net.Conn, *tunnel.ConnInfo)) error {
	relayDialer := b.cfg.RelayDialer
	cc, err := grpc.NewClient(b.target,
		grpc.WithTransportCredentials(b.creds),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return relayDialer.DialContext(ctx, "tcp", addr)
		}),
		// Notices a dead connection, e.g. after sleep, sooner than the
		// relay's heartbeats would.
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                10 * time.Second,
			Timeout:             20 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return fmt.Errorf("creating relay client: %w", err)
	}
	defer cc.Close()
	b.rpc = devtunnel.NewTunnelServiceClient(cc)
	b.accept = accept
	b.ctx = ctx
	// A stream the relay moved off may still be finishing.
	defer b.conns.Range(func(_ string, f *relayFrames) bool {
		f.stream.PeerReset()
		return true
	})

	err = b.run()
	if err != nil {
		b.mu.Lock()
		b.state = bridgev1.TunnelState_TUNNEL_STATE_FAILED
		b.lastErr = err.Error()
		b.mu.Unlock()
		b.logger.Error("Tunnel stopped", "error", err)
	}
	return err
}

// Open sends an outbound conn out through the deployment, to its hostname if
// the intercept's DNS gave it one, or else to the address it was headed for.
// The deployment resolves the hostname as its own code would. It waits a
// while for a stream to hold the tunnel.
func (b *Backend) Open(ctx context.Context, info *tunnel.ConnInfo) (net.Conn, error) {
	s := b.awaitLive(ctx)
	if s == nil {
		return nil, errors.New("no stream holds the tunnel")
	}
	n := b.nextEgress.Add(1)
	f := s.newConn("e"+strconv.FormatUint(n, 10), clientConnIDFloor|n, egress, info.Dest)
	if f == nil {
		return nil, errors.New("the tunnel's stream ended")
	}
	host := info.Hostname
	if host == "" {
		host = info.Dest.Host
	}
	s.send(&devtunnel.ConnectRequest{Frame: &devtunnel.ConnectRequest_Open{Open: &devtunnel.Open{
		ConnId:   f.wire,
		Protocol: devtunnel.Protocol_PROTOCOL_TCP,
		Host:     host,
		Port:     uint32(info.Dest.Port),
	}}})
	return f.stream.Conn(), nil
}

// counters tracks conns opening (delta 1) and ending (delta -1).
func (b *Backend) counters(dir direction, delta int64) {
	open, total := &b.ingressConns, &b.ingressTotal
	if dir == egress {
		open, total = &b.egressConns, &b.egressTotal
	}
	open.Add(delta)
	if delta > 0 {
		total.Add(delta)
	}
}

// SetToken replaces the client token. A backend waiting for one connects with
// it; a live one uses it on its next stream.
func (b *Backend) SetToken(token string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.token = token
	close(b.tokenSet)
	b.tokenSet = make(chan struct{})
}

// Status reports the tunnel's state and traffic.
func (b *Backend) Status() *bridgev1.TunnelStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := &bridgev1.TunnelStatus{
		State:             b.state,
		LastError:         b.lastErr,
		Reconnects:        b.reconnects,
		IngressConns:      b.ingressConns.Load(),
		EgressConns:       b.egressConns.Load(),
		IngressConnsTotal: b.ingressTotal.Load(),
		EgressConnsTotal:  b.egressTotal.Load(),
		BytesSent:         b.bytesSent.Load(),
		BytesReceived:     b.bytesReceived.Load(),
	}
	if !b.liveSince.IsZero() {
		st.LiveSinceUnixSeconds = b.liveSince.Unix()
	}
	return st
}

// awaitLive returns the stream new outbound conns take, waiting a while for
// one, or nil.
func (b *Backend) awaitLive(ctx context.Context) *stream {
	timer := time.NewTimer(b.timings.liveWait)
	defer timer.Stop()
	for {
		b.mu.Lock()
		s, ch := b.live, b.liveCh
		b.mu.Unlock()
		if s != nil {
			return s
		}
		select {
		case <-ch:
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

// run replaces the tunnel's stream whenever it ends, for as long as it takes:
// the relay's pods roll often. A stream the relay says to move off keeps its
// open conns while the next one opens.
func (b *Backend) run() error {
	delay := retryMin
	first := true
	warnedTaken := false
	for {
		token, ok := b.awaitToken()
		if !ok {
			return nil
		}
		if !first {
			b.mu.Lock()
			b.reconnects++
			b.mu.Unlock()
		}
		first = false

		s := b.newStream()
		ended := make(chan error, 1)
		moved := make(chan struct{})
		go func() { ended <- s.run(token, sync.OnceFunc(func() { close(moved) })) }()

		var err error
		select {
		case <-moved:
			b.logger.Info("Relay pod is shutting down, opening a new stream")
			continue
		case err = <-ended:
		}
		if b.ctx.Err() != nil {
			return nil
		}
		if s.held.Load() {
			delay = retryMin
			warnedTaken = false
		}
		code := status.Code(err)
		b.mu.Lock()
		if err != nil {
			b.lastErr = err.Error()
		} else {
			b.lastErr = "the relay ended the stream"
		}
		b.mu.Unlock()
		if fatal[code] {
			return fmt.Errorf("the relay refused the tunnel: %w", err)
		}

		logger := b.logger.With("code", code.String(), "error", err)
		switch code {
		case codes.Unauthenticated:
			// Each create-session replaces the client token.
			b.refuseToken(token)
			logger.Warn("The relay refused the client token, waiting for a new one")
			continue
		case codes.AlreadyExists:
			b.setState(bridgev1.TunnelState_TUNNEL_STATE_TAKEN)
			if !warnedTaken {
				warnedTaken = true
				logger.Warn("Another client holds the tunnel, waiting for it to let go")
			}
		default:
			b.mu.Lock()
			if b.held == 0 {
				b.state = bridgev1.TunnelState_TUNNEL_STATE_CONNECTING
			}
			b.mu.Unlock()
			logger.Info("Tunnel stream ended, reconnecting", "delay", delay)
		}

		select {
		case <-time.After(delay):
		case <-b.ctx.Done():
			return nil
		}
		delay = min(delay*2, retryMax)
	}
}

// awaitToken returns the client token, waiting for SetToken while there is
// none.
func (b *Backend) awaitToken() (string, bool) {
	for {
		b.mu.Lock()
		token, set := b.token, b.tokenSet
		if token == "" {
			b.state = bridgev1.TunnelState_TUNNEL_STATE_NEEDS_TOKEN
		}
		b.mu.Unlock()
		if token != "" {
			return token, true
		}
		select {
		case <-set:
		case <-b.ctx.Done():
			return "", false
		}
	}
}

// refuseToken forgets token, unless SetToken already replaced it.
func (b *Backend) refuseToken(token string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.token == token {
		b.token = ""
	}
}

func (b *Backend) setState(state bridgev1.TunnelState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = state
}

// setLive makes s the stream new outbound conns take; nil for none.
func (b *Backend) setLive(s *stream) {
	b.live = s
	if s != nil && !b.liveClosed {
		close(b.liveCh)
		b.liveClosed = true
	} else if s == nil && b.liveClosed {
		b.liveCh = make(chan struct{})
		b.liveClosed = false
	}
}

// holding: the relay routes the tunnel's conns to s.
func (b *Backend) holding(s *stream) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.held++
	b.setLive(s)
	if b.state != bridgev1.TunnelState_TUNNEL_STATE_LIVE {
		b.state = bridgev1.TunnelState_TUNNEL_STATE_LIVE
		b.liveSince = time.Now()
		b.logger.Info("Tunnel live", "reconnects", b.reconnects)
	}
}

// moving: s gets no new conns, but finishes the ones open on it.
func (b *Backend) moving(s *stream) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.live == s {
		b.setLive(nil)
	}
}

// lost: s ended.
func (b *Backend) lost(s *stream) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s.held.Load() {
		b.held--
	}
	if b.live == s {
		b.setLive(nil)
	}
	if b.held == 0 && b.state == bridgev1.TunnelState_TUNNEL_STATE_LIVE {
		b.state = bridgev1.TunnelState_TUNNEL_STATE_CONNECTING
		b.liveSince = time.Time{}
	}
}

var errSilent = errors.New("no heartbeat from the relay")
