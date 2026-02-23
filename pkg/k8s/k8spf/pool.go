package k8spf

import (
	"fmt"

	"github.com/puzpuzpuz/xsync/v3"
	"github.com/vercel/bridge/pkg/k8s/portforward"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// dialerPool caches SPDY portforward.Dialer instances by target key so
// multiple gRPC streams to the same pod share one SPDY connection.
type dialerPool struct {
	dialers *xsync.MapOf[string, *portforward.Dialer] // key: "pod.namespace:port"
}

func newDialerPool() *dialerPool {
	return &dialerPool{
		dialers: xsync.NewMapOf[string, *portforward.Dialer](),
	}
}

// Get returns an existing dialer for the target or creates a new one.
func (p *dialerPool) Get(t Target, restConfig *rest.Config, clientset kubernetes.Interface) (*portforward.Dialer, error) {
	key := t.String()

	// Fast path: dialer already exists.
	if d, ok := p.dialers.Load(key); ok {
		return d, nil
	}

	// Slow path: create a new dialer outside the map, then store-or-load.
	d, err := portforward.NewDialer(restConfig, clientset, t.Namespace, t.Name, t.Port)
	if err != nil {
		return nil, fmt.Errorf("k8spf pool: create dialer for %s: %w", key, err)
	}

	actual, loaded := p.dialers.LoadOrStore(key, d)
	if loaded {
		// Another goroutine won the race — close our duplicate.
		d.Close()
	}
	return actual, nil
}

// Close closes all cached dialers.
func (p *dialerPool) Close() {
	p.dialers.Range(func(key string, d *portforward.Dialer) bool {
		d.Close()
		p.dialers.Delete(key)
		return true
	})
}
