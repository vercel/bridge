package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/vercel/bridge/pkg/commands"
)

var version = "dev"

func main() {
	commands.Version = version

	app := commands.NewApp()
	if err := app.Run(context.Background(), os.Args); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}
