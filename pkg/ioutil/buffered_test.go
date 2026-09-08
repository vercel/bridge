package ioutil

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

type recordingRWC struct {
	written  bytes.Buffer
	closed   bool
	writeErr error
}

func (r *recordingRWC) Write(p []byte) (int, error) {
	if r.writeErr != nil {
		return 0, r.writeErr
	}
	return r.written.Write(p)
}

func (r *recordingRWC) Read(p []byte) (int, error) { return copy(p, "read"), nil }

func (r *recordingRWC) Close() error {
	r.closed = true
	return nil
}

func TestWritesBeforeSetAreFlushedInOrder(t *testing.T) {
	b := NewBufferedReadWriteCloser()
	rwc := &recordingRWC{}

	for _, chunk := range []string{"one ", "two ", "three"} {
		if _, err := b.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write before Set: %v", err)
		}
	}
	if _, err := b.Read(make([]byte, 4)); !errors.Is(err, ErrNotSet) {
		t.Fatalf("Read before Set: got %v, want ErrNotSet", err)
	}

	if err := b.Set(rwc); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := rwc.written.String(); got != "one two three" {
		t.Fatalf("flushed %q, want %q", got, "one two three")
	}

	if _, err := b.Write([]byte(" four")); err != nil {
		t.Fatalf("Write after Set: %v", err)
	}
	if got := rwc.written.String(); got != "one two three four" {
		t.Fatalf("after write-through got %q", got)
	}

	p := make([]byte, 4)
	if n, err := b.Read(p); err != nil || string(p[:n]) != "read" {
		t.Fatalf("Read after Set: %q, %v", p[:n], err)
	}

	if err := b.Set(&recordingRWC{}); !errors.Is(err, ErrAlreadySet) {
		t.Fatalf("second Set: got %v, want ErrAlreadySet", err)
	}
}

func TestCloseBeforeSetClosesTheLateArrival(t *testing.T) {
	b := NewBufferedReadWriteCloser()
	_, _ = b.Write([]byte("queued"))
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rwc := &recordingRWC{}
	if err := b.Set(rwc); !errors.Is(err, ErrClosed) {
		t.Fatalf("Set after Close: got %v, want ErrClosed", err)
	}
	if !rwc.closed {
		t.Fatal("expected Set to close the ReadWriteCloser handed to a closed buffer")
	}
	if rwc.written.Len() != 0 {
		t.Fatalf("expected nothing flushed to a closed buffer's late arrival, got %q", rwc.written.String())
	}
	if _, err := b.Write([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write after Close: got %v, want ErrClosed", err)
	}
}

func TestFlushFailureClosesTheUnderlying(t *testing.T) {
	b := NewBufferedReadWriteCloser()
	_, _ = b.Write([]byte("queued"))

	rwc := &recordingRWC{writeErr: io.ErrShortWrite}
	if err := b.Set(rwc); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Set with failing flush: got %v, want ErrShortWrite", err)
	}
	if !rwc.closed {
		t.Fatal("expected the underlying to be closed after a failed flush")
	}
	if _, err := b.Write([]byte("x")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write after failed flush: got %v, want ErrClosed", err)
	}
}

func TestCloseAfterSetClosesTheUnderlying(t *testing.T) {
	b := NewBufferedReadWriteCloser()
	rwc := &recordingRWC{}
	if err := b.Set(rwc); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !rwc.closed {
		t.Fatal("expected Close to close the underlying")
	}
}
