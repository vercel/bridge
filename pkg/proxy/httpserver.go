package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"connectrpc.com/connect"
	"github.com/gorilla/websocket"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/api/go/bridge/v1/bridgev1connect"
	"github.com/vercel/bridge/pkg/tunnel"
)

// HTTPServer serves the bridge proxy over HTTP using Connect RPC for unary
// methods and WebSocket for the bidirectional tunnel stream.
type HTTPServer struct {
	*Service
	addr       string
	httpServer *http.Server
	upgrader   websocket.Upgrader
}

// NewHTTPServer creates a new HTTP proxy server.
func NewHTTPServer(addr string, listenPorts []ListenPort, facades []*bridgev1.ServerFacade) *HTTPServer {
	s := &HTTPServer{
		Service: NewService(listenPorts, facades),
		addr:    addr,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}

	mux := http.NewServeMux()

	// Register Connect handlers for unary RPCs.
	path, handler := bridgev1connect.NewBridgeProxyServiceHandler(&connectHandler{svc: s.Service})
	mux.Handle(path, handler)

	// Register WebSocket handler for tunnel stream. This is mounted at the
	// same path as the Connect bidi stream RPC so clients use a consistent URL.
	mux.HandleFunc("/bridge.v1.BridgeProxyService/TunnelNetwork", s.handleTunnelWS)

	// Health check endpoint.
	mux.HandleFunc("/healthz", s.handleHealthz)

	s.httpServer = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	return s
}

// Start starts the HTTP server and ingress listeners.
func (s *HTTPServer) Start() error {
	s.Service.StartIngressListeners()

	lis, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.addr, err)
	}
	slog.Info("HTTP proxy server listening", "addr", s.addr)
	return s.httpServer.Serve(lis)
}

// Shutdown gracefully stops the HTTP server.
func (s *HTTPServer) Shutdown(ctx context.Context) {
	slog.Info("shutting down HTTP proxy server")
	s.httpServer.Shutdown(ctx)
}

// handleTunnelWS upgrades an HTTP connection to a WebSocket and runs the
// tunnel stream over it.
func (s *HTTPServer) handleTunnelWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("WebSocket upgrade failed", "error", err)
		return
	}
	defer conn.Close()

	stream := tunnel.NewWSStream(conn)
	s.Service.HandleTunnelStream(r.Context(), stream)
}

func (s *HTTPServer) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "serving"})
}

// connectHandler adapts Service to the Connect-generated BridgeProxyServiceHandler interface.
type connectHandler struct {
	bridgev1connect.UnimplementedBridgeProxyServiceHandler
	svc *Service
}

func (h *connectHandler) ResolveDNSQuery(ctx context.Context, req *connect.Request[bridgev1.ProxyResolveDNSRequest]) (*connect.Response[bridgev1.ProxyResolveDNSResponse], error) {
	resp, err := h.svc.ResolveDNSQuery(ctx, req.Msg)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(resp), nil
}

func (h *connectHandler) GetMetadata(ctx context.Context, req *connect.Request[bridgev1.GetMetadataRequest]) (*connect.Response[bridgev1.GetMetadataResponse], error) {
	resp, err := h.svc.GetMetadata(ctx, req.Msg)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(resp), nil
}

func (h *connectHandler) CopyFiles(ctx context.Context, req *connect.Request[bridgev1.CopyFilesRequest]) (*connect.Response[bridgev1.CopyFilesResponse], error) {
	resp, err := h.svc.CopyFiles(ctx, req.Msg)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(resp), nil
}

func (h *connectHandler) TunnelNetwork(_ context.Context, _ *connect.BidiStream[bridgev1.TunnelNetworkMessage, bridgev1.TunnelNetworkMessage]) error {
	return connect.NewError(connect.CodeUnimplemented, errors.New("use the WebSocket endpoint at this path instead"))
}
