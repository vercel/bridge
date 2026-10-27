package plumbing

import (
	"errors"
	"net"
	"sync/atomic"
	"time"
)

// MeteredConn is a net.Conn that counts what passes through it.
type MeteredConn interface {
	net.Conn
	// CloseWrite half-closes the wrapped conn, if it can.
	CloseWrite() error
	// Stats reports what has passed through so far.
	Stats() *MeteredStats
}

// MeteredStats is what has passed through a MeteredConn.
type MeteredStats struct {
	BytesRead    int64
	BytesWritten int64
	// How long writes took, which grows when the transport holds them back.
	WriteTime time.Duration
}

type meteredConn struct {
	net.Conn
	bytesRead, bytesWritten, writeTime atomic.Int64
}

func NewMeteredConn(conn net.Conn) MeteredConn {
	return &meteredConn{Conn: conn}
}

func (m *meteredConn) Read(p []byte) (int, error) {
	n, err := m.Conn.Read(p)
	m.bytesRead.Add(int64(n))
	return n, err
}

func (m *meteredConn) Write(p []byte) (int, error) {
	start := time.Now()
	n, err := m.Conn.Write(p)
	m.writeTime.Add(int64(time.Since(start)))
	m.bytesWritten.Add(int64(n))
	return n, err
}

func (m *meteredConn) CloseWrite() error {
	if cw, ok := m.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.ErrUnsupported
}

func (m *meteredConn) Stats() *MeteredStats {
	return &MeteredStats{
		BytesRead:    m.bytesRead.Load(),
		BytesWritten: m.bytesWritten.Load(),
		WriteTime:    time.Duration(m.writeTime.Load()),
	}
}
