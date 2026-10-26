package devtunnel

import (
	"bytes"
	"context"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	devtunnelv1 "github.com/vercel/bridge/api/go/vercel/api/dev_tunnel/v1"
	"github.com/vercel/bridge/pkg/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const testTimeout = 5 * time.Second

// More than one Data frame's worth.
const frameBytes = 64 << 10

// fakeRelay stands in for api-dev-tunnel, handing each Connect call to the
// test.
type fakeRelay struct {
	devtunnelv1.UnimplementedTunnelServiceServer
	url     string
	streams chan *relayStream
	// refuse ends a call before it reaches the test, unless it returns nil.
	refuse func(md metadata.MD) error
}

type relayStream struct {
	md   metadata.MD
	call grpc.BidiStreamingServer[devtunnelv1.ConnectRequest, devtunnelv1.ConnectResponse]
	recv chan *devtunnelv1.ConnectRequest
	end  chan error
}

func startRelay(t *testing.T) *fakeRelay {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	r := &fakeRelay{url: "http://" + lis.Addr().String(), streams: make(chan *relayStream, 8)}
	srv := grpc.NewServer()
	devtunnelv1.RegisterTunnelServiceServer(srv, r)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return r
}

func (r *fakeRelay) Connect(call grpc.BidiStreamingServer[devtunnelv1.ConnectRequest, devtunnelv1.ConnectResponse]) error {
	md, _ := metadata.FromIncomingContext(call.Context())
	if r.refuse != nil {
		if err := r.refuse(md); err != nil {
			return err
		}
	}
	s := &relayStream{md: md, call: call, recv: make(chan *devtunnelv1.ConnectRequest, 256), end: make(chan error, 1)}
	go func() {
		defer close(s.recv)
		for {
			msg, err := call.Recv()
			if err != nil {
				return
			}
			s.recv <- msg
		}
	}()
	r.streams <- s
	select {
	case err := <-s.end:
		return err
	case <-call.Context().Done():
		return nil
	}
}

func (r *fakeRelay) next(t *testing.T) *relayStream {
	t.Helper()
	select {
	case s := <-r.streams:
		return s
	case <-time.After(testTimeout):
		t.Fatal("no Connect call reached the relay")
		return nil
	}
}

func (s *relayStream) send(t *testing.T, msg *devtunnelv1.ConnectResponse) {
	t.Helper()
	require.NoError(t, s.call.Send(msg))
}

// hold makes the stream the tunnel's, as the relay's first heartbeat does.
func (s *relayStream) hold(t *testing.T) {
	s.send(t, &devtunnelv1.ConnectResponse{Frame: &devtunnelv1.ConnectResponse_Heartbeat{Heartbeat: &devtunnelv1.Heartbeat{}}})
}

// expect returns the next frame from the client other than a heartbeat.
func (s *relayStream) expect(t *testing.T) *devtunnelv1.ConnectRequest {
	t.Helper()
	timeout := time.After(testTimeout)
	for {
		select {
		case msg, ok := <-s.recv:
			require.True(t, ok, "the client ended the stream")
			if msg.GetHeartbeat() == nil {
				return msg
			}
		case <-timeout:
			t.Fatal("no frame from the client")
			return nil
		}
	}
}

// expectData collects the client's Data on a conn until it has n bytes.
func (s *relayStream) expectData(t *testing.T, id uint64, n int) []byte {
	t.Helper()
	var got []byte
	for len(got) < n {
		msg := s.expect(t)
		if d := msg.GetData(); d != nil && d.GetConnId() == id {
			got = append(got, d.GetPayload()...)
		}
	}
	return got
}

func open(id uint64, port int) *devtunnelv1.ConnectResponse {
	return &devtunnelv1.ConnectResponse{Frame: &devtunnelv1.ConnectResponse_Open{Open: &devtunnelv1.Open{
		ConnId: id, Protocol: devtunnelv1.Protocol_PROTOCOL_TCP, Port: uint32(port),
	}}}
}

func data(id uint64, payload string) *devtunnelv1.ConnectResponse {
	return &devtunnelv1.ConnectResponse{Frame: &devtunnelv1.ConnectResponse_Data{Data: &devtunnelv1.Data{ConnId: id, Payload: []byte(payload)}}}
}

func closeConn(id uint64) *devtunnelv1.ConnectResponse {
	return &devtunnelv1.ConnectResponse{Frame: &devtunnelv1.ConnectResponse_Close{Close: &devtunnelv1.Close{ConnId: id}}}
}

func window(id uint64, n int) *devtunnelv1.ConnectResponse {
	return &devtunnelv1.ConnectResponse{Frame: &devtunnelv1.ConnectResponse_Window{Window: &devtunnelv1.Window{ConnId: id, Bytes: uint32(n)}}}
}

// newTunnel runs a tunnel over a Backend for r.
func newTunnel(t *testing.T, r *fakeRelay, token string, port int, tune ...func(*timings)) (*Backend, tunnel.Tunnel) {
	t.Helper()
	b, err := New(Config{RelayURL: r.url, Token: token, Instance: "test-instance", Port: port})
	require.NoError(t, err)
	b.timings = timings{heartbeat: 50 * time.Millisecond, silent: time.Second, liveWait: 2 * time.Second}
	for _, f := range tune {
		f(&b.timings)
	}
	tun := tunnel.NewWithBackend(&net.Dialer{Timeout: time.Second}, b)
	tun.Start(context.Background())
	t.Cleanup(func() { _ = tun.Close() })
	return b, tun
}

// echoApp echoes each connection, then half-closes it once the peer has.
func echoApp(t *testing.T) int {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = lis.Close() })
	go func() {
		for {
			nc, err := lis.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(nc, nc)
				_ = nc.(*net.TCPConn).CloseWrite()
			}()
		}
	}()
	return lis.Addr().(*net.TCPAddr).Port
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	require.Eventually(t, cond, testTimeout, 10*time.Millisecond)
}

func TestIngressReachesTheApp(t *testing.T) {
	r := startRelay(t)
	port := echoApp(t)
	c, _ := newTunnel(t, r, "tok", port)

	s := r.next(t)
	assert.Equal(t, []string{"Bearer tok"}, s.md.Get("authorization"))
	assert.Equal(t, []string{"test-instance"}, s.md.Get(instanceHeader))
	s.hold(t)
	waitFor(t, func() bool { return c.Status().GetState() == bridgev1.TunnelState_TUNNEL_STATE_LIVE })

	s.send(t, open(1, port))
	s.send(t, data(1, "hello"))
	var echoed []byte
	granted := 0
	for len(echoed) < 5 || granted < 5 {
		msg := s.expect(t)
		if d := msg.GetData(); d != nil {
			echoed = append(echoed, d.GetPayload()...)
		}
		if w := msg.GetWindow(); w != nil {
			granted += int(w.GetBytes())
		}
	}
	assert.Equal(t, "hello", string(echoed))

	// The relay's half-close reaches the app, whose own comes back.
	s.send(t, closeConn(1))
	assert.Equal(t, uint64(1), s.expect(t).GetClose().GetConnId())
	waitFor(t, func() bool { return c.Status().GetIngressConns() == 0 })
	st := c.Status()
	assert.Equal(t, int64(1), st.GetIngressConnsTotal())
	assert.Equal(t, int64(5), st.GetBytesSent())
	assert.Equal(t, int64(5), st.GetBytesReceived())
}

func TestIngressToAnotherPortIsReset(t *testing.T) {
	r := startRelay(t)
	newTunnel(t, r, "tok", 3000)
	s := r.next(t)
	s.hold(t)

	s.send(t, open(1, 22))
	reset := s.expect(t).GetReset_()
	require.NotNil(t, reset)
	assert.Equal(t, uint64(1), reset.GetConnId())
	assert.Equal(t, "bridge serves only port 3000", reset.GetReason())
}

func TestIngressSendsNoMoreThanTheWindow(t *testing.T) {
	r := startRelay(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = lis.Close() })
	body := bytes.Repeat([]byte("x"), connWindow+frameBytes)
	go func() {
		nc, err := lis.Accept()
		if err == nil {
			_, _ = nc.Write(body)
		}
	}()
	port := lis.Addr().(*net.TCPAddr).Port
	newTunnel(t, r, "tok", port)
	s := r.next(t)
	s.hold(t)
	s.send(t, open(1, port))

	assert.Len(t, s.expectData(t, 1, connWindow), connWindow)
	select {
	case msg := <-s.recv:
		if msg.GetData() != nil {
			t.Fatal("sent past the window")
		}
	case <-time.After(200 * time.Millisecond):
	}

	s.send(t, window(1, frameBytes))
	assert.Len(t, s.expectData(t, 1, frameBytes), frameBytes)
}

func TestEgressLeavesThroughTheDeployment(t *testing.T) {
	r := startRelay(t)
	c, tun := newTunnel(t, r, "tok", 3000)
	s := r.next(t)
	s.hold(t)
	waitFor(t, func() bool { return c.Status().GetState() == bridgev1.TunnelState_TUNNEL_STATE_LIVE })

	app, intercepted := tcpPair(t)
	tun.AddConn(intercepted, "0.0.0.0:443", "example.com")
	o := s.expect(t).GetOpen()
	require.NotNil(t, o)
	assert.GreaterOrEqual(t, o.GetConnId(), clientConnIDFloor)
	assert.Equal(t, "example.com", o.GetHost())
	assert.Equal(t, uint32(443), o.GetPort())
	assert.Equal(t, devtunnelv1.Protocol_PROTOCOL_TCP, o.GetProtocol())
	id := o.GetConnId()

	_, err := app.Write([]byte("GET"))
	require.NoError(t, err)
	assert.Equal(t, "GET", string(s.expectData(t, id, 3)))

	s.send(t, data(id, "200"))
	buf := make([]byte, 3)
	_, err = io.ReadFull(app, buf)
	require.NoError(t, err)
	assert.Equal(t, "200", string(buf))

	// The deployment couldn't reach it, or it went away.
	s.send(t, &devtunnelv1.ConnectResponse{Frame: &devtunnelv1.ConnectResponse_Reset_{Reset_: &devtunnelv1.Reset{ConnId: id, Reason: "refused"}}})
	_ = app.SetReadDeadline(time.Now().Add(testTimeout))
	_, err = app.Read(buf)
	assert.Error(t, err)
	waitFor(t, func() bool { return c.Status().GetEgressConns() == 0 })
	assert.Equal(t, int64(1), c.Status().GetEgressConnsTotal())
}

func TestEgressWithNoLiveStreamIsDropped(t *testing.T) {
	r := startRelay(t)
	_, tun := newTunnel(t, r, "tok", 3000, func(tm *timings) { tm.liveWait = 50 * time.Millisecond })
	r.next(t) // never holds the tunnel

	app, intercepted := tcpPair(t)
	tun.AddConn(intercepted, "0.0.0.0:443", "example.com")
	_ = app.SetReadDeadline(time.Now().Add(testTimeout))
	_, err := app.Read(make([]byte, 1))
	assert.ErrorIs(t, err, io.EOF)
}

func TestReconnectKeepsTheOldStreamsConns(t *testing.T) {
	r := startRelay(t)
	port := echoApp(t)
	c, _ := newTunnel(t, r, "tok", port)
	first := r.next(t)
	first.hold(t)
	first.send(t, open(1, port))
	first.send(t, data(1, "a"))
	assert.Equal(t, "a", string(first.expectData(t, 1, 1)))

	first.send(t, &devtunnelv1.ConnectResponse{Frame: &devtunnelv1.ConnectResponse_Reconnect{Reconnect: &devtunnelv1.Reconnect{}}})
	second := r.next(t)
	assert.Equal(t, []string{"test-instance"}, second.md.Get(instanceHeader))
	second.hold(t)

	first.send(t, data(1, "b"))
	assert.Equal(t, "b", string(first.expectData(t, 1, 1)))
	st := c.Status()
	assert.Equal(t, bridgev1.TunnelState_TUNNEL_STATE_LIVE, st.GetState())
	assert.Equal(t, int64(1), st.GetReconnects())
}

func TestRefusedTokenWaitsForANewOne(t *testing.T) {
	r := startRelay(t)
	r.refuse = func(md metadata.MD) error {
		if md.Get("authorization")[0] == "Bearer old" {
			return status.Error(codes.Unauthenticated, "missing or invalid dev tunnel token")
		}
		return nil
	}
	c, _ := newTunnel(t, r, "old", 3000)
	waitFor(t, func() bool { return c.Status().GetState() == bridgev1.TunnelState_TUNNEL_STATE_NEEDS_TOKEN })

	c.SetToken("new")
	s := r.next(t)
	assert.Equal(t, []string{"Bearer new"}, s.md.Get("authorization"))
}

func TestNoTokenWaitsForOne(t *testing.T) {
	r := startRelay(t)
	c, _ := newTunnel(t, r, "", 3000)
	waitFor(t, func() bool { return c.Status().GetState() == bridgev1.TunnelState_TUNNEL_STATE_NEEDS_TOKEN })
	select {
	case <-r.streams:
		t.Fatal("connected without a token")
	case <-time.After(100 * time.Millisecond):
	}

	c.SetToken("tok")
	assert.Equal(t, []string{"Bearer tok"}, r.next(t).md.Get("authorization"))
}

func TestFatalRefusalStops(t *testing.T) {
	r := startRelay(t)
	r.refuse = func(metadata.MD) error {
		return status.Error(codes.PermissionDenied, "not allowed")
	}
	c, tun := newTunnel(t, r, "tok", 3000)

	select {
	case <-tun.Done():
	case <-time.After(testTimeout):
		t.Fatal("kept retrying")
	}
	assert.Contains(t, c.Status().GetLastError(), "not allowed")
	assert.Equal(t, bridgev1.TunnelState_TUNNEL_STATE_FAILED, c.Status().GetState())
}

func TestSilentRelayIsReplaced(t *testing.T) {
	r := startRelay(t)
	c, _ := newTunnel(t, r, "tok", 3000, func(tm *timings) { tm.silent = 200 * time.Millisecond })
	r.next(t) // never heard from again

	r.next(t)
	assert.Contains(t, c.Status().GetLastError(), errSilent.Error())
}

func TestRelayTarget(t *testing.T) {
	target, _, err := relayTarget("https://api-dev-tunnel.example.com")
	require.NoError(t, err)
	assert.Equal(t, "passthrough:///api-dev-tunnel.example.com:443", target)

	target, _, err = relayTarget("http://127.0.0.1:" + strconv.Itoa(8080))
	require.NoError(t, err)
	assert.Equal(t, "passthrough:///127.0.0.1:8080", target)

	for _, bad := range []string{"ftp://relay", "https://relay/v1", "https://"} {
		_, _, err := relayTarget(bad)
		assert.Error(t, err, bad)
	}
}

// tcpPair returns both ends of a TCP connection: the app's, and the one the
// transparent proxy would have accepted.
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer lis.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		nc, _ := lis.Accept()
		accepted <- nc
	}()
	app, err := net.Dial("tcp", lis.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = app.Close() })
	return app, <-accepted
}
