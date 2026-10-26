package plumbing

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// MeteredConn wraps a net.Conn and counts what passes through it. Given a
// window, Write sends no more than the peer has granted with Grant, and Read
// reports what it returns to OnRead so the caller can grant it back: the flow
// control a conn multiplexed with others needs, since the transport's own
// can only slow all of them.
type MeteredConn struct {
	net.Conn
	cfg MeteredConfig

	credit    atomic.Int64
	wake      chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
	// The wrapped conn's, if it has a Done method, so a write waiting for
	// credit returns once that conn is over.
	innerDone <-chan struct{}

	bytesRead, bytesWritten atomic.Int64
	writeWait, writeWaits   atomic.Int64
}

// MeteredConfig configures a MeteredConn.
type MeteredConfig struct {
	// Window is what Write may send before Grant gives more; 0 for no limit.
	Window int
	// OnRead is called with what each Read returned.
	OnRead func(n int)
}

// MeteredStats is what has passed through a MeteredConn.
type MeteredStats struct {
	BytesRead    int64
	BytesWritten int64
	// What Write may still send. Zero without a window.
	Credit int64
	// How long writes waited for credit, over how many waits.
	WriteWait  time.Duration
	WriteWaits int64
}

var _ net.Conn = (*MeteredConn)(nil)

func NewMeteredConn(conn net.Conn, cfg MeteredConfig) *MeteredConn {
	m := &MeteredConn{
		Conn:   conn,
		cfg:    cfg,
		wake:   make(chan struct{}, 1),
		closed: make(chan struct{}),
	}
	m.credit.Store(int64(cfg.Window))
	if d, ok := conn.(interface{ Done() <-chan struct{} }); ok {
		m.innerDone = d.Done()
	}
	return m
}

func (m *MeteredConn) Read(p []byte) (int, error) {
	n, err := m.Conn.Read(p)
	if n > 0 {
		m.bytesRead.Add(int64(n))
		if m.cfg.OnRead != nil {
			m.cfg.OnRead(n)
		}
	}
	return n, err
}

func (m *MeteredConn) Write(p []byte) (int, error) {
	if m.cfg.Window <= 0 {
		n, err := m.Conn.Write(p)
		m.bytesWritten.Add(int64(n))
		return n, err
	}
	written := 0
	for written < len(p) {
		credit, err := m.awaitCredit()
		if err != nil {
			return written, err
		}
		size := min(len(p)-written, int(credit))
		n, err := m.Conn.Write(p[written : written+size])
		m.credit.Add(-int64(n))
		m.bytesWritten.Add(int64(n))
		written += n
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

// Grant lets Write send n more bytes.
func (m *MeteredConn) Grant(n int) {
	m.credit.Add(int64(n))
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// CloseWrite half-closes the wrapped conn, if it can.
func (m *MeteredConn) CloseWrite() error {
	if cw, ok := m.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.ErrUnsupported
}

func (m *MeteredConn) Close() error {
	m.closeOnce.Do(func() { close(m.closed) })
	return m.Conn.Close()
}

func (m *MeteredConn) Stats() MeteredStats {
	st := MeteredStats{
		BytesRead:    m.bytesRead.Load(),
		BytesWritten: m.bytesWritten.Load(),
		WriteWait:    time.Duration(m.writeWait.Load()),
		WriteWaits:   m.writeWaits.Load(),
	}
	if m.cfg.Window > 0 {
		st.Credit = m.credit.Load()
	}
	return st
}

func (m *MeteredConn) awaitCredit() (int64, error) {
	var start time.Time
	defer func() {
		if !start.IsZero() {
			m.writeWait.Add(int64(time.Since(start)))
		}
	}()
	for {
		if credit := m.credit.Load(); credit > 0 {
			return credit, nil
		}
		if start.IsZero() {
			start = time.Now()
			m.writeWaits.Add(1)
		}
		select {
		case <-m.wake:
		case <-m.closed:
			return 0, net.ErrClosed
		case <-m.innerDone:
			return 0, net.ErrClosed
		}
	}
}
