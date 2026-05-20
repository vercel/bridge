package resources

import (
	"log/slog"

	bridgev1 "github.com/vercel/bridge/api/go/bridge/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"google.golang.org/protobuf/encoding/protojson"
)

// convertProbe converts a corev1.Probe into our bridge.v1.Probe form.
// Named ports are resolved against the container's declared Ports. Returns
// nil if the input is nil or has no recognised handler.
func convertProbe(p *corev1.Probe, ports []corev1.ContainerPort) *bridgev1.Probe {
	if p == nil {
		return nil
	}
	out := &bridgev1.Probe{
		InitialDelaySeconds: p.InitialDelaySeconds,
		PeriodSeconds:       p.PeriodSeconds,
		TimeoutSeconds:      p.TimeoutSeconds,
		SuccessThreshold:    p.SuccessThreshold,
		FailureThreshold:    p.FailureThreshold,
	}

	switch {
	case p.HTTPGet != nil:
		port, ok := resolvePort(p.HTTPGet.Port, ports)
		if !ok {
			slog.Warn("Could not resolve HTTP probe port; skipping probe", "port", p.HTTPGet.Port.String())
			return nil
		}
		headers := make([]*bridgev1.HTTPHeader, 0, len(p.HTTPGet.HTTPHeaders))
		for _, h := range p.HTTPGet.HTTPHeaders {
			headers = append(headers, &bridgev1.HTTPHeader{Name: h.Name, Value: h.Value})
		}
		out.Handler = &bridgev1.Probe_HttpGet{HttpGet: &bridgev1.HTTPGetAction{
			Path:        p.HTTPGet.Path,
			Port:        port,
			Scheme:      string(p.HTTPGet.Scheme),
			HttpHeaders: headers,
		}}

	case p.TCPSocket != nil:
		port, ok := resolvePort(p.TCPSocket.Port, ports)
		if !ok {
			slog.Warn("Could not resolve TCP probe port; skipping probe", "port", p.TCPSocket.Port.String())
			return nil
		}
		out.Handler = &bridgev1.Probe_TcpSocket{TcpSocket: &bridgev1.TCPSocketAction{Port: port}}

	case p.GRPC != nil:
		var service string
		if p.GRPC.Service != nil {
			service = *p.GRPC.Service
		}
		out.Handler = &bridgev1.Probe_Grpc{Grpc: &bridgev1.GRPCAction{
			Port:    p.GRPC.Port,
			Service: service,
		}}

	case p.Exec != nil:
		out.Handler = &bridgev1.Probe_Exec{Exec: &bridgev1.ExecAction{Command: append([]string(nil), p.Exec.Command...)}}

	default:
		// No recognised handler.
		return nil
	}

	return out
}

// resolvePort resolves an intstr.IntOrString port reference to an integer.
// Named ports are looked up in the container's declared Ports list. Returns
// the resolved port and true on success, or 0 and false if unresolvable.
func resolvePort(port intstr.IntOrString, ports []corev1.ContainerPort) (int32, bool) {
	if port.Type == intstr.Int {
		return port.IntVal, true
	}
	for _, p := range ports {
		if p.Name == port.StrVal {
			return p.ContainerPort, true
		}
	}
	return 0, false
}

// primaryAppPort returns the first non-grpc port declared on the container,
// or 0 if none exist. This is the port the bridge intercept remaps to the
// developer's --app-port locally.
func primaryAppPort(ports []corev1.ContainerPort) int32 {
	for _, p := range ports {
		if p.Name != "grpc" {
			return p.ContainerPort
		}
	}
	return 0
}

// appendProbeArg appends `<flag> <json>` to args when probe is non-nil. The
// probe is JSON-encoded via protojson so the bridge server can decode it
// with the same library at startup. A nil probe is a no-op.
func appendProbeArg(args []string, flag string, probe *bridgev1.Probe) []string {
	if probe == nil {
		return args
	}
	data, err := protojson.Marshal(probe)
	if err != nil {
		slog.Warn("Failed to marshal probe; skipping", "flag", flag, "error", err)
		return args
	}
	return append(args, flag, string(data))
}
