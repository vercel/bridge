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
				Name:  "server-facade",
				Usage: "Print the JSON schema for a server facade spec (used with --server-facade)",
				Action: func(_ context.Context, _ *cli.Command) error {
					fmt.Println(string(embeds.ServerFacadeSchema))
					return nil
				},
			},
			{
				Name:  "profile",
				Usage: "Print the JSON schema for .bridge/profile.json",
				Action: func(_ context.Context, _ *cli.Command) error {
					fmt.Println(string(embeds.ProfileSchema))
					return nil
				},
			},
		},
	}
}
