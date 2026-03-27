package commands

import (
	"encoding/base64"
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

func newTestExecServer(t *testing.T) (*httptest.Server, *websocket.Conn) {
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
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return srv, conn
}

func sendStart(t *testing.T, conn *websocket.Conn, command []string) {
	t.Helper()
	msg := execStartRequest{Type: "start", Command: command}
	data, err := json.Marshal(msg)
	require.NoError(t, err)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, data))
}

func readMessages(t *testing.T, conn *websocket.Conn, timeout time.Duration) (outputs []execOutputMessage, exit execExitMessage) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(timeout))
	for {
		_, raw, err := conn.ReadMessage()
		require.NoError(t, err)
		var mt execMessageType
		require.NoError(t, json.Unmarshal(raw, &mt))
		switch mt.Type {
		case "output":
			var out execOutputMessage
			require.NoError(t, json.Unmarshal(raw, &out))
			outputs = append(outputs, out)
		case "exit":
			require.NoError(t, json.Unmarshal(raw, &exit))
			return
		}
	}
}

func decodeOutputs(t *testing.T, outputs []execOutputMessage, stream string) string {
	t.Helper()
	var sb strings.Builder
	for _, o := range outputs {
		if o.Stream == stream {
			data, err := base64.StdEncoding.DecodeString(o.Data)
			require.NoError(t, err)
			sb.Write(data)
		}
	}
	return sb.String()
}

func TestExecBasicStdout(t *testing.T) {
	_, conn := newTestExecServer(t)
	sendStart(t, conn, []string{"echo", "hello"})
	outputs, exit := readMessages(t, conn, 5*time.Second)

	assert.Equal(t, 0, exit.Code)
	assert.Empty(t, exit.Error)
	assert.Contains(t, decodeOutputs(t, outputs, "stdout"), "hello")
}

func TestExecStderr(t *testing.T) {
	_, conn := newTestExecServer(t)
	sendStart(t, conn, []string{"sh", "-c", "echo err >&2"})
	outputs, exit := readMessages(t, conn, 5*time.Second)

	assert.Equal(t, 0, exit.Code)
	assert.Contains(t, decodeOutputs(t, outputs, "stderr"), "err")
}

func TestExecNonZeroExit(t *testing.T) {
	_, conn := newTestExecServer(t)
	sendStart(t, conn, []string{"sh", "-c", "exit 42"})
	_, exit := readMessages(t, conn, 5*time.Second)

	assert.Equal(t, 42, exit.Code)
	assert.Empty(t, exit.Error)
}

func TestExecInvalidCommand(t *testing.T) {
	_, conn := newTestExecServer(t)
	sendStart(t, conn, []string{"nonexistent-binary-xyz"})

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := conn.ReadMessage()
	require.NoError(t, err)
	var exit execExitMessage
	require.NoError(t, json.Unmarshal(raw, &exit))
	assert.Equal(t, "exit", exit.Type)
	assert.NotEmpty(t, exit.Error)
}

func TestExecSignal(t *testing.T) {
	_, conn := newTestExecServer(t)
	sendStart(t, conn, []string{"sleep", "60"})

	// Give the process a moment to start, then send SIGTERM.
	time.Sleep(100 * time.Millisecond)
	sig := execSignalMessage{Type: "signal", Signal: "SIGTERM"}
	data, _ := json.Marshal(sig)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, data))

	_, exit := readMessages(t, conn, 5*time.Second)
	assert.NotEqual(t, 0, exit.Code)
}

func TestExecStdin(t *testing.T) {
	_, conn := newTestExecServer(t)
	// head -n1 reads one line from stdin and prints it.
	sendStart(t, conn, []string{"head", "-n1"})

	payload := base64.StdEncoding.EncodeToString([]byte("hello from stdin\n"))
	stdinMsg := execStdinMessage{Type: "stdin", Data: payload}
	data, _ := json.Marshal(stdinMsg)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, data))

	outputs, exit := readMessages(t, conn, 5*time.Second)
	assert.Equal(t, 0, exit.Code)
	assert.Contains(t, decodeOutputs(t, outputs, "stdout"), "hello from stdin")
}

func TestExecInvalidStartMessage(t *testing.T) {
	_, conn := newTestExecServer(t)

	// Send a message with wrong type.
	msg := map[string]any{"type": "wrong", "command": []string{"echo"}}
	data, _ := json.Marshal(msg)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, data))

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := conn.ReadMessage()
	require.NoError(t, err)
	var exit execExitMessage
	require.NoError(t, json.Unmarshal(raw, &exit))
	assert.Equal(t, "exit", exit.Type)
	assert.Contains(t, exit.Error, "start")
}

func TestExecEmptyCommand(t *testing.T) {
	_, conn := newTestExecServer(t)

	msg := execStartRequest{Type: "start", Command: []string{}}
	data, _ := json.Marshal(msg)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, data))

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := conn.ReadMessage()
	require.NoError(t, err)
	var exit execExitMessage
	require.NoError(t, json.Unmarshal(raw, &exit))
	assert.Equal(t, "exit", exit.Type)
	assert.Contains(t, exit.Error, "start")
}
