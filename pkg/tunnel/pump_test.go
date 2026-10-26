package tunnel

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// fakeBackend has the flow control and half-close a dev tunnel has.
type fakeBackend struct {
	window   int
	frames   chan Frame
	sent     chan Frame
	released chan string
}

func newFakeBackend(window int) *fakeBackend {
	return &fakeBackend{
		window:   window,
		frames:   make(chan Frame, 16),
		sent:     make(chan Frame, 1024),
		released: make(chan string, 16),
	}
}

func (b *fakeBackend) Run(ctx context.Context, deliver func(Frame)) error {
	for {
		select {
		case f := <-b.frames:
			deliver(f)
		case <-ctx.Done():
			return nil
		}
	}
}

func (b *fakeBackend) ConnID(*ConnInfo) string                       { return "out" }
func (b *fakeBackend) Open(context.Context, string, *ConnInfo) error { return nil }
func (b *fakeBackend) Send(f Frame)                                  { b.sent <- f }
func (b *fakeBackend) Release(id string)                             { b.released <- id }
func (b *fakeBackend) Window() int                                   { return b.window }
func (b *fakeBackend) HalfClose() bool                               { return true }

// next returns the next frame the tunnel sent other than a window grant.
func (b *fakeBackend) next(t *testing.T) Frame {
	t.Helper()
	for {
		select {
		case f := <-b.sent:
			if f.Kind != FrameWindow {
				return f
			}
		case <-time.After(testTimeout):
			t.Fatal("the tunnel sent nothing")
		}
	}
}

func startBackendTunnel(t *testing.T, b *fakeBackend) Tunnel {
	t.Helper()
	tun := NewWithBackend(&net.Dialer{Timeout: time.Second}, b)
	tun.Start(context.Background())
	t.Cleanup(func() { tun.Close() })
	return tun
}

func TestHalfCloseCrossesThePump(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	got := make(chan string, 1)
	go func() {
		nc, err := lis.Accept()
		if err != nil {
			return
		}
		// Reads to the peer's half-close, then answers and closes.
		req, _ := io.ReadAll(nc)
		got <- string(req)
		_, _ = nc.Write([]byte("bye"))
		_ = nc.Close()
	}()
	b := newFakeBackend(1 << 20)
	startBackendTunnel(t, b)
	dest := splitAddr(lis.Addr().String())

	b.frames <- Frame{Kind: FrameOpen, ConnID: "1", Conn: &ConnInfo{Dest: dest}}
	b.frames <- Frame{Kind: FrameData, ConnID: "1", Payload: []byte("hi")}
	b.frames <- Frame{Kind: FrameCloseWrite, ConnID: "1"}

	select {
	case req := <-got:
		if req != "hi" {
			t.Fatalf("the app read %q", req)
		}
	case <-time.After(testTimeout):
		t.Fatal("the app never saw the peer's half-close")
	}
	if f := b.next(t); f.Kind != FrameData || string(f.Payload) != "bye" {
		t.Fatalf("expected the app's answer, got %+v", f)
	}
	if f := b.next(t); f.Kind != FrameCloseWrite {
		t.Fatalf("expected the app's half-close, got %+v", f)
	}
	select {
	case id := <-b.released:
		if id != "1" {
			t.Fatalf("released %q", id)
		}
	case <-time.After(testTimeout):
		t.Fatal("the conn was never released")
	}
}

func TestWritesWaitForWindow(t *testing.T) {
	b := newFakeBackend(10)
	tun := startBackendTunnel(t, b)
	app, intercepted := net.Pipe()
	defer app.Close()
	tun.AddConn(intercepted, "10.0.0.1:443", "example.com")

	go func() { _, _ = app.Write([]byte("0123456789abcdefghijklmno")) }()
	sent := ""
	for len(sent) < 10 {
		sent += string(b.next(t).Payload)
	}
	select {
	case f := <-b.sent:
		t.Fatalf("sent past the window: %+v", f)
	case <-time.After(100 * time.Millisecond):
	}

	b.frames <- Frame{Kind: FrameWindow, ConnID: "out", Bytes: 15}
	for len(sent) < 25 {
		sent += string(b.next(t).Payload)
	}
	if sent != "0123456789abcdefghijklmno" {
		t.Fatalf("sent %q", sent)
	}
}
