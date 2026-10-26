package tunnel

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// acceptingBackend hands the tunnel the conns a test gives it.
type acceptingBackend struct {
	conns chan net.Conn
	dest  Addr
}

func (b *acceptingBackend) Run(ctx context.Context, accept func(net.Conn, *ConnInfo)) error {
	for {
		select {
		case c := <-b.conns:
			accept(c, &ConnInfo{Dest: b.dest})
		case <-ctx.Done():
			return nil
		}
	}
}

func (b *acceptingBackend) Open(context.Context, *ConnInfo) (net.Conn, error) {
	return nil, net.ErrClosed
}

// tcpPair returns both ends of a TCP connection.
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := lis.Accept()
		accepted <- c
	}()
	a, err := net.Dial("tcp", lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b := <-accepted
	t.Cleanup(func() {
		a.Close()
		b.Close()
	})
	return a, b
}

func TestHalfCloseCrossesThePump(t *testing.T) {
	app, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	got := make(chan string, 1)
	go func() {
		c, err := app.Accept()
		if err != nil {
			return
		}
		// Reads to the peer's half-close, then answers and closes.
		req, _ := io.ReadAll(c)
		got <- string(req)
		_, _ = c.Write([]byte("bye"))
		_ = c.Close()
	}()

	b := &acceptingBackend{conns: make(chan net.Conn, 1), dest: splitAddr(app.Addr().String())}
	tun := NewWithBackend(&net.Dialer{Timeout: time.Second}, b)
	tun.Start(context.Background())
	t.Cleanup(func() { tun.Close() })

	peer, remote := tcpPair(t)
	b.conns <- remote
	_, _ = peer.Write([]byte("hi"))
	_ = peer.(*net.TCPConn).CloseWrite()

	select {
	case req := <-got:
		if req != "hi" {
			t.Fatalf("the app read %q", req)
		}
	case <-time.After(testTimeout):
		t.Fatal("the app never saw the peer's half-close")
	}
	_ = peer.SetReadDeadline(time.Now().Add(testTimeout))
	resp, err := io.ReadAll(peer)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp) != "bye" {
		t.Fatalf("the peer read %q", resp)
	}
}
