package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
)

// BridgeMetricsInterceptor returns a gRPC unary server interceptor that records
// per-call metrics tagged with bridge.device_id and bridge.workload_name
// extracted from the request message. It also enriches the active span
// (created by otelgrpc) with the same attributes.
func BridgeMetricsInterceptor() grpc.UnaryServerInterceptor {
	meter := otel.Meter("bridge.administrator")
	callCounter, _ := meter.Int64Counter("bridge.rpc.calls",
		metric.WithDescription("Total gRPC calls by method, device, and workload"),
	)
	callDuration, _ := meter.Float64Histogram("bridge.rpc.duration",
		metric.WithDescription("gRPC call duration by method, device, and workload"),
		metric.WithUnit("s"),
	)

	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		deviceID, workload := extractIdentifiers(req)

		attrs := []attribute.KeyValue{
			attribute.String("rpc.method", info.FullMethod),
		}
		if deviceID != "" {
			attrs = append(attrs, attribute.String("bridge.device_id", deviceID))
		}
		if workload != "" {
			attrs = append(attrs, attribute.String("bridge.workload_name", workload))
		}

		// Enrich the span created by otelgrpc with bridge-specific attributes.
		span := trace.SpanFromContext(ctx)
		span.SetAttributes(attrs...)

		start := time.Now()
		resp, err := handler(ctx, req)
		elapsed := time.Since(start).Seconds()

		code := status.Code(err)
		attrs = append(attrs, attribute.String("rpc.grpc.status_code", code.String()))

		callCounter.Add(ctx, 1, metric.WithAttributes(attrs...))
		callDuration.Record(ctx, elapsed, metric.WithAttributes(attrs...))

		return resp, err
	}
}

// extractIdentifiers pulls device_id and workload name from known request types.
func extractIdentifiers(req any) (deviceID, workload string) {
	switch r := req.(type) {
	case *bridgev1.CreateBridgeRequest:
		return r.GetDeviceId(), r.GetSourceDeployment()
	case *bridgev1.ListBridgesRequest:
		return r.GetDeviceId(), ""
	case *bridgev1.DeleteBridgeRequest:
		return r.GetDeviceId(), r.GetName()
	default:
		return "", ""
	}
}
