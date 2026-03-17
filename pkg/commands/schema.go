package commands

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"
	"github.com/vercel/bridge/pkg/embeds"
)

func Schema() *cli.Command {
	return &cli.Command{
		Name:  "schema",
		Usage: "Print JSON schemas for bridge resources",
		Commands: []*cli.Command{
			{
				Name:  "reactor",
				Usage: "Print the JSON schema for a reactor spec (used with --server-mocks)",
				Action: func(_ context.Context, _ *cli.Command) error {
					fmt.Println(string(embeds.ReactorSchema))
					return nil
				},
			},
		},
	}
}
