// Package k8spf implements a gRPC custom resolver and dialer for the k8spf:///
// scheme. It transparently handles Kubernetes config resolution, SPDY connection
// pooling, and port-forward stream creation so callers can use a self-describing
// target address like k8spf:///pod.namespace:port.
package k8spf

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/vercel/bridge/pkg/k8s/kube"
	"google.golang.org/grpc"
	"google.golang.org/grpc/resolver"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const Scheme = "k8spf"

// BuilderConfig holds optional defaults for the Builder.
type BuilderConfig struct {
	Context   string // default kubectl context override
	Namespace string // default kubeconfig namespace override
}

// Builder implements resolver.Builder and owns the SPDY dialer pool.
// It is designed to be passed to both grpc.WithResolvers and grpc.WithContextDialer.
type Builder struct {
	cfg  BuilderConfig
	pool *dialerPool

	mu         sync.Mutex
	restConfig *rest.Config
	clientset  kubernetes.Interface
}

// NewBuilder creates a Builder with the given config.
func NewBuilder(cfg BuilderConfig) *Builder {
	return &Builder{
		cfg:  cfg,
		pool: newDialerPool(),
	}
}

// Scheme returns "k8spf".
func (b *Builder) Scheme() string { return Scheme }

// Build is called by gRPC when a client is created with a k8spf:/// target.
// It parses the target, lazily resolves k8s config, and pushes the address
// to gRPC's connection manager.
func (b *Builder) Build(target resolver.Target, cc resolver.ClientConn, _ resolver.BuildOptions) (resolver.Resolver, error) {
	t, err := ParseTarget(target)
	if err != nil {
		return nil, err
	}

	// Ensure k8s config is resolved (uses target's context if set, else builder default).
	kubectx := t.Context
	if kubectx == "" {
		kubectx = b.cfg.Context
	}
	if _, _, err := b.resolveK8s(kubectx); err != nil {
		return nil, fmt.Errorf("k8spf: resolve k8s config: %w", err)
	}

	// Push the original address (with query params like ?workload=deployment)
	// so DialContext can re-resolve on each reconnect.
	if err := cc.UpdateState(resolver.State{
		Addresses: []resolver.Address{{Addr: t.String()}},
	}); err != nil {
		return nil, fmt.Errorf("k8spf: update resolver state: %w", err)
	}

	return &staticResolver{}, nil
}

// DialContext creates a net.Conn to the pod indicated by addr (format:
// "pod.namespace:port"). If the address does not look like a k8spf target
// (i.e. ParseAddr fails), it falls back to a standard TCP dial so the builder
// can be used as a universal context dialer for both k8spf and plain addresses.
func (b *Builder) DialContext(ctx context.Context, addr string) (net.Conn, error) {
	t, err := ParseAddr(addr)
	if err != nil {
		// Not a k8spf address — fall back to plain TCP.
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}

	kubectx := t.Context
	if kubectx == "" {
		kubectx = b.cfg.Context
	}
	rc, cs, err := b.resolveK8s(kubectx)
	if err != nil {
		return nil, fmt.Errorf("k8spf: resolve k8s config: %w", err)
	}

	// Resolve deployment → pod name if needed.
	podName, err := b.resolvePod(ctx, t, cs)
	if err != nil {
		return nil, err
	}

	// Create a resolved target with the actual pod name for the pool key.
	resolved := Target{
		Name:      podName,
		Namespace: t.Namespace,
		Port:      t.Port,
		Workload:  "pod",
	}

	d, err := b.pool.Get(resolved, rc, cs)
	if err != nil {
		return nil, err
	}

	return d.DialContext(ctx, addr)
}

// DialOptions returns the gRPC dial options needed to use this builder as both
// the resolver and context dialer. Pass these to grpc.NewClient alongside any
// other options (e.g. transport credentials).
func (b *Builder) DialOptions() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithResolvers(b),
		grpc.WithContextDialer(b.DialContext),
	}
}

// Close releases all pooled SPDY connections.
func (b *Builder) Close() error {
	b.pool.Close()
	return nil
}

// resolvePod returns the pod name to port-forward to. For pod workloads it
// returns the name directly. For deployment workloads it delegates to
// kube.ResolveDeploymentPod.
func (b *Builder) resolvePod(ctx context.Context, t Target, cs kubernetes.Interface) (string, error) {
	if t.Workload == "pod" {
		return t.Name, nil
	}
	return kube.GetFirstPodForDeployment(ctx, cs, t.Namespace, t.Name)
}

// resolveK8s lazily creates and caches the rest.Config and clientset.
func (b *Builder) resolveK8s(kubectx string) (*rest.Config, kubernetes.Interface, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.restConfig != nil {
		return b.restConfig, b.clientset, nil
	}

	rc, err := kube.RestConfig(kube.Config{
		Context:   kubectx,
		Namespace: b.cfg.Namespace,
	})
	if err != nil {
		return nil, nil, err
	}

	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, nil, err
	}

	b.restConfig = rc
	b.clientset = cs
	return rc, cs, nil
}

// staticResolver is a no-op resolver returned by Build. The address is resolved
// once at build time and never changes.
type staticResolver struct{}

func (*staticResolver) ResolveNow(resolver.ResolveNowOptions) {}
func (*staticResolver) Close()                                {}
