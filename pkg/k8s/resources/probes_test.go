package resources

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "github.com/vercel/bridge/api/go/bridge/v1" // ensure generated types load
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func ptrString(s string) *string { return &s }

func TestConvertProbe_HTTPGetWithNumericPort(t *testing.T) {
	src := &corev1.Probe{
		InitialDelaySeconds: 5,
		PeriodSeconds:       10,
		TimeoutSeconds:      3,
		SuccessThreshold:    1,
		FailureThreshold:    2,
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path:   "/healthz",
				Port:   intstr.FromInt(8080),
				Scheme: corev1.URISchemeHTTPS,
				HTTPHeaders: []corev1.HTTPHeader{
					{Name: "X-Source", Value: "bridge"},
				},
			},
		},
	}

	got := convertProbe(src, nil)
	require.NotNil(t, got)
	assert.Equal(t, int32(5), got.GetInitialDelaySeconds())
	assert.Equal(t, int32(10), got.GetPeriodSeconds())
	assert.Equal(t, int32(2), got.GetFailureThreshold())

	h := got.GetHttpGet()
	require.NotNil(t, h)
	assert.Equal(t, "/healthz", h.GetPath())
	assert.Equal(t, int32(8080), h.GetPort())
	assert.Equal(t, "HTTPS", h.GetScheme())
	require.Len(t, h.GetHttpHeaders(), 1)
	assert.Equal(t, "X-Source", h.GetHttpHeaders()[0].GetName())
	assert.Equal(t, "bridge", h.GetHttpHeaders()[0].GetValue())
}

func TestConvertProbe_HTTPGetWithNamedPort(t *testing.T) {
	src := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Path: "/",
				Port: intstr.FromString("http"),
			},
		},
	}
	ports := []corev1.ContainerPort{
		{Name: "http", ContainerPort: 3000},
		{Name: "metrics", ContainerPort: 9090},
	}

	got := convertProbe(src, ports)
	require.NotNil(t, got)
	assert.Equal(t, int32(3000), got.GetHttpGet().GetPort())
}

func TestConvertProbe_UnresolvableNamedPortReturnsNil(t *testing.T) {
	src := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromString("nope")},
		},
	}
	assert.Nil(t, convertProbe(src, nil))
}

func TestConvertProbe_TCPSocket(t *testing.T) {
	src := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(5432)},
		},
	}
	got := convertProbe(src, nil)
	require.NotNil(t, got)
	require.NotNil(t, got.GetTcpSocket())
	assert.Equal(t, int32(5432), got.GetTcpSocket().GetPort())
}

func TestConvertProbe_GRPC(t *testing.T) {
	src := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			GRPC: &corev1.GRPCAction{Port: 9090, Service: ptrString("my.Service")},
		},
	}
	got := convertProbe(src, nil)
	require.NotNil(t, got)
	require.NotNil(t, got.GetGrpc())
	assert.Equal(t, int32(9090), got.GetGrpc().GetPort())
	assert.Equal(t, "my.Service", got.GetGrpc().GetService())
}

func TestConvertProbe_Exec(t *testing.T) {
	src := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: []string{"/bin/sh", "-c", "exit 0"}},
		},
	}
	got := convertProbe(src, nil)
	require.NotNil(t, got)
	require.NotNil(t, got.GetExec())
	assert.Equal(t, []string{"/bin/sh", "-c", "exit 0"}, got.GetExec().GetCommand())
}

func TestConvertProbe_NilInput(t *testing.T) {
	assert.Nil(t, convertProbe(nil, nil))
}

func TestConvertProbe_NoHandler(t *testing.T) {
	assert.Nil(t, convertProbe(&corev1.Probe{}, nil))
}

func TestPrimaryAppPort(t *testing.T) {
	cases := []struct {
		name  string
		ports []corev1.ContainerPort
		want  int32
	}{
		{"empty", nil, 0},
		{"single", []corev1.ContainerPort{{ContainerPort: 8080}}, 8080},
		{"skips-grpc", []corev1.ContainerPort{
			{Name: "grpc", ContainerPort: 9090},
			{Name: "http", ContainerPort: 8080},
		}, 8080},
		{"all-grpc", []corev1.ContainerPort{
			{Name: "grpc", ContainerPort: 9090},
		}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, primaryAppPort(tc.ports))
		})
	}
}

// TestInjectProxyImage_PassesProbesAndSourceAppPort verifies that the source
// container's probes and primary app port flow into the proxy command flags.
func TestInjectProxyImage_PassesProbesAndSourceAppPort(t *testing.T) {
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "app",
						Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{Path: "/live", Port: intstr.FromInt(8080)},
							},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(8080)},
							},
						},
					}},
				},
			},
		},
	}

	injectProxyImage(deploy, "ghcr.io/bridge:test")

	cmd := deploy.Spec.Template.Spec.Containers[0].Command

	flagValue := func(flag string) (string, bool) {
		for i, a := range cmd {
			if a == flag && i+1 < len(cmd) {
				return cmd[i+1], true
			}
		}
		return "", false
	}

	v, ok := flagValue("--source-app-port")
	require.True(t, ok, "expected --source-app-port: %v", cmd)
	assert.Equal(t, "8080", v)

	v, ok = flagValue("--liveness-probe")
	require.True(t, ok, "expected --liveness-probe: %v", cmd)
	assert.Contains(t, v, "http_get")
	assert.Contains(t, v, "/live")

	v, ok = flagValue("--readiness-probe")
	require.True(t, ok)
	assert.Contains(t, v, "tcp_socket")

	_, ok = flagValue("--startup-probe")
	assert.False(t, ok, "startup probe should be absent when source had none")
}
