package tunnel

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

const testTimeout = 5 * time.Second

// fakeStream hands the tunnel the messages a test gives it, and keeps what the
// tunnel sends.
type fakeStream struct {
	features Features
	in       chan *Message
	out      chan *Message
}

func newFakeStream(f Features) *fakeStream {
	return &fakeStream{features: f, in: make(chan *Message, 64), out: make(chan *Message, 1024)}
}

func (s *fakeStream) Send(m *Message) error { s.out <- m; return nil }
func (s *fakeStream) Features() Features    { return s.features }

func (s *fakeStream) Recv() (*Message, error) {
	m, ok := <-s.in
	if !ok {
		return nil, io.EOF
	}
	return m, nil
}

// next returns the next message the tunnel sent other than a window grant.
func (s *fakeStream) next(t *testing.T) *Message {
	t.Helper()
	for {
		select {
		case m := <-s.out:
			if m.Kind != MessageWindow {
				return m
			}
		case <-time.After(testTimeout):
			t.Fatal("the tunnel sent nothing")
		}
	}
}

func startTunnel(t *testing.T, s Stream) Tunnel {
	t.Helper()
	tun := New(&net.Dialer{Timeout: time.Second}, s)
	tun.Start(context.Background())
	t.Cleanup(func() { tun.Close() })
	return tun
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
	s := newFakeStream(Features{Window: 1 << 20, HalfClose: true})
	startTunnel(t, s)

	s.in <- &Message{Kind: MessageOpen, ConnID: "1", Conn: &ConnInfo{Dest: SplitAddr(app.Addr().String())}}
	s.in <- &Message{Kind: MessageData, ConnID: "1", Payload: []byte("hi")}
	s.in <- &Message{Kind: MessageCloseWrite, ConnID: "1"}

	select {
	case req := <-got:
		if req != "hi" {
			t.Fatalf("the app read %q", req)
		}
	case <-time.After(testTimeout):
		t.Fatal("the app never saw the peer's half-close")
	}
	if m := s.next(t); m.Kind != MessageData || string(m.Payload) != "bye" {
		t.Fatalf("expected the app's answer, got %+v", m)
	}
	if m := s.next(t); m.Kind != MessageCloseWrite {
		t.Fatalf("expected the app's half-close, got %+v", m)
	}
}

func TestWritesWaitForWindow(t *testing.T) {
	s := newFakeStream(Features{Window: 10, HalfClose: true})
	tun := startTunnel(t, s)
	app, intercepted := net.Pipe()
	defer app.Close()
	tun.AddConn(intercepted, "10.0.0.1:443", "example.com")
	open := s.next(t)
	if open.Kind != MessageOpen || open.Conn.Hostname != "example.com" || open.Conn.Dest.Port != 443 {
		t.Fatalf("expected the open, got %+v", open)
	}

	go func() { _, _ = app.Write([]byte("0123456789abcdefghijklmno")) }()
	sent := ""
	for len(sent) < 10 {
		sent += string(s.next(t).Payload)
	}
	select {
	case m := <-s.out:
		t.Fatalf("sent past the window: %+v", m)
	case <-time.After(100 * time.Millisecond):
	}

	s.in <- &Message{Kind: MessageWindow, ConnID: open.ConnID, Bytes: 15}
	for len(sent) < 25 {
		sent += string(s.next(t).Payload)
	}
	if sent != "0123456789abcdefghijklmno" {
		t.Fatalf("sent %q", sent)
	}
}
