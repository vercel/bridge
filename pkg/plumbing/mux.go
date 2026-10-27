package plumbing

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
)

// Frames writes one conn's frames onto a multiplexed transport.
type Frames interface {
	// Data sends payload, which Frames may keep.
	Data(payload []byte) error
	// CloseWrite tells the peer this side has nothing more to send. A
	// transport that can't returns errors.ErrUnsupported, and the conn ends
	// instead.
	CloseWrite() error
	// Reset abandons the conn on the peer, for reason.
	Reset(reason string) error
	// Release forgets the conn once it's over.
	Release()
}

// MuxConfig configures a MuxStream.
type MuxConfig struct {
	// MaxFrame caps each Data's payload; 0 for no cap.
	MaxFrame int
	// Queue is how many payloads from the peer may wait unread before Deliver
	// waits; 0 for no limit.
	Queue         int
	Local, Remote net.Addr
}

// MuxStream is one conn's Stream on a multiplexed transport: Recv returns what
// the transport Delivers for it, and Send, CloseSend and Close write its
// frames. Conn is a metered net.Conn over it.
type MuxStream struct {
	frames Frames
	cfg    MuxConfig
	conn   MeteredConn

	mu   sync.Mutex
	in   [][]byte
	eof  bool
	wake chan struct{}
	room chan struct{}

	// Both sides have half-closed once both are set.
	closeSent, eofRead atomic.Bool
	done               chan struct{}
	once               sync.Once
}

var _ Stream = (*MuxStream)(nil)

func NewMuxStream(frames Frames, cfg MuxConfig) *MuxStream {
	s := &MuxStream{
		frames: frames,
		cfg:    cfg,
		wake:   make(chan struct{}, 1),
		room:   make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
	s.conn = NewMeteredConn(NewStreamConn(s, cfg.Local, cfg.Remote))
	return s
}

// Conn is the net.Conn over the stream.
func (s *MuxStream) Conn() MeteredConn { return s.conn }

// Deliver queues payload from the peer, waiting while a bounded queue is
// full, until ctx ends.
func (s *MuxStream) Deliver(ctx context.Context, payload []byte) {
	if len(payload) == 0 {
		return
	}
	for {
		s.mu.Lock()
		if s.eof || s.isDone() {
			s.mu.Unlock()
			return
		}
		if s.cfg.Queue == 0 || len(s.in) < s.cfg.Queue {
			s.in = append(s.in, payload)
			s.mu.Unlock()
			signal(s.wake)
			return
		}
		s.mu.Unlock()
		select {
		case <-s.room:
		case <-s.done:
			return
		case <-ctx.Done():
			return
		}
	}
}

// PeerClosed: the peer has nothing more to send. Recv returns io.EOF once it
// has returned the rest.
func (s *MuxStream) PeerClosed() {
	s.mu.Lock()
	s.eof = true
	s.mu.Unlock()
	signal(s.wake)
}

// PeerReset ends the conn: the peer abandoned it, so what it sent unread is
// dropped.
func (s *MuxStream) PeerReset() { s.end() }

func (s *MuxStream) Recv() ([]byte, error) {
	for {
		if s.isDone() {
			return nil, net.ErrClosed
		}
		s.mu.Lock()
		if len(s.in) > 0 {
			payload := s.in[0]
			s.in[0] = nil
			s.in = s.in[1:]
			s.mu.Unlock()
			signal(s.room)
			return payload, nil
		}
		eof := s.eof
		s.mu.Unlock()
		if eof {
			s.eofRead.Store(true)
			if s.closeSent.Load() {
				s.end()
			}
			return nil, io.EOF
		}
		select {
		case <-s.wake:
		case <-s.done:
		}
	}
}

func (s *MuxStream) Send(p []byte) error {
	for len(p) > 0 {
		if s.isDone() {
			return net.ErrClosed
		}
		size := len(p)
		if s.cfg.MaxFrame > 0 {
			size = min(size, s.cfg.MaxFrame)
		}
		if err := s.frames.Data(bytes.Clone(p[:size])); err != nil {
			return err
		}
		p = p[size:]
	}
	return nil
}

func (s *MuxStream) CloseSend() error {
	if s.isDone() || !s.closeSent.CompareAndSwap(false, true) {
		return nil
	}
	err := s.frames.CloseWrite()
	if errors.Is(err, errors.ErrUnsupported) || s.eofRead.Load() {
		s.end()
		return nil
	}
	return err
}

// Close resets the conn.
func (s *MuxStream) Close() error {
	if s.isDone() {
		return nil
	}
	err := s.frames.Reset("connection closed")
	s.end()
	return err
}

// Done is closed once the conn is over.
func (s *MuxStream) Done() <-chan struct{} { return s.done }

func (s *MuxStream) end() {
	s.once.Do(func() {
		close(s.done)
		s.frames.Release()
	})
}

func (s *MuxStream) isDone() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
