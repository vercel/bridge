package commands

import (
	"context"
	"fmt"
	"io"
	"time"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/admin"
	"github.com/vercel/bridge/pkg/interact"
)

// connectAdmin establishes a connection to the bridge administrator. It tries
// the remote administrator first; if unavailable, falls back to a local admin
// backed by the user's kubeconfig. The returned bool is true when the local
// fallback was used. The caller must defer adm.Close().
//
// Expects a Spinner in ctx (via interact.WithSpinner).
func connectAdmin(ctx context.Context, adminAddr, deviceID string) (admin.Service, bool, error) {
	sp := interact.GetSpinner(ctx)

	sp.SetTitle("Connecting to bridge administrator...")

	remote, dialErr := admin.NewClient(adminAddr)
	if dialErr == nil {
		// Probe the remote with a lightweight RPC to confirm it's reachable.
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, probeErr := remote.ListBridges(probeCtx, &bridgev1.ListBridgesRequest{DeviceId: deviceID})
		cancel()
		if probeErr == nil {
			return remote, false, nil
		}
		remote.Close()
	}

	// Remote unavailable — fall back to local.
	sp.SetTitle("Initializing local administrator...")

	local, err := admin.NewService(admin.LocalConfig{})
	if err != nil {
		return nil, false, fmt.Errorf("failed to initialize: %w", err)
	}
	return local, true, nil
}

// confirmLocalFallback prompts the user to confirm using local kubeconfig
// credentials. Returns true if the user accepts.
func confirmLocalFallback(p interact.Printer, r io.Reader) bool {
	kubeContext := currentKubeContext()
	p.Newline()
	p.Warn("No bridge administrator found in the cluster.")
	p.Info(fmt.Sprintf("Should Bridge use your local credentials for cluster %q instead.", kubeContext))
	p.Prompt("Continue? [y/N] ")

	answer := promptYN(r)
	return answer == "y" || answer == "yes"
}
