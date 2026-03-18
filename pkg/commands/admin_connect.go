package commands

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/vercel/bridge/pkg/admin"
	"github.com/vercel/bridge/pkg/grpcutil"
	"github.com/vercel/bridge/pkg/interact"
)

// connectAdmin establishes a connection to the bridge administrator.
// The caller must defer adm.Close().
//
// Expects a Spinner in ctx (via interact.WithSpinner).
func connectAdmin(ctx context.Context, adminAddr string) (admin.Service, error) {
	sp := interact.GetSpinner(ctx)

	sp.SetTitle("Connecting to bridge administrator...")
	logger := slog.With("addr", adminAddr)

	remote, dialErr := admin.NewClient(adminAddr)
	if dialErr == nil {
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		healthErr := grpcutil.WaitForHealthy(probeCtx, remote.Conn(), 500*time.Millisecond)
		cancel()
		if healthErr == nil {
			return remote, nil
		}
		logger.ErrorContext(ctx, "Failed to connect to administrator.", "err", healthErr.Error())
		remote.Close()
	}

	return nil, fmt.Errorf("unable to connect to bridge administrator at %s", adminAddr)
}
