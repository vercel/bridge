// Package devtunnel carries a tunnel over a dev tunnel: the Connect stream to
// api-dev-tunnel that `vc dev --tunnel` holds otherwise. Stream translates the
// relay's frames to and from tunnel Messages, so the deployment's conns reach
// the app and the app's outbound conns leave through the deployment.
package devtunnel

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"regexp"
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
	// What each side of a conn may send before the other grants more.
	connWindow = 256 << 10
	// A client numbers its conns from here; the relay numbers its own from 1.
	clientConnIDFloor = uint64(1) << 63
	// The relay's frames waiting for the tunnel to take them.
	queuedMessages = 256

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

// Config configures a Stream.
type Config struct {
	// RelayURL is api-dev-tunnel's URL, from create-session.
	RelayURL string
	// Token is the client token, from create-session. Without one the stream
	// waits for SetToken.
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

// Stream holds a dev tunnel's Connect stream to its relay, replacing it
// whenever it drops, as one tunnel.Stream.
type Stream struct {
	cfg      Config
	timings  timings
	logger   *slog.Logger
	instance string
	target   string
	creds    credentials.TransportCredentials

	// Set by Start, before any call.
	rpc devtunnel.TunnelServiceClient
	ctx context.Context

	msgs chan *tunnel.Message
	done chan struct{}
	// Why it stopped; read after done.
	err error

	// Each open conn's route, by the tunnel's id for it.
	routes *xsync.MapOf[string, *route]

	mu       sync.Mutex
	token    string
	tokenSet chan struct{}
	// The newest call holding the tunnel, which new outbound conns take.
	live       *call
	liveCh     chan struct{}
	liveClosed bool
	// Calls holding the tunnel, including one finishing its conns after the
	// relay said to reconnect.
	held       int
	state      bridgev1.TunnelState
	lastErr    string
	liveSince  time.Time
	reconnects int64

	ingressConns, egressConns, ingressTotal, egressTotal atomic.Int64
	bytesSent, bytesReceived                             atomic.Int64
	nextEgress, nextCall                                 atomic.Uint64
}

// route is where one of the tunnel's conns lives on the relay's protocol.
type route struct {
	c *call
	// The conn's id on c.
	wire uint64
	dir  direction
	// Both sides have half-closed once both are set.
	closeSent, peerClosed atomic.Bool
}

type direction int

const (
	// From the deployment to the app.
	ingress direction = iota
	// From the app out through the deployment.
	egress
)

var _ tunnel.Stream = (*Stream)(nil)

// New returns a Stream for cfg. Nothing connects until Start.
func New(cfg Config) (*Stream, error) {
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
	return &Stream{
		cfg:      cfg,
		timings:  defaultTimings,
		logger:   logger.With("component", "dev-tunnel", "relay", cfg.RelayURL, "instance", instance),
		instance: instance,
		target:   target,
		creds:    creds,
		msgs:     make(chan *tunnel.Message, queuedMessages),
		done:     make(chan struct{}),
		routes:   xsync.NewMapOf[string, *route](),
		token:    cfg.Token,
		tokenSet: make(chan struct{}),
		liveCh:   make(chan struct{}),
		state:    bridgev1.TunnelState_TUNNEL_STATE_CONNECTING,
	}, nil
}

// relayTarget turns the relay's URL into a gRPC target. Start dials it with
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

// Start holds the tunnel until ctx ends, replacing its Connect stream
// whenever it drops. Recv fails once it stops: when ctx ends, or the relay
// refuses the tunnel for good.
func (d *Stream) Start(ctx context.Context) {
	d.ctx = ctx
	go func() {
		err := d.hold()
		if err != nil {
			d.mu.Lock()
			d.state = bridgev1.TunnelState_TUNNEL_STATE_FAILED
			d.lastErr = err.Error()
			d.mu.Unlock()
			d.logger.Error("Tunnel stopped", "error", err)
		}
		d.err = err
		close(d.done)
	}()
}

func (d *Stream) hold() error {
	relayDialer := d.cfg.RelayDialer
	cc, err := grpc.NewClient(d.target,
		grpc.WithTransportCredentials(d.creds),
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
	d.rpc = devtunnel.NewTunnelServiceClient(cc)
	return d.run()
}

func (d *Stream) Features() tunnel.Features {
	return tunnel.Features{Window: connWindow, HalfClose: true}
}

func (d *Stream) Recv() (*tunnel.Message, error) {
	select {
	case m := <-d.msgs:
		return m, nil
	case <-d.done:
		if d.err != nil {
			return nil, d.err
		}
		return nil, io.EOF
	}
}

func (d *Stream) Send(m *tunnel.Message) error {
	if m.Kind == tunnel.MessageOpen {
		return d.open(m)
	}
	r, ok := d.routes.Load(m.ConnID)
	if !ok {
		// The conn is over.
		return nil
	}
	switch m.Kind {
	case tunnel.MessageData:
		d.bytesSent.Add(int64(len(m.Payload)))
		r.c.send(&devtunnel.ConnectRequest{Frame: &devtunnel.ConnectRequest_Data{Data: &devtunnel.Data{ConnId: r.wire, Payload: m.Payload}}})
	case tunnel.MessageCloseWrite:
		r.closeSent.Store(true)
		r.c.send(&devtunnel.ConnectRequest{Frame: &devtunnel.ConnectRequest_Close{Close: &devtunnel.Close{ConnId: r.wire}}})
		if r.peerClosed.Load() {
			d.forget(m.ConnID, r)
		}
	case tunnel.MessageReset:
		r.c.send(resetFrame(r.wire, m.Reason))
		d.forget(m.ConnID, r)
	case tunnel.MessageWindow:
		r.c.send(&devtunnel.ConnectRequest{Frame: &devtunnel.ConnectRequest_Window{Window: &devtunnel.Window{ConnId: r.wire, Bytes: uint32(m.Bytes)}}})
	}
	return nil
}

// open sends an outbound conn out through the deployment, to its hostname if
// the intercept's DNS gave it one, or else to the address it was headed for.
// The deployment resolves the hostname as its own code would. It waits a
// while for a call to hold the tunnel.
func (d *Stream) open(m *tunnel.Message) error {
	c := d.awaitLive()
	if c == nil {
		return errors.New("no stream holds the tunnel")
	}
	r := &route{c: c, wire: clientConnIDFloor | d.nextEgress.Add(1), dir: egress}
	if !c.track(m.ConnID, r) {
		return errors.New("the tunnel's stream ended")
	}
	host := m.Conn.Hostname
	if host == "" {
		host = m.Conn.Dest.Host
	}
	c.send(&devtunnel.ConnectRequest{Frame: &devtunnel.ConnectRequest_Open{Open: &devtunnel.Open{
		ConnId:   r.wire,
		Protocol: devtunnel.Protocol_PROTOCOL_TCP,
		Host:     host,
		Port:     uint32(m.Conn.Dest.Port),
	}}})
	return nil
}

// deliver hands the tunnel one of the relay's messages.
func (d *Stream) deliver(m *tunnel.Message) {
	select {
	case d.msgs <- m:
	case <-d.done:
	}
}

// forget drops id's route if it's still r. Reports whether it did.
func (d *Stream) forget(id string, r *route) bool {
	removed := false
	d.routes.Compute(id, func(cur *route, loaded bool) (*route, bool) {
		if !loaded {
			return nil, true
		}
		if cur != r {
			return cur, false
		}
		removed = true
		return cur, true
	})
	if removed {
		r.c.ids.Delete(r.wire)
		d.counters(r.dir, -1)
	}
	return removed
}

// counters tracks conns opening (delta 1) and ending (delta -1).
func (d *Stream) counters(dir direction, delta int64) {
	open, total := &d.ingressConns, &d.ingressTotal
	if dir == egress {
		open, total = &d.egressConns, &d.egressTotal
	}
	open.Add(delta)
	if delta > 0 {
		total.Add(delta)
	}
}

// SetToken replaces the client token. A stream waiting for one connects with
// it; a live one uses it on its next call.
func (d *Stream) SetToken(token string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.token = token
	close(d.tokenSet)
	d.tokenSet = make(chan struct{})
}

// Status reports the tunnel's state and traffic.
func (d *Stream) Status() *bridgev1.TunnelStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := &bridgev1.TunnelStatus{
		State:             d.state,
		LastError:         d.lastErr,
		Reconnects:        d.reconnects,
		IngressConns:      d.ingressConns.Load(),
		EgressConns:       d.egressConns.Load(),
		IngressConnsTotal: d.ingressTotal.Load(),
		EgressConnsTotal:  d.egressTotal.Load(),
		BytesSent:         d.bytesSent.Load(),
		BytesReceived:     d.bytesReceived.Load(),
	}
	if !d.liveSince.IsZero() {
		st.LiveSinceUnixSeconds = d.liveSince.Unix()
	}
	return st
}

// awaitLive returns the call new outbound conns take, waiting a while for
// one, or nil.
func (d *Stream) awaitLive() *call {
	timer := time.NewTimer(d.timings.liveWait)
	defer timer.Stop()
	for {
		d.mu.Lock()
		c, ch := d.live, d.liveCh
		d.mu.Unlock()
		if c != nil {
			return c
		}
		select {
		case <-ch:
		case <-timer.C:
			return nil
		case <-d.done:
			return nil
		}
	}
}

// run replaces the tunnel's call whenever it ends, for as long as it takes:
// the relay's pods roll often. A call the relay says to move off keeps its
// open conns while the next one opens.
func (d *Stream) run() error {
	delay := retryMin
	first := true
	warnedTaken := false
	for {
		token, ok := d.awaitToken()
		if !ok {
			return nil
		}
		if !first {
			d.mu.Lock()
			d.reconnects++
			d.mu.Unlock()
		}
		first = false

		c := d.newCall()
		ended := make(chan error, 1)
		moved := make(chan struct{})
		go func() { ended <- c.run(token, sync.OnceFunc(func() { close(moved) })) }()

		var err error
		select {
		case <-moved:
			d.logger.Info("Relay pod is shutting down, opening a new stream")
			continue
		case err = <-ended:
		}
		if d.ctx.Err() != nil {
			return nil
		}
		if c.held.Load() {
			delay = retryMin
			warnedTaken = false
		}
		code := status.Code(err)
		d.mu.Lock()
		if err != nil {
			d.lastErr = err.Error()
		} else {
			d.lastErr = "the relay ended the stream"
		}
		d.mu.Unlock()
		if fatal[code] {
			return fmt.Errorf("the relay refused the tunnel: %w", err)
		}

		logger := d.logger.With("code", code.String(), "error", err)
		switch code {
		case codes.Unauthenticated:
			// Each create-session replaces the client token.
			d.refuseToken(token)
			logger.Warn("The relay refused the client token, waiting for a new one")
			continue
		case codes.AlreadyExists:
			d.setState(bridgev1.TunnelState_TUNNEL_STATE_TAKEN)
			if !warnedTaken {
				warnedTaken = true
				logger.Warn("Another client holds the tunnel, waiting for it to let go")
			}
		default:
			d.mu.Lock()
			if d.held == 0 {
				d.state = bridgev1.TunnelState_TUNNEL_STATE_CONNECTING
			}
			d.mu.Unlock()
			logger.Info("Tunnel stream ended, reconnecting", "delay", delay)
		}

		select {
		case <-time.After(delay):
		case <-d.ctx.Done():
			return nil
		}
		delay = min(delay*2, retryMax)
	}
}

// awaitToken returns the client token, waiting for SetToken while there is
// none.
func (d *Stream) awaitToken() (string, bool) {
	for {
		d.mu.Lock()
		token, set := d.token, d.tokenSet
		if token == "" {
			d.state = bridgev1.TunnelState_TUNNEL_STATE_NEEDS_TOKEN
		}
		d.mu.Unlock()
		if token != "" {
			return token, true
		}
		select {
		case <-set:
		case <-d.ctx.Done():
			return "", false
		}
	}
}

// refuseToken forgets token, unless SetToken already replaced it.
func (d *Stream) refuseToken(token string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.token == token {
		d.token = ""
	}
}

func (d *Stream) setState(state bridgev1.TunnelState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.state = state
}

// setLive makes c the call new outbound conns take; nil for none.
func (d *Stream) setLive(c *call) {
	d.live = c
	if c != nil && !d.liveClosed {
		close(d.liveCh)
		d.liveClosed = true
	} else if c == nil && d.liveClosed {
		d.liveCh = make(chan struct{})
		d.liveClosed = false
	}
}

// holding: the relay routes the tunnel's conns to c.
func (d *Stream) holding(c *call) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.held++
	d.setLive(c)
	if d.state != bridgev1.TunnelState_TUNNEL_STATE_LIVE {
		d.state = bridgev1.TunnelState_TUNNEL_STATE_LIVE
		d.liveSince = time.Now()
		d.logger.Info("Tunnel live", "reconnects", d.reconnects)
	}
}

// moving: c gets no new conns, but finishes the ones open on it.
func (d *Stream) moving(c *call) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.live == c {
		d.setLive(nil)
	}
}

// lost: c ended.
func (d *Stream) lost(c *call) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c.held.Load() {
		d.held--
	}
	if d.live == c {
		d.setLive(nil)
	}
	if d.held == 0 && d.state == bridgev1.TunnelState_TUNNEL_STATE_LIVE {
		d.state = bridgev1.TunnelState_TUNNEL_STATE_CONNECTING
		d.liveSince = time.Time{}
	}
}

var errSilent = errors.New("no heartbeat from the relay")
