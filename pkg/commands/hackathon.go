package commands

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/gorilla/websocket"
	"github.com/urfave/cli/v3"
)

func Hackathon() *cli.Command {
	return &cli.Command{
		Name:  "hackathon",
		Usage: "Run a standalone __exec WebSocket server",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "addr",
				Usage: "Address to listen on",
				Value: ":8080",
			},
		},
		Action: runHackathon,
	}
}

func runHackathon(ctx context.Context, c *cli.Command) error {
	addr := c.String("addr")

	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	srv := &interceptServer{
		upgrader: upgrader,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/__exec", srv.handleExecWS)

	httpServer := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	logger := slog.With("addr", lis.Addr().String())
	logger.Info("Hackathon exec server listening")

	go httpServer.Serve(lis)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sigChan:
		logger.Info("Shutting down...")
	case <-ctx.Done():
	}

	return httpServer.Shutdown(context.Background())
}
