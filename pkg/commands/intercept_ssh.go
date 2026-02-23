package commands

import (
	"context"

	"github.com/vercel/bridge/pkg/sshproxy"
)

// SSHComponent wraps an SSH proxy for tunneled SSH connections.
type SSHComponent struct {
	proxy *sshproxy.SSHProxy
}

// SSHConfig holds configuration for starting the SSH component.
type SSHConfig struct {
	Name       string
	SandboxURL string
	LocalPort  int
}

// StartSSH creates and starts the SSH proxy.
func StartSSH(ctx context.Context, cfg SSHConfig) (*SSHComponent, error) {
	proxy, err := sshproxy.New(ctx, sshproxy.Config{
		Name:      cfg.Name,
		TunnelURL: cfg.SandboxURL,
		LocalPort: cfg.LocalPort,
	})
	if err != nil {
		return nil, err
	}

	return &SSHComponent{proxy: proxy}, nil
}

// Host returns the SSH host alias (e.g., "bridge.my-sandbox").
func (s *SSHComponent) Host() string {
	return s.proxy.Host()
}

// Serve starts accepting SSH connections and blocks until the context is cancelled.
func (s *SSHComponent) Serve(ctx context.Context) error {
	return s.proxy.Serve(ctx)
}

// Stop closes the SSH proxy and removes its SSH config entry.
func (s *SSHComponent) Stop() {
	_ = s.proxy.Close()
}
