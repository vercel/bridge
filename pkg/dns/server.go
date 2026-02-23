package dns

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"os"
	"strings"

	"github.com/vercel/bridge/pkg/conntrack"

	"github.com/miekg/dns"
)

// Server is a DNS server that delegates resolution to an ExchangeClient and
// allocates proxy IPs for intercepted responses.
type Server struct {
	exchangeClient ExchangeClient
	server         *dns.Server
	conn           *net.UDPConn
	port           int
	registry       *conntrack.Registry
}

// Config holds the DNS server configuration.
type Config struct {
	// ListenAddr is the address to listen on (e.g., "127.0.0.1:53")
	ListenAddr string

	// Client is the ExchangeClient used to resolve DNS queries
	Client ExchangeClient

	// Registry is the connection tracking registry for IP allocation
	Registry *conntrack.Registry
}

// New creates a new DNS server with the given configuration.
func New(cfg Config) (*Server, error) {
	// Create UDP listener to support port 0 (random port allocation)
	addr, err := net.ResolveUDPAddr("udp", cfg.ListenAddr)
	if err != nil {
		return nil, err
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, err
	}

	// Get the actual port
	actualAddr := conn.LocalAddr().(*net.UDPAddr)

	s := &Server{
		exchangeClient: cfg.Client,
		conn:           conn,
		port:           actualAddr.Port,
		registry:       cfg.Registry,
	}

	s.server = &dns.Server{
		PacketConn: conn,
		Handler:    dns.HandlerFunc(s.handleDNS),
	}

	return s, nil
}

// Port returns the port the DNS server is listening on.
func (s *Server) Port() int {
	return s.port
}

// ListenAndServe starts the DNS server.
func (s *Server) ListenAndServe() error {
	slog.Info("DNS server starting", "port", s.port)
	return s.server.ActivateAndServe()
}

// Shutdown gracefully shuts down the DNS server.
func (s *Server) Shutdown() error {
	return s.server.Shutdown()
}

// handleDNS processes incoming DNS queries by delegating to the exchange client.
func (s *Server) handleDNS(w dns.ResponseWriter, r *dns.Msg) {
	ctx := context.Background()

	resp, intercepted, err := s.exchangeClient.ExchangeContext(ctx, r)
	if err != nil {
		slog.Error("DNS exchange failed", "error", err)
		m := new(dns.Msg)
		m.SetRcode(r, dns.RcodeServerFailure)
		_ = w.WriteMsg(m)
		return
	}

	if !intercepted {
		if err := w.WriteMsg(resp); err != nil {
			slog.Error("Failed to write DNS response", "error", err)
		}
		return
	}

	// Intercepted: allocate proxy IPs for each A record answer
	var allocated []net.IP
	for i, rr := range resp.Answer {
		a, ok := rr.(*dns.A)
		if !ok {
			continue
		}

		hostname := strings.ToLower(strings.TrimSuffix(a.Hdr.Name, "."))
		proxyIP, err := s.registry.Register(hostname, a.A)
		if err != nil {
			slog.Error("Failed to allocate proxy IP", "hostname", hostname, "error", err)
			// Release any IPs we already allocated in this response
			for _, ip := range allocated {
				s.registry.Release(ip)
			}
			m := new(dns.Msg)
			m.SetRcode(r, dns.RcodeServerFailure)
			_ = w.WriteMsg(m)
			return
		}

		allocated = append(allocated, proxyIP)

		slog.Info("DNS intercepted",
			"hostname", hostname,
			"real_ip", a.A,
			"proxy_ip", proxyIP,
		)

		// Replace the real IP with the proxy IP in the response
		resp.Answer[i] = &dns.A{
			Hdr: a.Hdr,
			A:   proxyIP.To4(),
		}
	}

	if err := w.WriteMsg(resp); err != nil {
		slog.Error("Failed to write DNS response", "error", err)
		for _, ip := range allocated {
			s.registry.Release(ip)
		}
	}
}

// getSystemResolver reads the system's default DNS resolver from /etc/resolv.conf.
// Falls back to 8.8.8.8:53 if unable to determine system resolver.
func getSystemResolver() string {
	file, err := os.Open("/etc/resolv.conf")
	if err != nil {
		slog.Debug("Failed to open /etc/resolv.conf, using fallback", "error", err)
		return "8.8.8.8:53"
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "nameserver" {
			ns := fields[1]
			// Skip localhost to avoid loops (we're running on localhost)
			if ns == "127.0.0.1" || ns == "::1" || strings.HasPrefix(ns, "127.") {
				continue
			}
			if !strings.Contains(ns, ":") {
				ns = ns + ":53"
			}
			slog.Debug("Using system resolver", "nameserver", ns)
			return ns
		}
	}

	slog.Debug("No suitable nameserver found in /etc/resolv.conf, using fallback")
	return "8.8.8.8:53"
}

// matchWildcard checks if hostname matches a wildcard pattern.
// Supports * as a wildcard that matches any sequence of characters within a single label,
// and ** to match across multiple labels.
// Examples:
//   - "*.example.com" matches "foo.example.com" but not "bar.foo.example.com"
//   - "**.example.com" matches "foo.example.com" and "bar.foo.example.com"
//   - "api.*.internal" matches "api.foo.internal"
func matchWildcard(pattern, hostname string) bool {
	// Handle match-all wildcard.
	if pattern == "*" {
		return true
	}

	// Handle exact match
	if pattern == hostname {
		return true
	}

	// Handle ** (matches multiple labels)
	if strings.HasPrefix(pattern, "**.") {
		suffix := pattern[2:] // Include the dot: ".example.com"
		return strings.HasSuffix(hostname, suffix) || hostname == pattern[3:]
	}

	// Handle single * wildcard
	patternParts := strings.Split(pattern, ".")
	hostParts := strings.Split(hostname, ".")

	if len(patternParts) != len(hostParts) {
		return false
	}

	for i, pp := range patternParts {
		if pp == "*" {
			continue
		}
		if pp != hostParts[i] {
			return false
		}
	}

	return true
}
