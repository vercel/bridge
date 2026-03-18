package grpcutil

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// LoggingUnaryServerInterceptor returns a server interceptor that logs every
// unary RPC with its method, duration, and status code.
func LoggingUnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		logRPC(info.FullMethod, time.Since(start), err)
		return resp, err
	}
}

// LoggingStreamServerInterceptor returns a server interceptor that logs every
// stream RPC with its method, duration, and status code.
func LoggingStreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()
		err := handler(srv, ss)
		logRPC(info.FullMethod, time.Since(start), err)
		return err
	}
}

// LoggingUnaryClientInterceptor returns a client interceptor that logs every
// unary RPC with its method, duration, and status code.
func LoggingUnaryClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		start := time.Now()
		err := invoker(ctx, method, req, reply, cc, opts...)
		logRPC(method, time.Since(start), err)
		return err
	}
}

// LoggingStreamClientInterceptor returns a client interceptor that logs every
// stream RPC with its method, duration, and status code.
func LoggingStreamClientInterceptor() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		start := time.Now()
		stream, err := streamer(ctx, desc, cc, method, opts...)
		logRPC(method, time.Since(start), err)
		return stream, err
	}
}

// RecoveryUnaryServerInterceptor returns a server interceptor that recovers
// from panics in unary handlers, logs the panic with a stack trace, and
// returns an Internal error to the client.
func RecoveryUnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in gRPC handler",
					"method", info.FullMethod,
					"panic", fmt.Sprint(r),
					"stack", string(debug.Stack()),
				)
				err = status.Errorf(codes.Internal, "internal error")
			}
		}()
		return handler(ctx, req)
	}
}

// RecoveryStreamServerInterceptor returns a server interceptor that recovers
// from panics in stream handlers, logs the panic with a stack trace, and
// returns an Internal error to the client.
func RecoveryStreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in gRPC stream handler",
					"method", info.FullMethod,
					"panic", fmt.Sprint(r),
					"stack", string(debug.Stack()),
				)
				err = status.Errorf(codes.Internal, "internal error")
			}
		}()
		return handler(srv, ss)
	}
}

// logRPC logs an RPC call. Internal errors are logged at Error level.
func logRPC(method string, duration time.Duration, err error) {
	code := status.Code(err)
	attrs := []any{
		"method", method,
		"code", code.String(),
		"duration", duration,
	}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	if code == codes.Internal {
		slog.Error("gRPC call", attrs...)
	} else {
		slog.Info("gRPC call", attrs...)
	}
}
