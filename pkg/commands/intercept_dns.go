package commands

import (
	"fmt"
	"log/slog"

	"github.com/vercel-eddie/bridge/pkg/conntrack"
	bridgedns "github.com/vercel-eddie/bridge/pkg/dns"
)

// DNSComponent manages the DNS server for domain interception.
type DNSComponent struct {
	server *bridgedns.Server
	port   int
}

// DNSConfig holds configuration for starting the DNS component.
type DNSConfig struct {
	ListenPort int
	Client     bridgedns.ExchangeClient
	Registry   *conntrack.Registry
}

// StartDNS creates the DNS interception component.
func StartDNS(cfg DNSConfig) (*DNSComponent, error) {
	dnsServer, err := bridgedns.New(bridgedns.Config{
		ListenAddr: fmt.Sprintf("127.0.0.1:%d", cfg.ListenPort),
		Client:     cfg.Client,
		Registry:   cfg.Registry,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create DNS server: %w", err)
	}

	go func() {
		if err := dnsServer.ListenAndServe(); err != nil {
			slog.Error("DNS server error", "error", err)
		}
	}()

	port := dnsServer.Port()
	slog.Info("DNS interception enabled", "port", port)

	return &DNSComponent{
		server: dnsServer,
		port:   port,
	}, nil
}

// Port returns the port the DNS server is listening on.
func (d *DNSComponent) Port() int {
	return d.port
}

// Stop shuts down the DNS server.
func (d *DNSComponent) Stop() {
	if d.server != nil {
		_ = d.server.Shutdown()
	}
}
