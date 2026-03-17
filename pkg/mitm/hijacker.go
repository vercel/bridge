package mitm

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	"github.com/vercel/bridge/pkg/hostmatch"
)

// Hijacker intercepts outbound tunnel connections. The tunnel calls
// ShouldHijack for each new connection; if it returns true the tunnel calls
// Hijack instead of dialing the real destination.
type Hijacker interface {
	// ShouldHijack returns true if the hijacker wants to handle this connection.
	ShouldHijack(msg *bridgev1.TunnelNetworkMessage) bool

	// Hijack returns a net.Conn that the tunnel reads from and writes to,
	// just like a dialed connection. The hijacker is responsible for
	// servicing the other end (e.g. via a net.Pipe goroutine).
	Hijack(ctx context.Context, msg *bridgev1.TunnelNetworkMessage) (net.Conn, error)
}

// ReactorHijacker implements Hijacker by matching incoming connections against
// compiled reactors.
type ReactorHijacker struct {
	ca       *CA
	reactors []*CompiledReactor
}

// NewReactorHijacker compiles the given Reactors and parses the CA for TLS minting.
// certPEM/keyPEM may be nil if TLS interception is not needed.
func NewReactorHijacker(reactors []*bridgev1.Reactor, certPEM, keyPEM []byte) (*ReactorHijacker, error) {
	compiled := make([]*CompiledReactor, 0, len(reactors))
	for _, r := range reactors {
		cr, err := CompileReactor(r)
		if err != nil {
			return nil, err
		}
		compiled = append(compiled, cr)
	}

	var ca *CA
	if len(certPEM) > 0 && len(keyPEM) > 0 {
		var err error
		ca, err = ParseCA(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("parse CA: %w", err)
		}
	}

	return &ReactorHijacker{ca: ca, reactors: compiled}, nil
}

// ShouldHijack returns true if the message's hostname matches any reactor's
// host glob pattern.
func (h *ReactorHijacker) ShouldHijack(msg *bridgev1.TunnelNetworkMessage) bool {
	hostname := msg.GetHostname()
	if hostname == "" {
		return false
	}
	for _, r := range h.reactors {
		if hostmatch.Match(r.Host, hostname) {
			return true
		}
	}
	return false
}

// Hijack returns a net.Conn for the tunnel to read/write. It finds all reactors
// matching the hostname and creates a Handler to serve the connection.
func (h *ReactorHijacker) Hijack(ctx context.Context, msg *bridgev1.TunnelNetworkMessage) (net.Conn, error) {
	hostname := msg.GetHostname()

	matched := h.matchingReactors(hostname)
	if len(matched) == 0 {
		return nil, fmt.Errorf("no matching reactors for hostname %q", hostname)
	}

	tunnelConn, handlerConn := net.Pipe()

	dest := msg.GetDest()
	destAddr := fmt.Sprintf("%s:%d", dest.GetIp(), dest.GetPort())

	handler := &Handler{
		CA:       h.ca,
		Reactors: matched,
		DestAddr: destAddr,
	}

	go func() {
		defer handlerConn.Close()
		if err := handler.Serve(handlerConn, hostname); err != nil {
			slog.WarnContext(ctx, "Reactor handler error",
				"hostname", hostname,
				"error", err,
			)
		}
	}()

	return tunnelConn, nil
}

func (h *ReactorHijacker) matchingReactors(hostname string) []*CompiledReactor {
	var matched []*CompiledReactor
	for _, r := range h.reactors {
		if hostmatch.Match(r.Host, hostname) {
			matched = append(matched, r)
		}
	}
	return matched
}
