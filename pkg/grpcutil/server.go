package grpcutil

import (
	"google.golang.org/grpc"
)

// NewServer creates a gRPC server with default interceptors (recovery and
// logging) for both unary and stream RPCs. Additional server options are
// appended after the defaults.
func NewServer(opts ...grpc.ServerOption) *grpc.Server {
	defaults := []grpc.ServerOption{
		grpc.ChainUnaryInterceptor(
			RecoveryUnaryServerInterceptor(),
			LoggingUnaryServerInterceptor(),
		),
		grpc.ChainStreamInterceptor(
			RecoveryStreamServerInterceptor(),
			LoggingStreamServerInterceptor(),
		),
	}
	return grpc.NewServer(append(defaults, opts...)...)
}
