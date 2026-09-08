package ioutil

import (
	"bytes"
	"errors"
	"io"
	"sync"
)

var (
	ErrClosed     = errors.New("ioutil: closed")
	ErrNotSet     = errors.New("ioutil: underlying ReadWriteCloser not set")
	ErrAlreadySet = errors.New("ioutil: underlying ReadWriteCloser already set")
)

// BufferedReadWriteCloser buffers writes until Set supplies the underlying
// io.ReadWriteCloser, then flushes them in order and passes everything
// through. The zero value is ready to use.
type BufferedReadWriteCloser struct {
	// mu guards the handoff in Set: a Write that arrives while Set is flushing
	// the buffer must land after the flush, on the underlying rwc.
	mu     sync.Mutex
	rwc    io.ReadWriteCloser
	buf    bytes.Buffer
	closed bool
}

var _ io.ReadWriteCloser = (*BufferedReadWriteCloser)(nil)

// Set attaches rwc and flushes the buffered writes to it. It closes rwc and
// returns ErrClosed if Close was already called, or the write error if the
// flush fails.
func (b *BufferedReadWriteCloser) Set(rwc io.ReadWriteCloser) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		rwc.Close()
		return ErrClosed
	}
	if b.rwc != nil {
		return ErrAlreadySet
	}
	if b.buf.Len() > 0 {
		if _, err := rwc.Write(b.buf.Bytes()); err != nil {
			rwc.Close()
			b.closed = true
			b.buf.Reset()
			return err
		}
		b.buf.Reset()
	}
	b.rwc = rwc
	return nil
}

func (b *BufferedReadWriteCloser) Write(p []byte) (int, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return 0, ErrClosed
	}
	if b.rwc == nil {
		n, err := b.buf.Write(p)
		b.mu.Unlock()
		return n, err
	}
	rwc := b.rwc
	b.mu.Unlock()
	return rwc.Write(p)
}

func (b *BufferedReadWriteCloser) Read(p []byte) (int, error) {
	b.mu.Lock()
	rwc, closed := b.rwc, b.closed
	b.mu.Unlock()
	if closed {
		return 0, ErrClosed
	}
	if rwc == nil {
		return 0, ErrNotSet
	}
	return rwc.Read(p)
}

func (b *BufferedReadWriteCloser) Close() error {
	b.mu.Lock()
	rwc := b.rwc
	b.closed = true
	b.buf.Reset()
	b.mu.Unlock()
	if rwc != nil {
		return rwc.Close()
	}
	return nil
}
