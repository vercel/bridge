// Package portforward provides Kubernetes port-forwarding using SPDY streams,
// exposing net.Conn connections without binding local ports.
package portforward

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// Dialer creates net.Conn connections to a Kubernetes pod port via SPDY
// port-forwarding, without binding a local port.
type Dialer struct {
	conn  httpstream.Connection
	port  int
	reqID atomic.Int64
}

// dialTimeout is how long we wait for the initial SPDY port-forward
// connection before giving up. This prevents hanging indefinitely when the
// Kubernetes API server or kubelet is unreachable.
const dialTimeout = 15 * time.Second

// NewDialer establishes an SPDY connection to the given pod and returns
// a Dialer that can create net.Conn streams to the specified port.
func NewDialer(restConfig *rest.Config, clientset kubernetes.Interface, namespace, podName string, port int) (*Dialer, error) {
	reqURL := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(namespace).
		Name(podName).
		SubResource("portforward").
		URL()

	transport, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create SPDY round tripper: %w", err)
	}

	spdyDialer := spdy.NewDialer(upgrader, &http.Client{
		Transport: transport,
		Timeout:   dialTimeout,
	}, http.MethodPost, reqURL)

	conn, protocol, err := spdyDialer.Dial(portforward.PortForwardProtocolV1Name)
	if err != nil {
		return nil, fmt.Errorf("SPDY dial: %w", err)
	}
	if protocol != portforward.PortForwardProtocolV1Name {
		conn.Close()
		return nil, fmt.Errorf("unexpected protocol %q, expected %q", protocol, portforward.PortForwardProtocolV1Name)
	}

	return &Dialer{
		conn: conn,
		port: port,
	}, nil
}

// DialContext creates a new SPDY stream pair (data + error) and returns a
// net.Conn wrapping the data stream. The addr parameter is ignored — the
// connection always targets the pod port configured in NewDialer.
// This signature is compatible with grpc.WithContextDialer.
func (d *Dialer) DialContext(ctx context.Context, _ string) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		conn, err := d.createStream()
		ch <- result{conn, err}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.conn, r.err
	}
}

// createStream performs the actual SPDY stream creation.
func (d *Dialer) createStream() (net.Conn, error) {
	requestID := strconv.FormatInt(d.reqID.Add(1), 10)
	portStr := strconv.Itoa(d.port)

	// Error stream must be created before data stream per the k8s protocol.
	headers := http.Header{}
	headers.Set(corev1.StreamType, corev1.StreamTypeError)
	headers.Set(corev1.PortHeader, portStr)
	headers.Set(corev1.PortForwardRequestIDHeader, requestID)

	errorStream, err := d.conn.CreateStream(headers)
	if err != nil {
		return nil, fmt.Errorf("create error stream: %w", err)
	}
	// We don't write to the error stream; close the write direction.
	errorStream.Close()

	headers.Set(corev1.StreamType, corev1.StreamTypeData)
	dataStream, err := d.conn.CreateStream(headers)
	if err != nil {
		return nil, fmt.Errorf("create data stream: %w", err)
	}

	sc := &spdyConn{
		dataStream:  dataStream,
		errorStream: errorStream,
		parentConn:  d.conn,
	}

	// Monitor the error stream asynchronously.
	go sc.monitorErrors()

	return sc, nil
}

// Close closes the underlying SPDY connection.
func (d *Dialer) Close() error {
	return d.conn.Close()
}

// spdyConn wraps an SPDY data stream as a net.Conn.
type spdyConn struct {
	dataStream  httpstream.Stream
	errorStream httpstream.Stream
	parentConn  httpstream.Connection

	mu        sync.Mutex
	closed    bool
	remoteErr error
}

func (c *spdyConn) monitorErrors() {
	message, err := io.ReadAll(c.errorStream)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.remoteErr = fmt.Errorf("error stream: %w", err)
	} else if len(message) > 0 {
		c.remoteErr = fmt.Errorf("port-forward error: %s", string(message))
	}
}

func (c *spdyConn) Read(b []byte) (int, error) {
	n, err := c.dataStream.Read(b)
	if err != nil {
		// Check if there is a more descriptive error from the error stream.
		c.mu.Lock()
		remoteErr := c.remoteErr
		c.mu.Unlock()
		if remoteErr != nil {
			return n, remoteErr
		}
	}
	return n, err
}

func (c *spdyConn) Write(b []byte) (int, error) {
	return c.dataStream.Write(b)
}

func (c *spdyConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	c.dataStream.Close()
	c.parentConn.RemoveStreams(c.dataStream, c.errorStream)
	return nil
}

func (c *spdyConn) LocalAddr() net.Addr                { return spdyAddr{} }
func (c *spdyConn) RemoteAddr() net.Addr               { return spdyAddr{} }
func (c *spdyConn) SetDeadline(_ time.Time) error      { return nil }
func (c *spdyConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *spdyConn) SetWriteDeadline(_ time.Time) error { return nil }

// spdyAddr implements net.Addr for SPDY stream connections.
type spdyAddr struct{}

func (spdyAddr) Network() string { return "spdy" }
func (spdyAddr) String() string  { return "spdy" }

var _ net.Conn = (*spdyConn)(nil)
