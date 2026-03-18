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
// per-call metrics tagged with bridge-specific attributes extracted from the
// request message. It also enriches the active span (created by otelgrpc)
// with the same attributes.
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
		deviceID, workload, deviceInfo := extractRequestAttrs(req)

		attrs := []attribute.KeyValue{
			attribute.String("rpc.method", info.FullMethod),
		}
		if deviceID != "" {
			attrs = append(attrs, attribute.String("bridge.device_id", deviceID))
		}
		if workload != "" {
			attrs = append(attrs, attribute.String("bridge.workload_name", workload))
		}
		if deviceInfo != nil {
			if deviceInfo.Os != "" {
				attrs = append(attrs, attribute.String("bridge.device_os", deviceInfo.Os))
			}
			if deviceInfo.Arch != "" {
				attrs = append(attrs, attribute.String("bridge.device_arch", deviceInfo.Arch))
			}
			if deviceInfo.BridgeVersion != "" {
				attrs = append(attrs, attribute.String("bridge.version", deviceInfo.BridgeVersion))
			}
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

// extractRequestAttrs pulls device_id, workload name, and device info from known request types.
func extractRequestAttrs(req any) (deviceID, workload string, info *bridgev1.DeviceInfo) {
	switch r := req.(type) {
	case *bridgev1.CreateBridgeRequest:
		return r.GetDeviceId(), r.GetSourceDeployment(), r.GetDeviceInfo()
	case *bridgev1.ListBridgesRequest:
		return r.GetDeviceId(), "", r.GetDeviceInfo()
	case *bridgev1.DeleteBridgeRequest:
		return r.GetDeviceId(), r.GetName(), r.GetDeviceInfo()
	default:
		return "", "", nil
	}
}
