package tunnel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/puzpuzpuz/xsync/v3"
	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/plumbing"
	"google.golang.org/protobuf/proto"
)

const (
	maxReconnectDelay = 30 * time.Second
)

var _ TunnelDialer = (*Client)(nil)

// Client implements the tunnel client using WebSocket.
type Client struct {
	sandboxURL       string
	functionURL      string
	protectionBypass string
	appAddr          string // Local app address to forward inbound requests to

	conn              atomic.Pointer[websocket.Conn]
	isConnected       atomic.Bool
	reconnectAttempts atomic.Int32

	// Connection management
	connections *xsync.MapOf[string, *Conn]

	// Pending DNS resolution requests
	dnsRequests *xsync.MapOf[string, chan *bridgev1.ResolveDNSQueryResponse]
}

// NewClient creates a new tunnel client.
func NewClient(sandboxURL, functionURL, appAddr string) *Client {
	protectionBypass := getEnvOrDefault("VERCEL_AUTOMATION_BYPASS_SECRET", "")

	return &Client{
		sandboxURL:       sandboxURL,
		functionURL:      functionURL,
		protectionBypass: protectionBypass,
		appAddr:          appAddr,
		connections:      xsync.NewMapOf[string, *Conn](),
		dnsRequests:      xsync.NewMapOf[string, chan *bridgev1.ResolveDNSQueryResponse](),
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
		c.connections.Range(func(key string, conn *Conn) bool {
			_ = conn.Close()
			c.connections.Delete(key)
			return true
		})

		// Fail all pending DNS requests
		c.dnsRequests.Range(func(key string, ch chan *bridgev1.ResolveDNSQueryResponse) bool {
			close(ch)
			c.dnsRequests.Delete(key)
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

	// Handle DNS resolution responses
	if resp := msg.GetDnsResponse(); resp != nil {
		if ch, ok := c.dnsRequests.LoadAndDelete(resp.GetRequestId()); ok {
			ch <- resp
		}
		return
	}

	connID := msg.GetConnectionId()

	// Derive connection ID from source/dest if not provided
	if connID == "" {
		source := msg.GetSource()
		dest := msg.GetDest()
		if source != nil && dest != nil {
			connID = plumbing.ConnectionID(source.GetIp(), int(source.GetPort()), dest.GetIp(), int(dest.GetPort()))
		} else {
			slog.Error("Received an invalid message without a source or dest", "src", source, "dest", dest)
			return
		}
	}

	// Handle close messages
	if msg.GetClose() {
		if conn, ok := c.connections.LoadAndDelete(connID); ok {
			_ = conn.Close()
		}
		return
	}

	// Handle data for existing connection
	if conn, ok := c.connections.Load(connID); ok {
		select {
		case conn.readBuf <- msg.GetData():
		default:
			slog.Warn("Connection read buffer full", "connection_id", connID)
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

	// Forward to the local app if configured, otherwise use the dest from the message
	targetAddr := conn.dest
	if c.appAddr != "" {
		targetAddr = c.appAddr
	}

	// Connect to the destination
	targetConn, err := net.DialTimeout("tcp", targetAddr, 10*time.Second)
	if err != nil {
		slog.Error("Failed to connect to destination",
			"connection_id", connID,
			"dest", targetAddr,
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
	connID := plumbing.ConnectionID(sourceHost, sourcePort, destHost, destPort)

	// Check for existing connection
	if conn, ok := c.connections.Load(connID); ok {
		return conn, nil
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

// ResolveDNS sends a DNS resolution request through the tunnel and waits for
// the response. The context controls the timeout for the round-trip.
func (c *Client) ResolveDNS(ctx context.Context, hostname string) (*DNSResolveResult, error) {
	if !c.isConnected.Load() {
		return nil, fmt.Errorf("tunnel not connected")
	}

	// Generate a unique request ID
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	requestID := hex.EncodeToString(id)

	// Register a channel for the response before sending
	ch := make(chan *bridgev1.ResolveDNSQueryResponse, 1)
	c.dnsRequests.Store(requestID, ch)

	// Send the request
	if err := c.sendMessage(&bridgev1.Message{
		DnsRequest: &bridgev1.ResolveDNSQueryRequest{
			RequestId: requestID,
			Hostname:  hostname,
		},
	}); err != nil {
		c.dnsRequests.Delete(requestID)
		return nil, fmt.Errorf("failed to send DNS request: %w", err)
	}

	// Wait for the response or context cancellation
	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("tunnel disconnected while waiting for DNS response")
		}
		return &DNSResolveResult{
			Addresses: resp.GetAddresses(),
			Error:     resp.GetError(),
		}, nil
	case <-ctx.Done():
		c.dnsRequests.Delete(requestID)
		return nil, ctx.Err()
	}
}

// Close closes the tunnel connection.
func (c *Client) Close() error {
	c.isConnected.Store(false)
	conn := c.conn.Swap(nil)

	// Close all connections
	c.connections.Range(func(key string, conn *Conn) bool {
		conn.Close()
		c.connections.Delete(key)
		return true
	})

	if conn != nil {
		conn.Close()
	}

	slog.Info("Tunnel connection closed")
	return nil
}
