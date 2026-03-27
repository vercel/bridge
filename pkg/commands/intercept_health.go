package commands

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/gorilla/websocket"
)

// interceptServer accepts inbound dispatcher tunnel connections and hands them
// to the interceptor stream via a channel.
type interceptServer struct {
	httpServer   *http.Server
	tunnelConnCh chan *websocket.Conn
	upgrader     websocket.Upgrader
}

func newInterceptServer(addr string) (*interceptServer, error) {
	s := &interceptServer{
		tunnelConnCh: make(chan *websocket.Conn, 1),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/__tunnel/connect", s.handleTunnelWS)
	mux.HandleFunc("/__exec", s.handleExecWS)

	s.httpServer = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	logger := slog.With("addr", lis.Addr().String())
	logger.Info("Intercept server listening")

	go s.httpServer.Serve(lis)
	return s, nil
}

func (s *interceptServer) SetReady() {}

func (s *interceptServer) Stop() {
	_ = s.httpServer.Shutdown(context.Background())
}

func (s *interceptServer) TunnelConnCh() <-chan *websocket.Conn {
	return s.tunnelConnCh
}

func (s *interceptServer) handleTunnelWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger := slog.With("path", r.URL.Path)
		logger.Error("WebSocket upgrade failed", "error", err)
		return
	}

	select {
	case s.tunnelConnCh <- conn:
	default:
		select {
		case staleConn := <-s.tunnelConnCh:
			if staleConn != nil {
				_ = staleConn.Close()
			}
		default:
		}
		s.tunnelConnCh <- conn
	}
}
