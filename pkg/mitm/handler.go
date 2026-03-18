package mitm

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"

	"google.golang.org/protobuf/encoding/protojson"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
)

// Handler serves facade responses over a raw TCP connection for a set of
// compiled facades. It handles TLS termination, parses the HTTP request,
// evaluates routes, and writes the first matching action's response. If no
// route matches, it forwards the request to the original destination.
type Handler struct {
	CA       *CA
	Facades []*CompiledFacade
	DestAddr string // original destination ip:port for forwarding unmatched requests
}

// Serve handles a connection for the given hostname. It loops to support
// HTTP/1.1 keep-alive — multiple requests on the same TCP connection.
func (h *Handler) Serve(conn net.Conn, hostname string) error {
	isTLS, plainConn, err := h.terminateTLS(conn, hostname)
	if err != nil {
		return err
	}

	br := bufio.NewReader(plainConn)

	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			if err == io.EOF {
				return nil // client closed the connection
			}
			return fmt.Errorf("read HTTP request: %w", err)
		}

		if req.Host == "" {
			req.Host = hostname
		}

		if err := h.handleRequest(plainConn, req, hostname, isTLS); err != nil {
			return err
		}

		// If the client sent Connection: close, stop.
		if req.Close {
			return nil
		}
	}
}

// handleRequest evaluates facade routes for a single request. If a route
// matches, it writes the action response. Otherwise it forwards to the
// original destination.
func (h *Handler) handleRequest(conn net.Conn, req *http.Request, hostname string, isTLS bool) error {
	defer req.Body.Close()
	for _, facade := range h.Facades {
		for _, route := range facade.Routes {
			matched, err := route.Match(req)
			if err != nil {
				slog.Warn("CEL evaluation error", "host", hostname, "cel", route.CEL, "error", err)
				continue
			}
			if matched {
				slog.Debug("ServerFacade route matched", "host", hostname, "path", req.URL.Path, "cel", route.CEL)
				return writeAction(conn, route.Action)
			}
		}
	}

	slog.Debug("No facade route matched, forwarding", "host", hostname, "method", req.Method, "path", req.URL.Path, "dest", h.DestAddr)
	return h.forward(conn, req, hostname, isTLS)
}

// forward dials the real destination, sends the HTTP request, and pipes the
// response back to the client conn.
func (h *Handler) forward(clientConn net.Conn, req *http.Request, hostname string, isTLS bool) error {
	upstream, err := net.Dial("tcp", h.DestAddr)
	if err != nil {
		return fmt.Errorf("dial upstream %s: %w", h.DestAddr, err)
	}
	defer upstream.Close()

	// If the original connection was TLS, establish TLS to the real server.
	var upstreamWriter io.ReadWriter = upstream
	if isTLS {
		tlsConn := tls.Client(upstream, &tls.Config{
			ServerName: hostname,
		})
		defer tlsConn.Close()
		if err := tlsConn.Handshake(); err != nil {
			return fmt.Errorf("upstream TLS handshake: %w", err)
		}
		upstreamWriter = tlsConn
	}

	// Write the request to upstream.
	if err := req.Write(upstreamWriter); err != nil {
		return fmt.Errorf("write request to upstream: %w", err)
	}

	// Read the response from upstream and write it back to the client.
	resp, err := http.ReadResponse(bufio.NewReader(upstreamWriter), req)
	if err != nil {
		return fmt.Errorf("read upstream response: %w", err)
	}
	defer resp.Body.Close()

	if err := resp.Write(clientConn); err != nil {
		return fmt.Errorf("write response to client: %w", err)
	}

	return nil
}

// terminateTLS peeks at the connection and terminates TLS if a ClientHello is
// detected. Returns whether TLS was terminated and the plaintext conn.
func (h *Handler) terminateTLS(conn net.Conn, hostname string) (isTLS bool, plain net.Conn, err error) {
	br := bufio.NewReader(conn)

	first, err := br.Peek(1)
	if err != nil {
		return false, nil, fmt.Errorf("peek: %w", err)
	}

	plainConn := net.Conn(newBufferedConn(conn, br))

	if first[0] == 0x16 && h.CA != nil {
		leaf, err := h.CA.MintCert(hostname)
		if err != nil {
			return false, nil, fmt.Errorf("mint cert for %s: %w", hostname, err)
		}
		tlsConn := tls.Server(plainConn, &tls.Config{
			Certificates: []tls.Certificate{*leaf},
		})
		if err := tlsConn.Handshake(); err != nil {
			return false, nil, fmt.Errorf("TLS handshake: %w", err)
		}
		return true, tlsConn, nil
	}

	return false, plainConn, nil
}

// writeAction serializes a ServerFacadeAction as an HTTP response.
func writeAction(conn net.Conn, action *bridgev1.ServerFacadeAction) error {
	httpResp := action.GetHttpResponse()
	if httpResp == nil {
		return fmt.Errorf("action has no http_response")
	}

	status := int(httpResp.GetStatus())
	if status == 0 {
		status = http.StatusOK
	}

	header := http.Header{}
	for k, v := range httpResp.GetHeaders() {
		header.Set(k, v)
	}

	var body []byte
	if httpResp.GetBody() != nil {
		var err error
		body, err = protojson.Marshal(httpResp.GetBody())
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		if header.Get("Content-Type") == "" {
			header.Set("Content-Type", "application/json")
		}
	}

	header.Set("Content-Length", strconv.Itoa(len(body)))

	resp := &http.Response{
		StatusCode:    status,
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		ContentLength: int64(len(body)),
		Body:          io.NopCloser(bytes.NewReader(body)),
	}

	return resp.Write(conn)
}

// bufferedConn wraps a net.Conn with a bufio.Reader so peeked bytes are not lost.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func newBufferedConn(conn net.Conn, r *bufio.Reader) *bufferedConn {
	return &bufferedConn{Conn: conn, r: r}
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}
