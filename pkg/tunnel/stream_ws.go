package tunnel

import (
	"context"
	"fmt"
	"sync"

	"github.com/gorilla/websocket"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"google.golang.org/protobuf/proto"
)

// WSStream implements Stream over a WebSocket connection. Messages are
// serialized as protobuf and sent as binary WebSocket frames, mirroring the
// gRPC bidirectional stream wire behavior.
type WSStream struct {
	conn *websocket.Conn

	// WebSocket writes are not concurrent-safe; guard with a mutex.
	writeMu sync.Mutex
	connMu  sync.RWMutex

	refresher func(context.Context) (*websocket.Conn, error)
}

// NewWSStream wraps a WebSocket connection as a tunnel Stream.
func NewWSStream(conn *websocket.Conn) *WSStream {
	return &WSStream{conn: conn}
}

// NewReconnectableWSStream wraps a WebSocket connection as a tunnel Stream
// and uses the refresher to replace the underlying connection after failures.
func NewReconnectableWSStream(conn *websocket.Conn, refresher func(context.Context) (*websocket.Conn, error)) *WSStream {
	return &WSStream{
		conn:      conn,
		refresher: refresher,
	}
}

// NewWakeableWSStream waits for inbound WebSocket connections on connCh after
// calling wake. Refreshes replace the underlying connection with the next
// accepted socket from connCh.
func NewWakeableWSStream(connCh <-chan *websocket.Conn, wake func(context.Context) error) *WSStream {
	return &WSStream{
		refresher: func(ctx context.Context) (*websocket.Conn, error) {
			if connCh == nil {
				return nil, fmt.Errorf("ws stream: tunnel connection channel not configured")
			}

			select {
			case conn := <-connCh:
				if conn != nil {
					return conn, nil
				}
			default:
			}

			if wake == nil {
				return nil, fmt.Errorf("ws stream: wake not configured")
			}
			if err := wake(ctx); err != nil {
				return nil, err
			}
			select {
			case conn := <-connCh:
				if conn == nil {
					return nil, fmt.Errorf("ws stream: accepted connection is nil")
				}
				return conn, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
}

func (s *WSStream) Send(msg *bridgev1.TunnelNetworkMessage) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("ws stream: marshal: %w", err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	conn := s.currentConn()
	if conn == nil {
		return fmt.Errorf("ws stream: send: no active connection")
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		return fmt.Errorf("ws stream: send: %w", err)
	}
	return nil
}

func (s *WSStream) Recv() (*bridgev1.TunnelNetworkMessage, error) {
	conn := s.currentConn()
	if conn == nil {
		return nil, fmt.Errorf("ws stream: recv: no active connection")
	}
	_, data, err := conn.ReadMessage()
	if err != nil {
		return nil, fmt.Errorf("ws stream: recv: %w", err)
	}
	msg := &bridgev1.TunnelNetworkMessage{}
	if err := proto.Unmarshal(data, msg); err != nil {
		return nil, fmt.Errorf("ws stream: unmarshal: %w", err)
	}
	return msg, nil
}

func (s *WSStream) Refresh(ctx context.Context) error {
	if s.refresher == nil {
		return fmt.Errorf("ws stream: refresh not supported")
	}
	conn, err := s.refresher(ctx)
	if err != nil {
		return fmt.Errorf("ws stream: refresh: %w", err)
	}

	s.writeMu.Lock()
	s.connMu.Lock()
	oldConn := s.conn
	s.conn = conn
	s.connMu.Unlock()
	s.writeMu.Unlock()

	if oldConn != nil {
		_ = oldConn.Close()
	}
	return nil
}

// Close closes the underlying WebSocket connection.
func (s *WSStream) Close() error {
	s.writeMu.Lock()
	s.connMu.Lock()
	conn := s.conn
	s.conn = nil
	s.connMu.Unlock()
	s.writeMu.Unlock()
	if conn == nil {
		return nil
	}
	return conn.Close()
}

func (s *WSStream) currentConn() *websocket.Conn {
	s.connMu.RLock()
	defer s.connMu.RUnlock()
	return s.conn
}
