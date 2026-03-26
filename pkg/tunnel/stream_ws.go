package tunnel

import (
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
}

// NewWSStream wraps a WebSocket connection as a tunnel Stream.
func NewWSStream(conn *websocket.Conn) *WSStream {
	return &WSStream{conn: conn}
}

func (s *WSStream) Send(msg *bridgev1.TunnelNetworkMessage) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("ws stream: marshal: %w", err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		return fmt.Errorf("ws stream: send: %w", err)
	}
	return nil
}

func (s *WSStream) Recv() (*bridgev1.TunnelNetworkMessage, error) {
	_, data, err := s.conn.ReadMessage()
	if err != nil {
		return nil, fmt.Errorf("ws stream: recv: %w", err)
	}
	msg := &bridgev1.TunnelNetworkMessage{}
	if err := proto.Unmarshal(data, msg); err != nil {
		return nil, fmt.Errorf("ws stream: unmarshal: %w", err)
	}
	return msg, nil
}

// Close closes the underlying WebSocket connection.
func (s *WSStream) Close() error {
	return s.conn.Close()
}
