package portforward

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/httpstream"
)

type fakeConn struct {
	closeCh chan bool
}

func (c *fakeConn) CreateStream(http.Header) (httpstream.Stream, error) { return nil, nil }
func (c *fakeConn) Close() error                                        { return nil }
func (c *fakeConn) CloseChan() <-chan bool                              { return c.closeCh }
func (c *fakeConn) SetIdleTimeout(time.Duration)                        {}
func (c *fakeConn) RemoveStreams(...httpstream.Stream)                  {}

func TestDialerClosedFollowsTheSPDYConnection(t *testing.T) {
	conn := &fakeConn{closeCh: make(chan bool)}
	d := &Dialer{conn: conn}

	assert.False(t, d.Closed())

	close(conn.closeCh)
	assert.True(t, d.Closed())
}
