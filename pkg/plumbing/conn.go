package plumbing

import (
	"net"
	"sync"
	"time"
)

// Stream carries one conn's bytes each way: a per-conn gRPC stream, or one
// conn's frames on a multiplexed one.
type Stream interface {
	// Send sends p to the peer. It doesn't keep p after returning.
	Send(p []byte) error
	// Recv returns the peer's next bytes, or io.EOF once the peer has
	// nothing more to send.
	Recv() ([]byte, error)
	// CloseSend tells the peer this side has nothing more to send.
	CloseSend() error
	// Close abandons the stream in both directions.
	Close() error
}

// NewStreamConn turns s into a net.Conn, whose CloseWrite is s.CloseSend.
// Deadlines aren't supported; Close unblocks a pending Read or Write.
func NewStreamConn(s Stream, local, remote net.Addr) net.Conn {
	c := &streamConn{stream: s, local: local, remote: remote, closed: make(chan struct{})}
	c.done = c.closed
	if d, ok := s.(interface{ Done() <-chan struct{} }); ok {
		c.done = d.Done()
	}
	return c
}

type streamConn struct {
	stream        Stream
	local, remote net.Addr
	// The rest of the last Recv; only Read touches it.
	pending   []byte
	closed    chan struct{}
	closeOnce sync.Once
	// The stream's, if it has one, else closed.
	done <-chan struct{}
}

func (c *streamConn) Read(p []byte) (int, error) {
	for len(c.pending) == 0 {
		chunk, err := c.stream.Recv()
		if err != nil {
			return 0, err
		}
		c.pending = chunk
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *streamConn) Write(p []byte) (int, error) {
	if err := c.stream.Send(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *streamConn) CloseWrite() error { return c.stream.CloseSend() }

func (c *streamConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.stream.Close()
}

// Done is closed once the conn is over.
func (c *streamConn) Done() <-chan struct{} { return c.done }

func (c *streamConn) LocalAddr() net.Addr              { return c.local }
func (c *streamConn) RemoteAddr() net.Addr             { return c.remote }
func (c *streamConn) SetDeadline(time.Time) error      { return nil }
func (c *streamConn) SetReadDeadline(time.Time) error  { return nil }
func (c *streamConn) SetWriteDeadline(time.Time) error { return nil }
