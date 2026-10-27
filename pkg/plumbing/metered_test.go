package plumbing

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingConn keeps what's written to it and reads from r.
type recordingConn struct {
	net.Conn
	r  io.Reader
	mu sync.Mutex
	w  bytes.Buffer
}

func (c *recordingConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *recordingConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.w.Write(p)
}

func (c *recordingConn) written() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.w.String()
}

func (c *recordingConn) Close() error { return nil }

func newRecordingConn(r string) *recordingConn {
	return &recordingConn{r: bytes.NewReader([]byte(r))}
}

func TestMeteredConnCounts(t *testing.T) {
	inner := newRecordingConn("hello")
	m := NewMeteredConn(inner)

	_, err := m.Write([]byte("abc"))
	require.NoError(t, err)
	_, err = m.Read(make([]byte, 16))
	require.NoError(t, err)

	assert.Equal(t, "abc", inner.written())
	st := m.Stats()
	assert.Equal(t, int64(5), st.BytesRead)
	assert.Equal(t, int64(3), st.BytesWritten)
	assert.Greater(t, st.WriteTime, time.Duration(0))
}

func TestMeteredConnCloseWrite(t *testing.T) {
	assert.True(t, errors.Is(NewMeteredConn(newRecordingConn("")).CloseWrite(), errors.ErrUnsupported))

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer lis.Close()
	go func() {
		nc, err := lis.Accept()
		if err == nil {
			_, _ = io.Copy(io.Discard, nc)
			_ = nc.Close()
		}
	}()
	nc, err := net.Dial("tcp", lis.Addr().String())
	require.NoError(t, err)
	defer nc.Close()
	m := NewMeteredConn(nc)
	require.NoError(t, m.CloseWrite())
	// The peer saw EOF and closed its side.
	_, err = nc.Read(make([]byte, 1))
	assert.ErrorIs(t, err, io.EOF)
}
