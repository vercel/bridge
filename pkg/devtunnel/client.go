// Package devtunnel carries an intercept's traffic over a dev tunnel: the
// Connect stream to api-dev-tunnel that `vc dev --tunnel` holds otherwise.
// The deployment's conns reach the app, and the app's outbound conns leave
// through the deployment.
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

	"github.com/segmentio/ksuid"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	devtunnelv1 "github.com/vercel/bridge/api/go/vercel/api/dev_tunnel/v1"
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
	// What each side of a conn may send before the other grants more.
	connWindow = 256 << 10
	// Per Data frame.
	maxPayload = 64 << 10
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

// Config configures a Client.
type Config struct {
	// RelayURL is api-dev-tunnel's URL, from create-session.
	RelayURL string
	// Token is the client token, from create-session. Without one the client
	// waits for SetToken.
	Token string
	// Instance tells the relay this process's reconnects from another
	// client's, so a restart can take the tunnel back while its old stream is
	// still open. Random if empty.
	Instance string
	// Port is the only port of the app the deployment's conns may reach.
	Port int
	// AppDialer connects the deployment's conns to the app. Defaults to a
	// net.Dialer.
	AppDialer plumbing.ContextDialer
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

// Client holds a dev tunnel's stream to its relay, replacing it whenever it
// drops, and implements tunnel.Tunnel over it.
type Client struct {
	cfg      Config
	timings  timings
	logger   *slog.Logger
	instance string
	cc       *grpc.ClientConn
	rpc      devtunnelv1.TunnelServiceClient

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
	nextEgress                                           atomic.Uint64

	ctx       context.Context
	cancel    context.CancelFunc
	started   atomic.Bool
	done      chan struct{}
	closeOnce sync.Once
	err       error
}

var _ tunnel.Tunnel = (*Client)(nil)

// New returns a Client for cfg. Nothing connects until Start.
func New(cfg Config) (*Client, error) {
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
	if cfg.AppDialer == nil {
		cfg.AppDialer = &net.Dialer{Timeout: 10 * time.Second}
	}
	if cfg.RelayDialer == nil {
		cfg.RelayDialer = &net.Dialer{Timeout: 10 * time.Second}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "dev-tunnel", "relay", cfg.RelayURL, "instance", instance)

	relayDialer := cfg.RelayDialer
	cc, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(creds),
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
		return nil, fmt.Errorf("creating relay client: %w", err)
	}

	c := &Client{
		cfg:      cfg,
		timings:  defaultTimings,
		logger:   logger,
		instance: instance,
		cc:       cc,
		rpc:      devtunnelv1.NewTunnelServiceClient(cc),
		token:    cfg.Token,
		tokenSet: make(chan struct{}),
		liveCh:   make(chan struct{}),
		state:    bridgev1.TunnelState_TUNNEL_STATE_CONNECTING,
		done:     make(chan struct{}),
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	return c, nil
}

// relayTarget turns the relay's URL into a gRPC target. The client dials it
// with RelayDialer rather than resolving it through gRPC, so an intercept
// whose DNS forwards every domain still reaches the relay directly.
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

// Start holds the tunnel until ctx ends or Close.
func (c *Client) Start(ctx context.Context) {
	if !c.started.CompareAndSwap(false, true) {
		return
	}
	stop := context.AfterFunc(ctx, c.cancel)
	go func() {
		defer stop()
		c.finish(c.run())
	}()
}

// Close ends every stream and the conns on them.
func (c *Client) Close() error {
	c.cancel()
	if c.started.Load() {
		<-c.done
	} else {
		c.finish(nil)
	}
	return nil
}

func (c *Client) Done() <-chan struct{} { return c.done }

// Err is why the client stopped: the relay refusing the tunnel for good, or
// nil after Close. Valid once Done is closed.
func (c *Client) Err() error {
	<-c.done
	return c.err
}

// SetToken replaces the client token. A client waiting for one connects with
// it; a live one uses it on its next stream.
func (c *Client) SetToken(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = token
	close(c.tokenSet)
	c.tokenSet = make(chan struct{})
}

// Status reports the tunnel's state and traffic.
func (c *Client) Status() *bridgev1.TunnelStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := &bridgev1.TunnelStatus{
		State:             c.state,
		LastError:         c.lastErr,
		Reconnects:        c.reconnects,
		IngressConns:      c.ingressConns.Load(),
		EgressConns:       c.egressConns.Load(),
		IngressConnsTotal: c.ingressTotal.Load(),
		EgressConnsTotal:  c.egressTotal.Load(),
		BytesSent:         c.bytesSent.Load(),
		BytesReceived:     c.bytesReceived.Load(),
	}
	if !c.liveSince.IsZero() {
		st.LiveSinceUnixSeconds = c.liveSince.Unix()
	}
	return st
}

// AddConn sends one of the app's outbound conns out through the deployment,
// to hostname if the intercept's DNS gave the conn one, or else to the
// address it was headed for. The deployment resolves the hostname as its own
// code would.
func (c *Client) AddConn(nc net.Conn, destOverride string, hostname string) {
	dst := destOverride
	if dst == "" {
		dst = nc.LocalAddr().String()
	}
	host, portStr, err := net.SplitHostPort(dst)
	port, portErr := strconv.Atoi(portStr)
	if err != nil || portErr != nil {
		c.logger.Warn("Dropping outbound connection with no destination", "dest", dst)
		_ = nc.Close()
		return
	}
	if hostname != "" {
		host = hostname
	}
	logger := c.logger.With("host", host, "port", port)

	go func() {
		s := c.awaitLive()
		if s == nil {
			logger.Warn("Dropping outbound connection: no stream holds the tunnel")
			_ = nc.Close()
			return
		}
		id := clientConnIDFloor | c.nextEgress.Add(1)
		cn := s.newConn(id, egress)
		if cn == nil {
			logger.Warn("Dropping outbound connection: the tunnel's stream ended")
			_ = nc.Close()
			return
		}
		logger.Debug("Opening outbound connection", "conn_id", id)
		// Queued ahead of any Data the conn sends.
		s.send(&devtunnelv1.ConnectRequest{Frame: &devtunnelv1.ConnectRequest_Open{Open: &devtunnelv1.Open{
			ConnId:   id,
			Protocol: devtunnelv1.Protocol_PROTOCOL_TCP,
			Host:     host,
			Port:     uint32(port),
		}}})
		cn.attach(nc)
	}()
}

// awaitLive returns the stream new outbound conns take, waiting a while for
// one, or nil.
func (c *Client) awaitLive() *stream {
	timer := time.NewTimer(c.timings.liveWait)
	defer timer.Stop()
	for {
		c.mu.Lock()
		s, ch := c.live, c.liveCh
		c.mu.Unlock()
		if s != nil {
			return s
		}
		select {
		case <-ch:
		case <-timer.C:
			return nil
		case <-c.ctx.Done():
			return nil
		}
	}
}

// run replaces the tunnel's stream whenever it ends, for as long as it takes:
// the relay's pods roll often. A stream the relay says to move off keeps its
// open conns while the next one opens.
func (c *Client) run() error {
	delay := retryMin
	first := true
	warnedTaken := false
	for {
		token, ok := c.awaitToken()
		if !ok {
			return nil
		}
		if !first {
			c.mu.Lock()
			c.reconnects++
			c.mu.Unlock()
		}
		first = false

		s := c.newStream()
		ended := make(chan error, 1)
		moved := make(chan struct{})
		go func() { ended <- s.run(token, sync.OnceFunc(func() { close(moved) })) }()

		var err error
		select {
		case <-moved:
			c.logger.Info("Relay pod is shutting down, opening a new stream")
			continue
		case err = <-ended:
		}
		if c.ctx.Err() != nil {
			return nil
		}
		if s.held.Load() {
			delay = retryMin
			warnedTaken = false
		}
		code := status.Code(err)
		c.mu.Lock()
		if err != nil {
			c.lastErr = err.Error()
		} else {
			c.lastErr = "the relay ended the stream"
		}
		c.mu.Unlock()
		if fatal[code] {
			return fmt.Errorf("the relay refused the tunnel: %w", err)
		}

		logger := c.logger.With("code", code.String(), "error", err)
		switch code {
		case codes.Unauthenticated:
			// Each create-session replaces the client token.
			c.refuseToken(token)
			logger.Warn("The relay refused the client token, waiting for a new one")
			continue
		case codes.AlreadyExists:
			c.setState(bridgev1.TunnelState_TUNNEL_STATE_TAKEN)
			if !warnedTaken {
				warnedTaken = true
				logger.Warn("Another client holds the tunnel, waiting for it to let go")
			}
		default:
			c.mu.Lock()
			if c.held == 0 {
				c.state = bridgev1.TunnelState_TUNNEL_STATE_CONNECTING
			}
			c.mu.Unlock()
			logger.Info("Tunnel stream ended, reconnecting", "delay", delay)
		}

		select {
		case <-time.After(delay):
		case <-c.ctx.Done():
			return nil
		}
		delay = min(delay*2, retryMax)
	}
}

// awaitToken returns the client token, waiting for SetToken while there is
// none.
func (c *Client) awaitToken() (string, bool) {
	for {
		c.mu.Lock()
		token, set := c.token, c.tokenSet
		if token == "" {
			c.state = bridgev1.TunnelState_TUNNEL_STATE_NEEDS_TOKEN
		}
		c.mu.Unlock()
		if token != "" {
			return token, true
		}
		select {
		case <-set:
		case <-c.ctx.Done():
			return "", false
		}
	}
}

// refuseToken forgets token, unless SetToken already replaced it.
func (c *Client) refuseToken(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == token {
		c.token = ""
	}
}

func (c *Client) setState(state bridgev1.TunnelState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = state
}

// setLive makes s the stream new outbound conns take; nil for none.
func (c *Client) setLive(s *stream) {
	c.live = s
	if s != nil && !c.liveClosed {
		close(c.liveCh)
		c.liveClosed = true
	} else if s == nil && c.liveClosed {
		c.liveCh = make(chan struct{})
		c.liveClosed = false
	}
}

// holding: the relay routes the tunnel's conns to s.
func (c *Client) holding(s *stream) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held++
	c.setLive(s)
	if c.state != bridgev1.TunnelState_TUNNEL_STATE_LIVE {
		c.state = bridgev1.TunnelState_TUNNEL_STATE_LIVE
		c.liveSince = time.Now()
		c.logger.Info("Tunnel live", "reconnects", c.reconnects)
	}
}

// moving: s gets no new conns, but finishes the ones open on it.
func (c *Client) moving(s *stream) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.live == s {
		c.setLive(nil)
	}
}

// lost: s ended.
func (c *Client) lost(s *stream) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.held.Load() {
		c.held--
	}
	if c.live == s {
		c.setLive(nil)
	}
	if c.held == 0 && c.state == bridgev1.TunnelState_TUNNEL_STATE_LIVE {
		c.state = bridgev1.TunnelState_TUNNEL_STATE_CONNECTING
		c.liveSince = time.Time{}
	}
}

func (c *Client) finish(err error) {
	c.closeOnce.Do(func() {
		c.cancel()
		if err != nil {
			c.mu.Lock()
			c.state = bridgev1.TunnelState_TUNNEL_STATE_FAILED
			c.lastErr = err.Error()
			c.mu.Unlock()
			c.logger.Error("Tunnel stopped", "error", err)
		}
		c.err = err
		_ = c.cc.Close()
		close(c.done)
	})
}

var errSilent = errors.New("no heartbeat from the relay")
