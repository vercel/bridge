package commands

import (
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/vercel/bridge/pkg/mutagen"
)

// FileSyncComponent manages a mutagen file sync session.
type FileSyncComponent struct {
	client   *mutagen.Client
	syncName string
}

// FileSyncConfig holds configuration for starting the file sync component.
type FileSyncConfig struct {
	SyncName   string
	SyncSource string
	SyncTarget string
}

// StartFileSync installs mutagen, creates a client, and starts a sync session.
func StartFileSync(cfg FileSyncConfig) (*FileSyncComponent, error) {
	slog.Info("Checking mutagen installation...")
	if err := mutagen.Install(); err != nil {
		return nil, fmt.Errorf("failed to install mutagen: %w", err)
	}
	slog.Info("Mutagen installed", "path", mutagen.BinaryPath())

	client, err := mutagen.NewClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create mutagen client: %w", err)
	}

	absSource, err := filepath.Abs(cfg.SyncSource)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve sync source: %w", err)
	}
	slog.Info("Starting mutagen sync", "source", cfg.SyncSource, "abs_source", absSource, "target", cfg.SyncTarget)

	if err := client.CreateSyncSession(mutagen.SyncConfig{
		Name:      cfg.SyncName,
		Source:    absSource,
		Target:    cfg.SyncTarget,
		IgnoreVCS: true,
		SyncMode:  "two-way-resolved",
	}); err != nil {
		return nil, fmt.Errorf("failed to create mutagen sync: %w", err)
	}

	slog.Info("Mutagen sync started", "name", cfg.SyncName)

	return &FileSyncComponent{
		client:   client,
		syncName: cfg.SyncName,
	}, nil
}

// Stop terminates the mutagen sync session.
func (f *FileSyncComponent) Stop() {
	if f.client == nil || f.syncName == "" {
		return
	}

	if err := f.client.TerminateSyncSession(f.syncName); err != nil {
		slog.Error("Failed to terminate mutagen sync", "error", err)
	} else {
		slog.Info("Mutagen sync terminated", "name", f.syncName)
	}
}
