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

// ForwarderHTTPServer serves BridgeProxyService over HTTP (Connect RPC +
// WebSocket tunnel), forwarding all calls to an upstream via the Forwarder.
type ForwarderHTTPServer struct {
	forwarder  *Forwarder
	addr       string
	httpServer *http.Server
	upgrader   websocket.Upgrader
}

// NewForwarderHTTPServer creates an HTTP server backed by a Forwarder.
func NewForwarderHTTPServer(addr string, fwd *Forwarder) *ForwarderHTTPServer {
	s := &ForwarderHTTPServer{
		forwarder: fwd,
		addr:      addr,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}

	mux := http.NewServeMux()

	path, handler := bridgev1connect.NewBridgeProxyServiceHandler(&forwarderConnectHandler{fwd: fwd})
	mux.Handle(path, handler)

	mux.HandleFunc("/bridge.v1.BridgeProxyService/TunnelNetwork", s.handleTunnelWS)
	mux.HandleFunc("/healthz", s.handleHealthz)

	s.httpServer = &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	return s
}

func (s *ForwarderHTTPServer) Start() error {
	lis, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.addr, err)
	}
	slog.Info("Server proxy (HTTP) listening", "addr", s.addr)
	return s.httpServer.Serve(lis)
}

func (s *ForwarderHTTPServer) Shutdown(ctx context.Context) {
	slog.Info("shutting down server proxy (HTTP)")
	s.httpServer.Shutdown(ctx)
	s.forwarder.Close()
}

func (s *ForwarderHTTPServer) handleTunnelWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("WebSocket upgrade failed", "error", err)
		return
	}
	defer conn.Close()

	stream := tunnel.NewWSStream(conn)
	s.forwarder.HandleTunnelStream(r.Context(), stream)
}

func (s *ForwarderHTTPServer) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "serving"})
}

// forwarderConnectHandler adapts Forwarder to the Connect BridgeProxyServiceHandler.
type forwarderConnectHandler struct {
	bridgev1connect.UnimplementedBridgeProxyServiceHandler
	fwd *Forwarder
}

func (h *forwarderConnectHandler) ResolveDNSQuery(ctx context.Context, req *connect.Request[bridgev1.ProxyResolveDNSRequest]) (*connect.Response[bridgev1.ProxyResolveDNSResponse], error) {
	resp, err := h.fwd.ResolveDNSQuery(ctx, req.Msg)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(resp), nil
}

func (h *forwarderConnectHandler) GetMetadata(ctx context.Context, req *connect.Request[bridgev1.GetMetadataRequest]) (*connect.Response[bridgev1.GetMetadataResponse], error) {
	resp, err := h.fwd.GetMetadata(ctx, req.Msg)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(resp), nil
}

func (h *forwarderConnectHandler) CopyFiles(ctx context.Context, req *connect.Request[bridgev1.CopyFilesRequest]) (*connect.Response[bridgev1.CopyFilesResponse], error) {
	resp, err := h.fwd.CopyFiles(ctx, req.Msg)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(resp), nil
}

func (h *forwarderConnectHandler) TunnelNetwork(_ context.Context, _ *connect.BidiStream[bridgev1.TunnelNetworkMessage, bridgev1.TunnelNetworkMessage]) error {
	return connect.NewError(connect.CodeUnimplemented, errors.New("use the WebSocket endpoint at this path instead"))
}
