package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/vercel/bridge/pkg/commands"
	"github.com/vercel/bridge/pkg/interact"
)

var version = "dev"
var bridgeFeatureTag = ""

func main() {
	commands.Version = version
	commands.BridgeFeatureTag = bridgeFeatureTag

	app := commands.NewApp()
	if err := app.Run(context.Background(), os.Args); err != nil {
		slog.Error("fatal", "error", err)
		if interact.IsJSON() {
			commands.WriteErrorResult(os.Stdout, err)
		} else {
			p := interact.NewPrinter(os.Stderr)
			p.Errorf("%s", err)
		}
		os.Exit(1)
	}
}
