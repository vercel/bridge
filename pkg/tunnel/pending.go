package tunnel

import (
	"errors"
	"io"
	"net"
	"sync"
)

var errNotResolved = errors.New("tunnel: connection not yet dialed")

// pendingConn stands in for a tunneled connection whose dial has not completed.
// The recv pump registers it under the connection ID before dialing so that
// chunks arriving while the dial is in flight queue here, in order, instead of
// each spawning another dial for the same connection.
type pendingConn struct {
	mu     sync.Mutex
	conn   net.Conn
	queued [][]byte
	closed bool
}

var _ io.ReadWriteCloser = (*pendingConn)(nil)

// resolve attaches the dialed connection and flushes the queued chunks in
// order. It closes conn and reports the reason if the pending connection was
// already closed or a queued chunk fails to write.
func (p *pendingConn) resolve(conn net.Conn) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		conn.Close()
		return net.ErrClosed
	}
	for _, b := range p.queued {
		if _, err := conn.Write(b); err != nil {
			conn.Close()
			p.closed = true
			p.queued = nil
			return err
		}
	}
	p.queued = nil
	p.conn = conn
	return nil
}

func (p *pendingConn) Write(b []byte) (int, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return 0, net.ErrClosed
	}
	if p.conn == nil {
		p.queued = append(p.queued, append([]byte(nil), b...))
		p.mu.Unlock()
		return len(b), nil
	}
	conn := p.conn
	p.mu.Unlock()
	return conn.Write(b)
}

func (p *pendingConn) Read(b []byte) (int, error) {
	p.mu.Lock()
	conn, closed := p.conn, p.closed
	p.mu.Unlock()
	if closed {
		return 0, net.ErrClosed
	}
	if conn == nil {
		return 0, errNotResolved
	}
	return conn.Read(b)
}

func (p *pendingConn) Close() error {
	p.mu.Lock()
	conn := p.conn
	p.closed = true
	p.queued = nil
	p.mu.Unlock()
	if conn != nil {
		return conn.Close()
	}
	return nil
}
