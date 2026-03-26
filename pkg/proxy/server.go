package proxy

import "context"

// Server is the interface satisfied by both GRPCServer and HTTPServer.
type Server interface {
	Start() error
	Shutdown(ctx context.Context)
}
