package e2e

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHackathonExecEcho(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	binPath := t.TempDir() + "/bridge"
	build := exec.Command("go", "build", "-o", binPath, "./cmd/bridge")
	build.Dir = ".."
	out, err := build.CombinedOutput()
	require.NoError(t, err, "build failed: %s", string(out))

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := lis.Addr().String()
	lis.Close()

	cmd := exec.Command(binPath, "hackathon", "--addr", addr)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	wsURL := fmt.Sprintf("ws://%s/__exec?cols=80&rows=24", addr)
	var conn *websocket.Conn
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, _, err = websocket.DefaultDialer.Dial(wsURL, nil)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.NoError(t, err, "failed to connect to hackathon server at %s", wsURL)
	t.Cleanup(func() { conn.Close() })

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("echo hello\n")))

	var sb strings.Builder
	readDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(readDeadline) {
		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, raw, readErr := conn.ReadMessage()
		if readErr != nil {
			break
		}
		sb.Write(raw)
		if strings.Contains(sb.String(), "hello") {
			break
		}
	}

	assert.Contains(t, sb.String(), "hello")
}
