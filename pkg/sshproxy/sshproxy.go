// Package sshproxy provides a local SSH proxy that tunnels connections through WebSocket.
package sshproxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vercel/bridge/pkg/netutil"
	"github.com/vercel/bridge/pkg/proxy"
)

// SSHProxy manages a local TCP proxy for SSH connections tunneled through WebSocket.
type SSHProxy struct {
	name      string
	tunnelURL string
	proxy     *proxy.TCPProxyListener
	port      int
}

// Config configures the SSH proxy.
type Config struct {
	// Name is the sandbox name used for SSH config alias (e.g., "my-sandbox" -> "bridge.my-sandbox")
	Name string
	// TunnelURL is the WebSocket URL to tunnel connections through
	TunnelURL string
	// LocalPort is the local port to listen on (0 for random)
	LocalPort int
}

// New creates and starts a new SSH proxy.
func New(ctx context.Context, cfg Config) (*SSHProxy, error) {
	// Create WebSocket dialer for the tunnel
	wsDialer := proxy.NewWSDialer(cfg.TunnelURL)

	// Create TCP proxy that uses the WebSocket dialer
	// Bind to 127.0.0.1 explicitly for IPv4 compatibility
	tcpProxy, err := proxy.NewTCPProxyListener(ctx, "127.0.0.1:"+strconv.Itoa(cfg.LocalPort), wsDialer)
	if err != nil {
		return nil, fmt.Errorf("failed to create TCP proxy: %w", err)
	}

	actualPort := tcpProxy.Port()

	// Update SSH config with the actual port
	if err := updateSSHConfig(cfg.Name, actualPort); err != nil {
		tcpProxy.Close()
		return nil, fmt.Errorf("failed to update SSH config: %w", err)
	}

	// Log the SSH config location for debugging
	if home, err := os.UserHomeDir(); err == nil {
		slog.Info("SSH config updated",
			"bridge_config", filepath.Join(home, ".bridge", "ssh_config"),
			"main_config", filepath.Join(home, ".ssh", "config"),
		)
	}

	slog.Info("SSH proxy started",
		"name", cfg.Name,
		"host", fmt.Sprintf("bridge.%s", cfg.Name),
		"port", actualPort,
	)

	return &SSHProxy{
		name:      cfg.Name,
		tunnelURL: cfg.TunnelURL,
		proxy:     tcpProxy,
		port:      actualPort,
	}, nil
}

// Name returns the sandbox name.
func (s *SSHProxy) Name() string {
	return s.name
}

// Host returns the SSH host alias (e.g., "bridge.my-sandbox").
func (s *SSHProxy) Host() string {
	return fmt.Sprintf("bridge.%s", s.name)
}

// Port returns the local port the proxy is listening on.
func (s *SSHProxy) Port() int {
	return s.port
}

// Addr returns the local address the proxy is listening on.
func (s *SSHProxy) Addr() net.Addr {
	return s.proxy.Addr()
}

// Serve starts accepting connections and blocks until the context is cancelled.
func (s *SSHProxy) Serve(ctx context.Context) error {
	return netutil.AcceptLoop(ctx, s.proxy, func(conn net.Conn) {
		s.proxy.HandleConn(conn)
	})
}

// Close stops the proxy and removes the SSH config entry.
func (s *SSHProxy) Close() error {
	var errs []error

	if err := s.proxy.Close(); err != nil {
		errs = append(errs, fmt.Errorf("failed to close proxy: %w", err))
	}

	if err := removeSSHConfig(s.name); err != nil {
		errs = append(errs, fmt.Errorf("failed to remove SSH config: %w", err))
	} else {
		slog.Info("SSH config removed", "host", s.Host())
	}

	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

func updateSSHConfig(name string, port int) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	bridgeDir := filepath.Join(home, ".bridge")
	if err := os.MkdirAll(bridgeDir, 0755); err != nil {
		return err
	}

	// Manage bridge SSH configs in a separate file
	bridgeSSHConfig := filepath.Join(bridgeDir, "ssh_config")

	// Read existing config
	existingConfig := ""
	if data, err := os.ReadFile(bridgeSSHConfig); err == nil {
		existingConfig = string(data)
	}

	hostAlias := fmt.Sprintf("bridge.%s", name)
	hostEntry := fmt.Sprintf(`Host %s
    HostName 127.0.0.1
    Port %d
    ForwardAgent yes
    StrictHostKeyChecking no
    UserKnownHostsFile /dev/null

`, hostAlias, port)

	// Check if entry already exists and update it, or append
	if strings.Contains(existingConfig, fmt.Sprintf("Host %s\n", hostAlias)) {
		// Remove existing entry and add new one
		lines := strings.Split(existingConfig, "\n")
		var newLines []string
		skip := false
		for _, line := range lines {
			if strings.HasPrefix(line, "Host "+hostAlias) {
				skip = true
				continue
			}
			if skip && strings.HasPrefix(line, "Host ") {
				skip = false
			}
			if !skip {
				newLines = append(newLines, line)
			}
		}
		existingConfig = strings.Join(newLines, "\n")
	}

	newConfig := existingConfig + hostEntry

	if err := os.WriteFile(bridgeSSHConfig, []byte(newConfig), 0644); err != nil {
		return err
	}

	// Ensure main SSH config includes our config
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return err
	}

	mainSSHConfig := filepath.Join(sshDir, "config")
	includeDirective := fmt.Sprintf("Include %s\n", bridgeSSHConfig)

	mainConfig := ""
	if data, err := os.ReadFile(mainSSHConfig); err == nil {
		mainConfig = string(data)
	}

	if !strings.Contains(mainConfig, bridgeSSHConfig) {
		// Add include at the beginning
		newMainConfig := includeDirective + mainConfig
		if err := os.WriteFile(mainSSHConfig, []byte(newMainConfig), 0644); err != nil {
			return err
		}
	}

	return nil
}

func removeSSHConfig(name string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	bridgeSSHConfig := filepath.Join(home, ".bridge", "ssh_config")

	data, err := os.ReadFile(bridgeSSHConfig)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	hostAlias := fmt.Sprintf("bridge.%s", name)
	existingConfig := string(data)

	if !strings.Contains(existingConfig, fmt.Sprintf("Host %s\n", hostAlias)) {
		return nil
	}

	// Remove the entry
	lines := strings.Split(existingConfig, "\n")
	var newLines []string
	skip := false
	for _, line := range lines {
		if strings.HasPrefix(line, "Host "+hostAlias) {
			skip = true
			continue
		}
		if skip && strings.HasPrefix(line, "Host ") {
			skip = false
		}
		if skip && strings.TrimSpace(line) == "" {
			continue
		}
		if !skip {
			newLines = append(newLines, line)
		}
	}

	newConfig := strings.Join(newLines, "\n")
	return os.WriteFile(bridgeSSHConfig, []byte(newConfig), 0644)
}
