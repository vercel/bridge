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

// FacadeHijacker implements Hijacker by matching incoming connections against
// compiled server facades.
type FacadeHijacker struct {
	ca      *CA
	facades []*CompiledFacade
}

// NewFacadeHijacker compiles the given ServerFacades and parses the CA for TLS minting.
// certPEM/keyPEM may be nil if TLS interception is not needed.
func NewFacadeHijacker(facades []*bridgev1.ServerFacade, certPEM, keyPEM []byte) (*FacadeHijacker, error) {
	compiled := make([]*CompiledFacade, 0, len(facades))
	for _, f := range facades {
		cf, err := CompileFacade(f)
		if err != nil {
			return nil, err
		}
		compiled = append(compiled, cf)
	}

	var ca *CA
	if len(certPEM) > 0 && len(keyPEM) > 0 {
		var err error
		ca, err = ParseCA(certPEM, keyPEM)
		if err != nil {
			return nil, fmt.Errorf("parse CA: %w", err)
		}
	}

	return &FacadeHijacker{ca: ca, facades: compiled}, nil
}

// ShouldHijack returns true if the message's hostname matches any facade's
// host glob pattern.
func (h *FacadeHijacker) ShouldHijack(msg *bridgev1.TunnelNetworkMessage) bool {
	hostname := msg.GetHostname()
	if hostname == "" {
		return false
	}
	for _, f := range h.facades {
		if hostmatch.Match(f.Host, hostname) {
			return true
		}
	}
	return false
}

// Hijack returns a net.Conn for the tunnel to read/write. It finds all facades
// matching the hostname and creates a Handler to serve the connection.
func (h *FacadeHijacker) Hijack(ctx context.Context, msg *bridgev1.TunnelNetworkMessage) (net.Conn, error) {
	hostname := msg.GetHostname()

	matched := h.matchingFacades(hostname)
	if len(matched) == 0 {
		return nil, fmt.Errorf("no matching facades for hostname %q", hostname)
	}

	tunnelConn, handlerConn := net.Pipe()

	dest := msg.GetDest()
	destAddr := fmt.Sprintf("%s:%d", dest.GetIp(), dest.GetPort())

	handler := &Handler{
		CA:      h.ca,
		Facades: matched,
		DestAddr: destAddr,
	}

	go func() {
		defer handlerConn.Close()
		if err := handler.Serve(handlerConn, hostname); err != nil {
			slog.WarnContext(ctx, "Facade handler error",
				"hostname", hostname,
				"error", err,
			)
		}
	}()

	return tunnelConn, nil
}

func (h *FacadeHijacker) matchingFacades(hostname string) []*CompiledFacade {
	var matched []*CompiledFacade
	for _, f := range h.facades {
		if hostmatch.Match(f.Host, hostname) {
			matched = append(matched, f)
		}
	}
	return matched
}
