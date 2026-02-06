package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/puzpuzpuz/xsync/v3"
	bridgev1 "github.com/vercel-eddie/bridge/api/go/bridge/v1"
	"github.com/vercel-eddie/bridge/pkg/bidi"
	"google.golang.org/protobuf/proto"
)

const (
	// registrationTimeout is how long to wait for a registration message
	registrationTimeout = 30 * time.Second
)

// serverEntry represents a server (dispatcher) connection waiting to be paired with a client.
type serverEntry struct {
	conn *websocket.Conn
	done chan struct{} // closed when relay finishes
}

// WSServer is a WebSocket server that tunnels connections to a target.
type WSServer struct {
	httpServer *http.Server
	addr       string
	dialer     Dialer
	name       string
	upgrader   websocket.Upgrader
	conns      *xsync.MapOf[*websocket.Conn, struct{}]

	// Channel for server (dispatcher) connections waiting to be paired with a client.
	// Buffered to 1 so the server can register before a client arrives.
	pendingServer chan *serverEntry
}

// WSServerConfig configures the WebSocket server.
type WSServerConfig struct {
	Addr   string // Listen address (e.g., ":3000" or "0.0.0.0:3000")
	Dialer Dialer // Dialer for establishing connections to the target
	Name   string // Name of the sandbox
}

// NewWSServer creates a new WebSocket tunnel server.
func NewWSServer(cfg WSServerConfig) *WSServer {
	addr := cfg.Addr
	if addr == "" {
		addr = ":3000"
	}

	s := &WSServer{
		addr:          addr,
		dialer:        cfg.Dialer,
		name:          cfg.Name,
		conns:         xsync.NewMapOf[*websocket.Conn, struct{}](),
		pendingServer: make(chan *serverEntry, 1),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  32 * 1024,
			WriteBufferSize: 32 * 1024,
			CheckOrigin: func(r *http.Request) bool {
				return true // Allow all origins for tunnel
			},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/ssh", s.handleSSH)
	mux.HandleFunc("/tunnel", s.handleTunnel)

	s.httpServer = &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  0, // No timeout for WebSocket
		WriteTimeout: 0,
	}

	return s
}

func (s *WSServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Bridge-Name", s.name)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (s *WSServer) handleSSH(w http.ResponseWriter, r *http.Request) {
	wsConn, err := s.upgrader.Upgrade(w, r, http.Header{
		"X-Bridge-Name": []string{s.name},
	})
	if err != nil {
		slog.Error("failed to upgrade websocket", "error", err, "remote", r.RemoteAddr)
		return
	}

	s.conns.Store(wsConn, struct{}{})
	defer func() {
		s.conns.Delete(wsConn)
		wsConn.Close()
	}()

	remoteAddr := r.RemoteAddr
	slog.Info("SSH websocket tunnel connected", "remote", remoteAddr)

	// Dial the target
	targetConn, err := s.dialer.Dial(r.Context())
	if err != nil {
		slog.Error("failed to dial target", "error", err, "remote", remoteAddr)
		return
	}
	defer targetConn.Close()

	slog.Info("connected to SSH target", "remote", remoteAddr)

	// Create adapters for bidirectional copy
	wsAdapter := &wsConnAdapter{conn: wsConn}

	bidi.New(wsAdapter, targetConn).Wait(context.Background())

	slog.Info("SSH websocket tunnel disconnected", "remote", remoteAddr)
}

func (s *WSServer) handleTunnel(w http.ResponseWriter, r *http.Request) {
	wsConn, err := s.upgrader.Upgrade(w, r, http.Header{
		"X-Bridge-Name": []string{s.name},
	})
	if err != nil {
		slog.Error("failed to upgrade websocket for tunnel", "error", err, "remote", r.RemoteAddr)
		return
	}

	s.conns.Store(wsConn, struct{}{})
	defer func() {
		s.conns.Delete(wsConn)
		_ = wsConn.Close()
	}()

	remoteAddr := r.RemoteAddr
	slog.Debug("tunnel connection established", "remote", remoteAddr)

	// Set read deadline for registration message
	_ = wsConn.SetReadDeadline(time.Now().Add(registrationTimeout))

	// Wait for registration message
	messageType, data, err := wsConn.ReadMessage()
	if err != nil {
		slog.Error("failed to read registration message", "error", err, "remote", remoteAddr)
		return
	}

	// Clear the deadline after successful read
	_ = wsConn.SetReadDeadline(time.Time{})

	if messageType != websocket.BinaryMessage && messageType != websocket.TextMessage {
		slog.Error("unexpected message type for registration", "type", messageType, "remote", remoteAddr)
		return
	}

	// Parse the registration message
	var msg bridgev1.Message
	if err := proto.Unmarshal(data, &msg); err != nil {
		slog.Error("failed to parse registration message", "error", err, "remote", remoteAddr)
		return
	}

	reg := msg.GetRegistration()
	if reg == nil {
		slog.Error("registration message missing registration field", "remote", remoteAddr)
		return
	}

	slog.Debug("received tunnel registration",
		"remote", remoteAddr,
		"is_server", reg.GetIsServer(),
	)

	if reg.GetIsServer() {
		s.handleServerRegistration(wsConn, remoteAddr)
	} else {
		s.handleClientRegistration(r.Context(), wsConn, remoteAddr)
	}
}

func (s *WSServer) handleServerRegistration(wsConn *websocket.Conn, remoteAddr string) {
	entry := &serverEntry{
		conn: wsConn,
		done: make(chan struct{}),
	}

	select {
	case s.pendingServer <- entry:
		slog.Info("server registered, waiting for client", "remote", remoteAddr)
		// Block until the relay finishes
		<-entry.done
		slog.Debug("server handler exiting after tunnel closed", "remote", remoteAddr)
	default:
		slog.Warn("server connection rejected, slot full", "remote", remoteAddr)
		s.sendError(wsConn, "another server is already connected")
	}
}

func (s *WSServer) handleClientRegistration(ctx context.Context, wsConn *websocket.Conn, remoteAddr string) {
	// Wait for a server (dispatcher) to be available
	select {
	case entry := <-s.pendingServer:
		slog.Info("tunnel paired", "client", remoteAddr)

		// Relay messages between client and server
		relayMessages(wsConn, entry.conn)

		// Signal server handler to exit
		close(entry.done)

		slog.Debug("tunnel closed", "client", remoteAddr)

	case <-ctx.Done():
		slog.Error("timeout waiting for server connection", "remote", remoteAddr)
		s.sendError(wsConn, "timeout waiting for server connection")
	}
}

func (s *WSServer) sendError(wsConn *websocket.Conn, errMsg string) {
	msg := &bridgev1.Message{
		Error: errMsg,
		Close: true,
	}
	data, err := proto.Marshal(msg)
	if err != nil {
		slog.Error("failed to marshal error message", "error", err)
		return
	}
	if err := wsConn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		slog.Error("failed to send error message", "error", err)
	}
}

// relayMessages relays WebSocket messages between two connections,
// preserving message boundaries for proper protobuf parsing.
func relayMessages(conn1, conn2 *websocket.Conn) {
	done := make(chan struct{}, 2)

	// conn1 -> conn2
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			messageType, data, err := conn1.ReadMessage()
			if err != nil {
				if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					slog.Debug("relay read error from conn1", "error", err)
				}
				return
			}
			if err := conn2.WriteMessage(messageType, data); err != nil {
				slog.Debug("relay write error to conn2", "error", err)
				return
			}
		}
	}()

	// conn2 -> conn1
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			messageType, data, err := conn2.ReadMessage()
			if err != nil {
				if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
					slog.Debug("relay read error from conn2", "error", err)
				}
				return
			}
			if err := conn1.WriteMessage(messageType, data); err != nil {
				slog.Debug("relay write error to conn1", "error", err)
				return
			}
		}
	}()

	// Wait for one direction to finish
	<-done
}

// Start starts the WebSocket server.
func (s *WSServer) Start() error {
	slog.Info("starting websocket tunnel server", "addr", s.addr)
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully shuts down the server.
func (s *WSServer) Shutdown(ctx context.Context) error {
	slog.Info("shutting down websocket tunnel server")

	// Close all active WebSocket connections
	s.conns.Range(func(conn *websocket.Conn, _ struct{}) bool {
		conn.Close()
		return true
	})

	return s.httpServer.Shutdown(ctx)
}

// Addr returns the address the server is listening on.
func (s *WSServer) Addr() string {
	return s.addr
}

// wsConnAdapter adapts a websocket.Conn to io.ReadWriteCloser
type wsConnAdapter struct {
	conn    *websocket.Conn
	readMu  sync.Mutex
	writeMu sync.Mutex
	buf     []byte
	offset  int
}

func (a *wsConnAdapter) Read(p []byte) (int, error) {
	a.readMu.Lock()
	defer a.readMu.Unlock()

	// If we have buffered data, return it
	if a.offset < len(a.buf) {
		n := copy(p, a.buf[a.offset:])
		a.offset += n
		return n, nil
	}

	// Read next message
	messageType, data, err := a.conn.ReadMessage()
	if err != nil {
		return 0, err
	}

	if messageType != websocket.BinaryMessage {
		// Skip non-binary messages, try again
		return a.Read(p)
	}

	a.buf = data
	a.offset = 0

	n := copy(p, a.buf)
	a.offset = n
	return n, nil
}

func (a *wsConnAdapter) Write(p []byte) (int, error) {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()

	err := a.conn.WriteMessage(websocket.BinaryMessage, p)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func (a *wsConnAdapter) Close() error {
	return a.conn.Close()
}
