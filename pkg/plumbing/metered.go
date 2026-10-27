package plumbing

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// MeteredConn is a net.Conn that counts what passes through it. Given a
// window, Write sends no more than the peer has granted with Grant: the flow
// control a conn multiplexed with others needs, since the transport's own can
// only slow all of them.
type MeteredConn interface {
	net.Conn
	// Grant lets Write send n more bytes.
	Grant(n int)
	// CloseWrite half-closes the wrapped conn, if it can.
	CloseWrite() error
	// Stats reports what has passed through so far.
	Stats() *MeteredStats
}

type meteredConn struct {
	net.Conn
	window int

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

// NewMeteredConn wraps conn. Write may send window bytes before Grant gives
// more; 0 for no limit.
func NewMeteredConn(conn net.Conn, window int) MeteredConn {
	m := &meteredConn{
		Conn:   conn,
		window: window,
		wake:   make(chan struct{}, 1),
		closed: make(chan struct{}),
	}
	m.credit.Store(int64(window))
	if d, ok := conn.(interface{ Done() <-chan struct{} }); ok {
		m.innerDone = d.Done()
	}
	return m
}

func (m *meteredConn) Read(p []byte) (int, error) {
	n, err := m.Conn.Read(p)
	m.bytesRead.Add(int64(n))
	return n, err
}

func (m *meteredConn) Write(p []byte) (int, error) {
	if m.window <= 0 {
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

func (m *meteredConn) Grant(n int) {
	m.credit.Add(int64(n))
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *meteredConn) CloseWrite() error {
	if cw, ok := m.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.ErrUnsupported
}

func (m *meteredConn) Close() error {
	m.closeOnce.Do(func() { close(m.closed) })
	return m.Conn.Close()
}

func (m *meteredConn) Stats() *MeteredStats {
	st := &MeteredStats{
		BytesRead:    m.bytesRead.Load(),
		BytesWritten: m.bytesWritten.Load(),
		WriteWait:    time.Duration(m.writeWait.Load()),
		WriteWaits:   m.writeWaits.Load(),
	}
	if m.window > 0 {
		st.Credit = m.credit.Load()
	}
	return st
}

func (m *meteredConn) awaitCredit() (int64, error) {
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
