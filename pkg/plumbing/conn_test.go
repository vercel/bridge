package plumbing

import (
	"io"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chunkStream replays chunks, then EOF, and records what's done to it.
type chunkStream struct {
	chunks     [][]byte
	sent       []string
	sendClosed bool
	closed     bool
	done       chan struct{}
}

func (s *chunkStream) Send(p []byte) error {
	s.sent = append(s.sent, string(p))
	return nil
}

func (s *chunkStream) Recv() ([]byte, error) {
	if len(s.chunks) == 0 {
		return nil, io.EOF
	}
	chunk := s.chunks[0]
	s.chunks = s.chunks[1:]
	return chunk, nil
}

func (s *chunkStream) CloseSend() error {
	s.sendClosed = true
	return nil
}

func (s *chunkStream) Close() error {
	s.closed = true
	return nil
}

func (s *chunkStream) Done() <-chan struct{} { return s.done }

func TestStreamConn(t *testing.T) {
	s := &chunkStream{chunks: [][]byte{[]byte("hello "), []byte("world")}, done: make(chan struct{})}
	conn := NewStreamConn(s, nil, nil)

	// A Read smaller than a chunk takes the rest of it next.
	buf := make([]byte, 4)
	n, err := conn.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, "hell", string(buf[:n]))
	rest, err := io.ReadAll(conn)
	require.NoError(t, err)
	assert.Equal(t, "o world", string(rest))

	_, err = conn.Write([]byte("hi"))
	require.NoError(t, err)
	assert.Equal(t, []string{"hi"}, s.sent)

	require.NoError(t, conn.(interface{ CloseWrite() error }).CloseWrite())
	assert.True(t, s.sendClosed)
	assert.Equal(t, (<-chan struct{})(s.done), conn.(interface{ Done() <-chan struct{} }).Done())
	require.NoError(t, conn.Close())
	assert.True(t, s.closed)

	var _ net.Conn = conn
}
