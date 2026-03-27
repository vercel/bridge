package commands

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

type resizeMessage struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

func getShell() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		return shell
	}
	return "/bin/bash"
}

func (s *interceptServer) handleExecWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("Exec WebSocket upgrade failed", "error", err)
		return
	}
	defer conn.Close()

	cols, _ := strconv.Atoi(r.URL.Query().Get("cols"))
	rows, _ := strconv.Atoi(r.URL.Query().Get("rows"))
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}

	home, _ := os.UserHomeDir()

	shell := getShell()
	cmd := exec.Command(shell)
	cmd.Dir = filepath.Join(home, "devbox-todo")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor")

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
	})
	if err != nil {
		slog.Error("Failed to start PTY", "error", err)
		return
	}
	defer ptmx.Close()

	logger := slog.With("shell", shell, "pid", cmd.Process.Pid)
	logger.Info("PTY session started")

	done := make(chan struct{})

	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, readErr := ptmx.Read(buf)
			if n > 0 {
				if writeErr := conn.WriteMessage(websocket.TextMessage, buf[:n]); writeErr != nil {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}()

	go func() {
		for {
			_, raw, readErr := conn.ReadMessage()
			if readErr != nil {
				cmd.Process.Kill()
				return
			}

			msg := string(raw)
			if strings.HasPrefix(msg, "{") {
				var resize resizeMessage
				if json.Unmarshal(raw, &resize) == nil && resize.Type == "resize" {
					pty.Setsize(ptmx, &pty.Winsize{
						Cols: uint16(resize.Cols),
						Rows: uint16(resize.Rows),
					})
					continue
				}
			}

			ptmx.Write(raw)
		}
	}()

	<-done
	cmd.Wait()
	logger.Info("PTY session ended")
}
