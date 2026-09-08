package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
)

const testTimeout = 5 * time.Second

type fakeStream struct {
	in  chan *bridgev1.TunnelNetworkMessage
	out chan *bridgev1.TunnelNetworkMessage
}

func newFakeStream() *fakeStream {
	return &fakeStream{
		in:  make(chan *bridgev1.TunnelNetworkMessage),
		out: make(chan *bridgev1.TunnelNetworkMessage, 64),
	}
}

func (s *fakeStream) Send(msg *bridgev1.TunnelNetworkMessage) error {
	s.out <- msg
	return nil
}

func (s *fakeStream) Recv() (*bridgev1.TunnelNetworkMessage, error) {
	msg, ok := <-s.in
	if !ok {
		return nil, io.EOF
	}
	return msg, nil
}

// fakeConn records writes and blocks reads until closed.
type fakeConn struct {
	mu      sync.Mutex
	written []byte
	done    chan struct{}
	once    sync.Once
}

func newFakeConn() *fakeConn {
	return &fakeConn{done: make(chan struct{})}
}

func (c *fakeConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	c.written = append(c.written, b...)
	c.mu.Unlock()
	return len(b), nil
}

func (c *fakeConn) Read([]byte) (int, error) {
	<-c.done
	return 0, io.EOF
}

func (c *fakeConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

func (c *fakeConn) Written() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.written)
}

func (c *fakeConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *fakeConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *fakeConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }

// blockingDialer counts dials and parks each one until release is closed.
type blockingDialer struct {
	dials   atomic.Int32
	release chan struct{}
	err     error
	mu      sync.Mutex
	conns   []*fakeConn
}

func newBlockingDialer() *blockingDialer {
	return &blockingDialer{release: make(chan struct{})}
}

func (d *blockingDialer) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	d.dials.Add(1)
	select {
	case <-d.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if d.err != nil {
		return nil, d.err
	}
	conn := newFakeConn()
	d.mu.Lock()
	d.conns = append(d.conns, conn)
	d.mu.Unlock()
	return conn, nil
}

func (d *blockingDialer) connCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.conns)
}

func (d *blockingDialer) conn(t *testing.T, i int) *fakeConn {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.conns) <= i {
		t.Fatalf("expected at least %d dialed conns, got %d", i+1, len(d.conns))
	}
	return d.conns[i]
}

func dataMsg(connID string, data string) *bridgev1.TunnelNetworkMessage {
	return &bridgev1.TunnelNetworkMessage{
		ConnectionId: connID,
		Source:       &bridgev1.TunnelAddress{Ip: "172.17.0.3", Port: 55988},
		Dest:         &bridgev1.TunnelAddress{Ip: "172.20.143.45", Port: 80},
		Data:         []byte(data),
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func startTunnel(t *testing.T, dialer *blockingDialer) (*fakeStream, Tunnel) {
	t.Helper()
	stream := newFakeStream()
	tun := New(dialer, stream)
	ctx, cancel := context.WithCancel(context.Background())
	tun.Start(ctx)
	t.Cleanup(func() {
		cancel()
		tun.Close()
	})
	return stream, tun
}

func TestChunksArrivingDuringDialShareOneConnection(t *testing.T) {
	dialer := newBlockingDialer()
	stream, _ := startTunnel(t, dialer)
	const connID = "172.17.0.3:55988->172.20.143.45:80"

	stream.in <- dataMsg(connID, "PRI * HTTP/2.0\r\n")
	stream.in <- dataMsg(connID, "HEADERS")
	waitFor(t, "first dial", func() bool { return dialer.dials.Load() >= 1 })
	stream.in <- dataMsg(connID, "DATA")

	close(dialer.release)

	waitFor(t, "dial to return", func() bool { return dialer.connCount() >= 1 })
	if got := dialer.dials.Load(); got != 1 {
		t.Fatalf("expected exactly one dial for the connection, got %d", got)
	}
	conn := dialer.conn(t, 0)
	waitFor(t, "queued chunks to flush", func() bool {
		return conn.Written() == "PRI * HTTP/2.0\r\nHEADERSDATA"
	})

	stream.in <- dataMsg(connID, "MORE")
	waitFor(t, "post-dial chunk to write through", func() bool {
		return conn.Written() == "PRI * HTTP/2.0\r\nHEADERSDATAMORE"
	})
}

func TestDialFailureReportsErrorAndForgetsConnection(t *testing.T) {
	dialer := newBlockingDialer()
	dialer.err = errors.New("dial tcp: connection refused")
	close(dialer.release)
	stream, _ := startTunnel(t, dialer)
	const connID = "172.17.0.3:55988->172.20.143.45:80"

	stream.in <- dataMsg(connID, "hello")

	select {
	case msg := <-stream.out:
		if msg.GetConnectionId() != connID || msg.GetError() == "" {
			t.Fatalf("expected error reply for %s, got %+v", connID, msg)
		}
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the connect-failed reply")
	}

	stream.in <- dataMsg(connID, "hello again")
	waitFor(t, "a fresh dial after the failed one was forgotten", func() bool {
		return dialer.dials.Load() == 2
	})
}

func TestPeerErrorDuringDialClosesTheDialedConnection(t *testing.T) {
	dialer := newBlockingDialer()
	stream, tun := startTunnel(t, dialer)
	const connID = "172.17.0.3:55988->172.20.143.45:80"

	stream.in <- dataMsg(connID, "hello")
	waitFor(t, "first dial", func() bool { return dialer.dials.Load() == 1 })
	stream.in <- &bridgev1.TunnelNetworkMessage{ConnectionId: connID, Error: "peer closed"}
	waitFor(t, "the peer error to drop the pending connection", func() bool {
		_, loaded := tun.(*tunnelImpl).conns.Load(connID)
		return !loaded
	})

	close(dialer.release)

	waitFor(t, "dial to return", func() bool { return dialer.connCount() == 1 })
	conn := dialer.conn(t, 0)
	select {
	case <-conn.done:
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the dialed conn to be closed")
	}
	if got := conn.Written(); got != "" {
		t.Fatalf("expected nothing written to a connection closed before dial completed, got %q", got)
	}
}

func TestMessageWithoutDestIsIgnored(t *testing.T) {
	dialer := newBlockingDialer()
	close(dialer.release)
	stream, _ := startTunnel(t, dialer)

	stream.in <- &bridgev1.TunnelNetworkMessage{ConnectionId: "unknown", Error: "late reply"}
	stream.in <- dataMsg("172.17.0.3:1->172.20.143.45:80", "hello")

	waitFor(t, "the well-formed message to dial", func() bool { return dialer.dials.Load() == 1 })
}
