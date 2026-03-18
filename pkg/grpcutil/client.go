package grpcutil

import (
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// NewClient creates a gRPC client connection with default interceptors
// (logging) and insecure transport credentials. Additional dial options
// are appended after the defaults.
func NewClient(target string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	defaults := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(LoggingUnaryClientInterceptor()),
		grpc.WithChainStreamInterceptor(LoggingStreamClientInterceptor()),
	}
	return grpc.NewClient(target, append(defaults, opts...)...)
}
