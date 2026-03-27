package commands

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/gorilla/websocket"
)

// execMessageType identifies the kind of WebSocket message in the exec protocol.
type execMessageType struct {
	Type string `json:"type"`
}

// execStartRequest is sent once by the client after WebSocket upgrade to start a command.
type execStartRequest struct {
	Type    string            `json:"type"`
	Command []string          `json:"command"`
	Dir     string            `json:"dir,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// execStdinMessage forwards data to the process's stdin.
type execStdinMessage struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

// execSignalMessage sends a signal to the process group.
type execSignalMessage struct {
	Type   string `json:"type"`
	Signal string `json:"signal"`
}

// execOutputMessage streams a chunk of process output to the client.
type execOutputMessage struct {
	Type   string `json:"type"`
	Stream string `json:"stream"`
	Data   string `json:"data"`
}

// execExitMessage is the final message sent when the process exits.
type execExitMessage struct {
	Type  string `json:"type"`
	Code  int    `json:"code"`
	Error string `json:"error,omitempty"`
}

func (s *interceptServer) handleExecWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("Exec WebSocket upgrade failed", "error", err)
		return
	}
	defer conn.Close()

	// Read the start request.
	_, msg, err := conn.ReadMessage()
	if err != nil {
		slog.Error("Failed to read exec start message", "error", err)
		return
	}

	var start execStartRequest
	if err := json.Unmarshal(msg, &start); err != nil {
		writeExecExit(conn, nil, -1, "invalid start message: "+err.Error())
		return
	}
	if start.Type != "start" || len(start.Command) == 0 {
		writeExecExit(conn, nil, -1, "first message must be type \"start\" with a non-empty command")
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	cmd := exec.CommandContext(ctx, start.Command[0], start.Command[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if start.Dir != "" {
		cmd.Dir = start.Dir
	}
	if len(start.Env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range start.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		writeExecExit(conn, nil, -1, "stdout pipe: "+err.Error())
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		writeExecExit(conn, nil, -1, "stderr pipe: "+err.Error())
		return
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		writeExecExit(conn, nil, -1, "stdin pipe: "+err.Error())
		return
	}

	if err := cmd.Start(); err != nil {
		writeExecExit(conn, nil, -1, "failed to start command: "+err.Error())
		return
	}

	var writeMu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(2)

	// Stream stdout.
	go func() {
		defer wg.Done()
		streamOutput(conn, &writeMu, stdout, "stdout")
	}()

	// Stream stderr.
	go func() {
		defer wg.Done()
		streamOutput(conn, &writeMu, stderr, "stderr")
	}()

	// Read client messages (stdin, signals).
	go func() {
		defer stdin.Close()
		for {
			_, raw, readErr := conn.ReadMessage()
			if readErr != nil {
				cancel()
				return
			}
			var mt execMessageType
			if json.Unmarshal(raw, &mt) != nil {
				continue
			}
			switch mt.Type {
			case "stdin":
				var sm execStdinMessage
				if json.Unmarshal(raw, &sm) != nil {
					continue
				}
				data, decErr := base64.StdEncoding.DecodeString(sm.Data)
				if decErr != nil {
					continue
				}
				_, _ = stdin.Write(data)
			case "signal":
				var sm execSignalMessage
				if json.Unmarshal(raw, &sm) != nil {
					continue
				}
				sig := parseSignal(sm.Signal)
				if sig != 0 && cmd.Process != nil {
					_ = syscall.Kill(-cmd.Process.Pid, sig)
				}
			}
		}
	}()

	// Wait for output streams to drain, then wait for process exit.
	wg.Wait()
	exitCode := 0
	waitErr := cmd.Wait()
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			writeExecExit(conn, &writeMu, -1, waitErr.Error())
			return
		}
	}

	writeExecExit(conn, &writeMu, exitCode, "")
}

func streamOutput(conn *websocket.Conn, mu *sync.Mutex, r io.Reader, stream string) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			msg := execOutputMessage{
				Type:   "output",
				Stream: stream,
				Data:   base64.StdEncoding.EncodeToString(buf[:n]),
			}
			data, _ := json.Marshal(msg)
			mu.Lock()
			_ = conn.WriteMessage(websocket.TextMessage, data)
			mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func writeExecExit(conn *websocket.Conn, mu *sync.Mutex, code int, errMsg string) {
	msg := execExitMessage{
		Type:  "exit",
		Code:  code,
		Error: errMsg,
	}
	data, _ := json.Marshal(msg)
	if mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	_ = conn.WriteMessage(websocket.TextMessage, data)
}

func parseSignal(name string) syscall.Signal {
	switch name {
	case "SIGTERM":
		return syscall.SIGTERM
	case "SIGKILL":
		return syscall.SIGKILL
	case "SIGINT":
		return syscall.SIGINT
	default:
		return 0
	}
}
