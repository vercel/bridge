package tunnel

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	bridgev1 "github.com/vercel-eddie/bridge/api/go/bridge/v1"
	"google.golang.org/protobuf/proto"
)

const (
	maxReconnectDelay = 30 * time.Second
)

// Client implements the tunnel client using WebSocket.
type Client struct {
	sandboxURL       string
	functionURL      string
	deploymentID     string
	protectionBypass string

	conn              atomic.Pointer[websocket.Conn]
	isConnected       atomic.Bool
	reconnectAttempts atomic.Int32

	// Connection management
	connections sync.Map // map[string]*Conn
}

// NewClient creates a new tunnel client.
func NewClient(sandboxURL, functionURL string) *Client {
	// Get configuration from environment if not provided
	deploymentID := getEnvOrDefault("VERCEL_DEPLOYMENT_ID", "local")
	protectionBypass := getEnvOrDefault("VERCEL_AUTOMATION_BYPASS_SECRET", "")

	return &Client{
		sandboxURL:       sandboxURL,
		functionURL:      functionURL,
		deploymentID:     deploymentID,
		protectionBypass: protectionBypass,
	}
}

func getEnvOrDefault(key, defaultVal string) string {
	if v := getEnv(key); v != "" {
		return v
	}
	return defaultVal
}

// getEnv is a variable so it can be mocked in tests
var getEnv = os.Getenv

// ConnectWithReconnect connects and handles automatic reconnection until context is cancelled.
func (c *Client) ConnectWithReconnect(ctx context.Context) {
	logger := slog.With("component", "tunnel")
	for {
		select {
		case <-ctx.Done():
			logger.Info("Tunnel connection stopped", "reason", ctx.Err())
			return
		default:
		}

		err := c.connectAndServe(ctx)
		if err != nil {
			if ctx.Err() != nil {
				logger.Info("Tunnel connection stopped", "reason", ctx.Err())
				return
			}
			logger.Error("Connection error", "error", err)
		}

		c.isConnected.Store(false)
		c.conn.Store(nil)
		attempts := c.reconnectAttempts.Add(1)

		// Close all active connections
		c.connections.Range(func(key, value any) bool {
			if conn, ok := value.(*Conn); ok {
				_ = conn.Close()
			}
			c.connections.Delete(key)
			return true
		})

		delay := time.Duration(1<<uint(attempts-1)) * time.Second
		if delay > maxReconnectDelay {
			delay = maxReconnectDelay
		}

		logger.Info("Reconnecting", "delay", delay, "attempt", attempts)

		select {
		case <-ctx.Done():
			logger.Info("Tunnel connection stopped during reconnect delay", "reason", ctx.Err())
			return
		case <-time.After(delay):
		}
	}
}

func (c *Client) connectAndServe(ctx context.Context) error {
	slog.Info("Connecting to bridge server",
		"sandbox_url", c.sandboxURL,
		"function_url", c.functionURL,
		"deployment_id", c.deploymentID,
		"has_bypass_secret", c.protectionBypass != "",
	)

	// Convert HTTP URL to WebSocket URL and add /tunnel path
	wsURL := c.sandboxURL
	if strings.HasPrefix(wsURL, "https://") {
		wsURL = "wss://" + strings.TrimPrefix(wsURL, "https://")
	} else if strings.HasPrefix(wsURL, "http://") {
		wsURL = "ws://" + strings.TrimPrefix(wsURL, "http://")
	}
	wsURL = strings.TrimSuffix(wsURL, "/") + "/tunnel"

	u, err := url.Parse(wsURL)
	if err != nil {
		return fmt.Errorf("invalid sandbox URL: %w", err)
	}

	dialer := &websocket.Dialer{
		HandshakeTimeout: 30 * time.Second,
		ReadBufferSize:   32 * 1024,
		WriteBufferSize:  32 * 1024,
	}

	header := http.Header{}
	header.Set("Origin", fmt.Sprintf("https://%s", u.Host))

	conn, _, err := dialer.DialContext(ctx, wsURL, header)
	if err != nil {
		return fmt.Errorf("websocket dial failed: %w", err)
	}

	c.conn.Store(conn)

	// Send registration message
	reg := &bridgev1.Message{
		Registration: &bridgev1.Message_Registration{
			DeploymentId:           c.deploymentID,
			IsServer:               false, // We are the client
			FunctionUrl:            c.functionURL,
			ProtectionBypassSecret: c.protectionBypass,
		},
	}

	if err := c.sendMessage(reg); err != nil {
		conn.Close()
		return fmt.Errorf("failed to send registration: %w", err)
	}

	slog.Info("Registration sent, waiting for messages...")

	c.isConnected.Store(true)
	c.reconnectAttempts.Store(0)

	// Handle incoming messages
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		_, data, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return fmt.Errorf("connection closed by server")
			}
			return fmt.Errorf("read error: %w", err)
		}

		var msg bridgev1.Message
		if err := proto.Unmarshal(data, &msg); err != nil {
			slog.Warn("Failed to parse message", "error", err)
			continue
		}

		c.handleMessage(&msg)
	}
}

func (c *Client) handleMessage(msg *bridgev1.Message) {
	// Handle error messages from the server
	if errMsg := msg.GetError(); errMsg != "" {
		slog.Error("Received error from bridge server", "error", errMsg)
		return
	}

	connID := msg.GetConnectionId()

	// Derive connection ID from source/dest if not provided
	if connID == "" {
		source := msg.GetSource()
		dest := msg.GetDest()
		if source != nil && dest != nil {
			connID = generateConnectionID(source.GetIp(), int(source.GetPort()), dest.GetIp(), int(dest.GetPort()))
		} else {
			slog.Error("Received an invalid message without a source or dest", "src", source, "dest", dest)
			return
		}
	}

	// Handle close messages
	if msg.GetClose() {
		if value, ok := c.connections.LoadAndDelete(connID); ok {
			if conn, ok := value.(*Conn); ok {
				_ = conn.Close()
			}
		}
		return
	}

	// Handle data for existing connection
	if value, ok := c.connections.Load(connID); ok {
		if conn, ok := value.(*Conn); ok {
			select {
			case conn.readBuf <- msg.GetData():
			default:
				slog.Warn("Connection read buffer full", "connection_id", connID)
			}
		}
		return
	}

	// New inbound connection from bridge (dispatcher requesting connection)
	dest := msg.GetDest()
	if dest == nil {
		slog.Warn("Received message without destination", "connection_id", connID)
		return
	}

	sourceAddr := ""
	if source := msg.GetSource(); source != nil {
		sourceAddr = fmt.Sprintf("%s:%d", source.GetIp(), source.GetPort())
	}
	destAddr := fmt.Sprintf("%s:%d", dest.GetIp(), dest.GetPort())

	// Create and register the connection
	conn := newConn(connID, sourceAddr, destAddr, c)
	c.connections.Store(connID, conn)

	slog.Info("New inbound request from bridge",
		"connection_id", connID,
		"dest", destAddr,
	)

	// Handle the connection
	go c.handleInboundConnection(conn, msg.GetData())
}

func (c *Client) handleInboundConnection(conn *Conn, initialData []byte) {
	connID := conn.id
	destAddr := conn.dest

	// Connect to the destination
	targetConn, err := net.DialTimeout("tcp", destAddr, 10*time.Second)
	if err != nil {
		slog.Error("Failed to connect to destination",
			"connection_id", connID,
			"dest", destAddr,
			"error", err,
		)
		c.connections.Delete(connID)
		_ = c.sendClose(connID)
		return
	}
	defer targetConn.Close()

	// Write initial data if provided
	if len(initialData) > 0 {
		if _, err := targetConn.Write(initialData); err != nil {
			slog.Error("Failed to write initial data", "connection_id", connID, "error", err)
			c.connections.Delete(connID)
			_ = c.sendClose(connID)
			return
		}
	}

	defer func() {
		c.connections.Delete(connID)
		conn.Close()
	}()

	// Bidirectional copy
	done := make(chan struct{}, 2)

	// Bridge -> Destination
	go func() {
		for data := range conn.readBuf {
			if _, err := targetConn.Write(data); err != nil {
				slog.Debug("Error writing to destination", "error", err)
				break
			}
		}
		done <- struct{}{}
	}()

	// Destination -> Bridge
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := targetConn.Read(buf)
			if n > 0 {
				if sendErr := c.sendData(connID, buf[:n]); sendErr != nil {
					slog.Debug("Error sending to bridge", "error", sendErr)
					break
				}
			}
			if err != nil {
				if err != io.EOF {
					slog.Debug("Error reading from destination", "error", err)
				}
				break
			}
		}
		_ = c.sendClose(connID)
		done <- struct{}{}
	}()

	<-done
}

// DialThroughTunnel dials a destination through the tunnel.
func (c *Client) DialThroughTunnel(sourceAddr, destination string) (net.Conn, error) {
	if !c.isConnected.Load() {
		return nil, fmt.Errorf("tunnel not connected")
	}

	// Parse addresses
	sourceHost, sourcePortStr, _ := net.SplitHostPort(sourceAddr)
	sourcePort := 0
	fmt.Sscanf(sourcePortStr, "%d", &sourcePort)

	destHost, destPortStr, _ := net.SplitHostPort(destination)
	destPort := 0
	fmt.Sscanf(destPortStr, "%d", &destPort)

	// Generate connection ID
	connID := generateConnectionID(sourceHost, sourcePort, destHost, destPort)

	// Check for existing connection
	if value, ok := c.connections.Load(connID); ok {
		if conn, ok := value.(*Conn); ok {
			return conn, nil
		}
	}

	// Create new connection
	conn := newConn(connID, sourceAddr, destination, c)
	c.connections.Store(connID, conn)

	slog.Debug("Outbound connection tracked",
		"connection_id", connID,
		"source", sourceAddr,
		"destination", destination,
	)

	return conn, nil
}

func (c *Client) sendMessage(msg *bridgev1.Message) error {
	conn := c.conn.Load()
	if conn == nil {
		return fmt.Errorf("not connected")
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		return err
	}

	return conn.WriteMessage(websocket.BinaryMessage, data)
}

func (c *Client) sendData(connID string, data []byte) error {
	return c.sendMessage(&bridgev1.Message{
		ConnectionId: connID,
		Data:         data,
	})
}

func (c *Client) sendDataWithAddresses(connID string, data []byte, sourceAddr, destAddr string) error {
	sourceHost, sourcePortStr, _ := net.SplitHostPort(sourceAddr)
	sourcePort := int32(0)
	fmt.Sscanf(sourcePortStr, "%d", &sourcePort)

	destHost, destPortStr, _ := net.SplitHostPort(destAddr)
	destPort := int32(0)
	fmt.Sscanf(destPortStr, "%d", &destPort)

	return c.sendMessage(&bridgev1.Message{
		ConnectionId: connID,
		Data:         data,
		Source: &bridgev1.Message_Address{
			Ip:   sourceHost,
			Port: sourcePort,
		},
		Dest: &bridgev1.Message_Address{
			Ip:   destHost,
			Port: destPort,
		},
	})
}

func (c *Client) sendClose(connID string) error {
	return c.sendMessage(&bridgev1.Message{
		ConnectionId: connID,
		Close:        true,
	})
}

// Close closes the tunnel connection.
func (c *Client) Close() error {
	c.isConnected.Store(false)
	conn := c.conn.Swap(nil)

	// Close all connections
	c.connections.Range(func(key, value any) bool {
		if conn, ok := value.(*Conn); ok {
			conn.Close()
		}
		c.connections.Delete(key)
		return true
	})

	if conn != nil {
		conn.Close()
	}

	slog.Info("Tunnel connection closed")
	return nil
}
