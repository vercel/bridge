package devtunnel

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"

	devtunnelv1 "github.com/vercel/bridge/api/go/vercel/api/dev_tunnel/v1"
)

type direction int

const (
	// From the deployment to the app.
	ingress direction = iota
	// From the app out through the deployment.
	egress
)

func (d direction) String() string {
	if d == egress {
		return "egress"
	}
	return "ingress"
}

// conn joins one of the stream's conns to a local TCP connection. Each
// direction moves only as fast as its reader takes it: what the local side
// sends waits on the relay's window, and the relay gets window back as what it
// sent is written locally. Either side may half-close; the conn is over once
// both have, or either resets.
type conn struct {
	id     uint64
	s      *stream
	dir    direction
	logger *slog.Logger

	mu   sync.Mutex
	cond *sync.Cond
	nc   net.Conn
	// What this side may still send the relay.
	sendWindow int
	// From the relay, not yet written locally; a nil entry half-closes.
	pending                [][]byte
	closeSent, closeQueued bool
	writeClosed, over      bool
}

// attach starts moving bytes once the local connection is up.
func (c *conn) attach(nc net.Conn) {
	c.mu.Lock()
	if c.over {
		c.mu.Unlock()
		_ = nc.Close()
		return
	}
	c.nc = nc
	c.mu.Unlock()
	go c.readLocal()
	go c.writeLocal()
}

// readLocal sends what the local side sends, as the window allows.
func (c *conn) readLocal() {
	buf := make([]byte, maxPayload)
	for {
		c.mu.Lock()
		for c.sendWindow == 0 && !c.over {
			c.cond.Wait()
		}
		if c.over {
			c.mu.Unlock()
			return
		}
		size := min(c.sendWindow, maxPayload)
		c.mu.Unlock()

		n, err := c.nc.Read(buf[:size])
		if n > 0 {
			c.mu.Lock()
			if c.over {
				c.mu.Unlock()
				return
			}
			c.sendWindow -= n
			c.mu.Unlock()
			c.s.c.bytesSent.Add(int64(n))
			c.s.send(&devtunnelv1.ConnectRequest{Frame: &devtunnelv1.ConnectRequest_Data{Data: &devtunnelv1.Data{
				ConnId:  c.id,
				Payload: bytes.Clone(buf[:n]),
			}}})
		}
		if errors.Is(err, io.EOF) {
			c.sendClose()
			return
		}
		if err != nil {
			c.reset(err.Error())
			return
		}
	}
}

// writeLocal writes what the relay sent, granting it back as it goes.
func (c *conn) writeLocal() {
	for {
		c.mu.Lock()
		for len(c.pending) == 0 && !c.over {
			c.cond.Wait()
		}
		if c.over {
			c.mu.Unlock()
			return
		}
		payload := c.pending[0]
		c.pending[0] = nil
		c.pending = c.pending[1:]
		c.mu.Unlock()

		if payload == nil {
			if cw, ok := c.nc.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			}
			c.mu.Lock()
			c.writeClosed = true
			done := c.closeSent
			c.mu.Unlock()
			if done {
				c.end()
			}
			return
		}
		if _, err := c.nc.Write(payload); err != nil {
			c.reset(err.Error())
			return
		}
		c.mu.Lock()
		over := c.over
		c.mu.Unlock()
		if !over {
			c.s.send(&devtunnelv1.ConnectRequest{Frame: &devtunnelv1.ConnectRequest_Window{Window: &devtunnelv1.Window{
				ConnId: c.id,
				Bytes:  uint32(len(payload)),
			}}})
		}
	}
}

// received queues payload from the relay.
func (c *conn) received(payload []byte) {
	if len(payload) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.over || c.closeQueued {
		return
	}
	c.pending = append(c.pending, payload)
	c.cond.Broadcast()
}

// closeReceived: the relay's side has nothing more to send.
func (c *conn) closeReceived() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.over || c.closeQueued {
		return
	}
	c.closeQueued = true
	c.pending = append(c.pending, nil)
	c.cond.Broadcast()
}

// grant: the relay may take this many more bytes.
func (c *conn) grant(bytes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sendWindow += bytes
	c.cond.Broadcast()
}

// sendClose: the local side has nothing more to send.
func (c *conn) sendClose() {
	c.mu.Lock()
	if c.over || c.closeSent {
		c.mu.Unlock()
		return
	}
	c.closeSent = true
	done := c.writeClosed
	c.mu.Unlock()
	c.s.send(&devtunnelv1.ConnectRequest{Frame: &devtunnelv1.ConnectRequest_Close{Close: &devtunnelv1.Close{ConnId: c.id}}})
	if done {
		c.end()
	}
}

// reset abandons the conn and tells the relay why.
func (c *conn) reset(reason string) {
	c.mu.Lock()
	over := c.over
	c.mu.Unlock()
	if over {
		return
	}
	c.s.send(resetFrame(c.id, reason))
	c.abandon()
}

// abandon drops the conn in both directions without telling the relay, which
// already knows.
func (c *conn) abandon() {
	c.end()
}

func (c *conn) end() {
	c.mu.Lock()
	if c.over {
		c.mu.Unlock()
		return
	}
	c.over = true
	nc := c.nc
	c.cond.Broadcast()
	c.mu.Unlock()
	if nc != nil {
		_ = nc.Close()
	}
	c.s.forget(c)
}
