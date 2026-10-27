package plumbing

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingFrames keeps the frames a MuxStream writes.
type recordingFrames struct {
	mu        sync.Mutex
	data      []string
	closes    int
	resets    []string
	releases  int
	halfClose bool
}

func (f *recordingFrames) Data(p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data = append(f.data, string(p))
	return nil
}

func (f *recordingFrames) CloseWrite() error {
	if !f.halfClose {
		return errors.ErrUnsupported
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

func (f *recordingFrames) Reset(reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resets = append(f.resets, reason)
	return nil
}

func (f *recordingFrames) Release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases++
}

func TestMuxStreamReceivesInOrder(t *testing.T) {
	f := &recordingFrames{halfClose: true}
	s := NewMuxStream(f, MuxConfig{})
	ctx := context.Background()
	s.Deliver(ctx, []byte("ab"))
	s.Deliver(ctx, []byte("cde"))
	s.PeerClosed()

	got, err := io.ReadAll(s.Conn())
	require.NoError(t, err)
	assert.Equal(t, "abcde", string(got))
}

func TestMuxStreamBoundedQueueWaitsForRecv(t *testing.T) {
	s := NewMuxStream(&recordingFrames{}, MuxConfig{Queue: 1})
	ctx := context.Background()
	s.Deliver(ctx, []byte("a"))

	delivered := make(chan struct{})
	go func() {
		s.Deliver(ctx, []byte("b"))
		close(delivered)
	}()
	select {
	case <-delivered:
		t.Fatal("queued past the bound")
	case <-time.After(50 * time.Millisecond):
	}
	_, err := s.Recv()
	require.NoError(t, err)
	select {
	case <-delivered:
	case <-time.After(time.Second):
		t.Fatal("Recv made no room")
	}

	// A cancelled ctx gives up a wait.
	waiting, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		s.Deliver(waiting, []byte("c"))
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Deliver kept waiting after ctx ended")
	}
}

func TestMuxStreamEndsOnceBothSidesHalfClose(t *testing.T) {
	f := &recordingFrames{halfClose: true}
	s := NewMuxStream(f, MuxConfig{})
	require.NoError(t, s.CloseSend())
	assert.Equal(t, 1, f.closes)
	assert.Equal(t, 0, f.releases)

	s.PeerClosed()
	_, err := s.Recv()
	assert.ErrorIs(t, err, io.EOF)
	assert.Equal(t, 1, f.releases)
	require.NoError(t, s.Close())
	assert.Empty(t, f.resets, "a conn both sides closed isn't reset")
}

func TestMuxStreamWithoutHalfCloseEndsAtCloseSend(t *testing.T) {
	f := &recordingFrames{}
	s := NewMuxStream(f, MuxConfig{})
	require.NoError(t, s.CloseSend())
	assert.Equal(t, 1, f.releases)
	_, err := s.Recv()
	assert.ErrorIs(t, err, net.ErrClosed)
}

func TestMuxStreamSendsInFramesAndResets(t *testing.T) {
	f := &recordingFrames{halfClose: true}
	s := NewMuxStream(f, MuxConfig{MaxFrame: 2})
	require.NoError(t, s.Send([]byte("abcde")))
	assert.Equal(t, []string{"ab", "cd", "e"}, f.data)

	require.NoError(t, s.Close())
	assert.Equal(t, []string{"connection closed"}, f.resets)
	assert.ErrorIs(t, s.Send([]byte("x")), net.ErrClosed)
	require.NoError(t, s.Close())
	assert.Equal(t, 1, f.releases)
}

func TestMuxStreamPeerResetDropsWhatWasUnread(t *testing.T) {
	f := &recordingFrames{halfClose: true}
	s := NewMuxStream(f, MuxConfig{})
	s.Deliver(context.Background(), []byte("unread"))
	s.PeerReset()

	_, err := s.Recv()
	assert.ErrorIs(t, err, net.ErrClosed)
	assert.Empty(t, f.resets, "the peer already knows")
	assert.Equal(t, 1, f.releases)
}
