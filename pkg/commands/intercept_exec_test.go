package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestExecServer(t *testing.T, query string) *websocket.Conn {
	t.Helper()
	s := &interceptServer{
		tunnelConnCh: make(chan *websocket.Conn, 1),
		upgrader:     websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/__exec", s.handleExecWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/__exec"
	if query != "" {
		wsURL += "?" + query
	}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestExecEcho(t *testing.T) {
	conn := newTestExecServer(t, "cols=80&rows=24")

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("echo hello\n")))

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var sb strings.Builder
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			break
		}
		sb.Write(raw)
		if strings.Contains(sb.String(), "hello") {
			break
		}
	}
	assert.Contains(t, sb.String(), "hello")
}

func TestExecResize(t *testing.T) {
	conn := newTestExecServer(t, "cols=80&rows=24")

	resize := resizeMessage{Type: "resize", Cols: 120, Rows: 40}
	data, err := json.Marshal(resize)
	require.NoError(t, err)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, data))

	time.Sleep(100 * time.Millisecond)

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("echo ok\n")))

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var sb strings.Builder
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			break
		}
		sb.Write(raw)
		if strings.Contains(sb.String(), "ok") {
			break
		}
	}
	assert.Contains(t, sb.String(), "ok")
}

func TestExecExit(t *testing.T) {
	conn := newTestExecServer(t, "")

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("exit\n")))

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
	}
}
