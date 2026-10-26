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
	r    io.Reader
	mu   sync.Mutex
	w    bytes.Buffer
	done chan struct{}
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

func (c *recordingConn) Done() <-chan struct{} { return c.done }

func newRecordingConn(r string) *recordingConn {
	return &recordingConn{r: bytes.NewReader([]byte(r)), done: make(chan struct{})}
}

// writeAsync starts a Write and returns its result.
func writeAsync(m *MeteredConn, p string) <-chan error {
	errc := make(chan error, 1)
	go func() {
		_, err := m.Write([]byte(p))
		errc <- err
	}()
	return errc
}

func TestMeteredConnCountsWithoutAWindow(t *testing.T) {
	inner := newRecordingConn("hello")
	var reads []int
	m := NewMeteredConn(inner, MeteredConfig{OnRead: func(n int) { reads = append(reads, n) }})

	_, err := m.Write([]byte("abc"))
	require.NoError(t, err)
	buf := make([]byte, 16)
	n, err := m.Read(buf)
	require.NoError(t, err)

	assert.Equal(t, "abc", inner.written())
	assert.Equal(t, []int{n}, reads)
	assert.Equal(t, MeteredStats{BytesRead: 5, BytesWritten: 3}, m.Stats())
}

func TestMeteredConnHoldsWritesToTheWindow(t *testing.T) {
	inner := newRecordingConn("")
	m := NewMeteredConn(inner, MeteredConfig{Window: 4})

	errc := writeAsync(m, "abcdefgh")
	require.Eventually(t, func() bool { return inner.written() == "abcd" }, time.Second, time.Millisecond)
	select {
	case <-errc:
		t.Fatal("wrote past the window")
	case <-time.After(50 * time.Millisecond):
	}

	m.Grant(4)
	require.NoError(t, <-errc)
	assert.Equal(t, "abcdefgh", inner.written())
	st := m.Stats()
	assert.Equal(t, int64(0), st.Credit)
	assert.Equal(t, int64(1), st.WriteWaits)
	assert.Greater(t, st.WriteWait, time.Duration(0))
}

func TestMeteredConnWriteWaitingEndsWithTheConn(t *testing.T) {
	m := NewMeteredConn(newRecordingConn(""), MeteredConfig{Window: 1})
	errc := writeAsync(m, "ab")
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, m.Close())
	assert.ErrorIs(t, <-errc, net.ErrClosed)

	inner := newRecordingConn("")
	m = NewMeteredConn(inner, MeteredConfig{Window: 1})
	errc = writeAsync(m, "ab")
	time.Sleep(10 * time.Millisecond)
	close(inner.done)
	assert.ErrorIs(t, <-errc, net.ErrClosed)
}

func TestMeteredConnCloseWrite(t *testing.T) {
	assert.True(t, errors.Is(NewMeteredConn(newRecordingConn(""), MeteredConfig{}).CloseWrite(), errors.ErrUnsupported))

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
	m := NewMeteredConn(nc, MeteredConfig{})
	require.NoError(t, m.CloseWrite())
	// The peer saw EOF and closed its side.
	_, err = nc.Read(make([]byte, 1))
	assert.ErrorIs(t, err, io.EOF)
}
