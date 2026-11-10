package k8spf

import (
	"context"
	"fmt"
	"net"

	"github.com/puzpuzpuz/xsync/v3"
	"github.com/vercel/bridge/pkg/k8s/portforward"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// podDialer is the part of portforward.Dialer the pool depends on.
type podDialer interface {
	DialContext(ctx context.Context, addr string) (net.Conn, error)
	Closed() bool
	Close() error
}

type newPodDialerFunc func(restConfig *rest.Config, clientset kubernetes.Interface, namespace, podName string, port int) (podDialer, error)

// dialerPool caches SPDY portforward.Dialer instances by target key so
// multiple gRPC streams to the same pod share one SPDY connection.
type dialerPool struct {
	dialers   *xsync.MapOf[string, podDialer] // key: "pod.namespace:port"
	newDialer newPodDialerFunc
}

func newDialerPool() *dialerPool {
	return &dialerPool{
		dialers: xsync.NewMapOf[string, podDialer](),
		newDialer: func(restConfig *rest.Config, clientset kubernetes.Interface, namespace, podName string, port int) (podDialer, error) {
			d, err := portforward.NewDialer(restConfig, clientset, namespace, podName, port)
			if err != nil {
				return nil, err
			}
			return d, nil
		},
	}
}

// Get returns an open dialer for the target, creating one if there is none or
// the cached one's SPDY connection has closed. gRPC redials through here after
// a dropped connection, so handing back the dead dialer would fail every
// redial for as long as the pod lives.
func (p *dialerPool) Get(t Target, restConfig *rest.Config, clientset kubernetes.Interface) (podDialer, error) {
	key := t.String()

	// Fast path: an open dialer already exists.
	if d, ok := p.dialers.Load(key); ok {
		if !d.Closed() {
			return d, nil
		}
		p.evict(key, d)
	}

	// Slow path: create a new dialer outside the map, then store-or-load.
	d, err := p.newDialer(restConfig, clientset, t.Namespace, t.Name, t.Port)
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

// evict removes dead from the pool unless another goroutine already replaced
// it, and closes it.
func (p *dialerPool) evict(key string, dead podDialer) {
	p.dialers.Compute(key, func(current podDialer, loaded bool) (podDialer, bool) {
		if loaded && current != dead {
			return current, false
		}
		return nil, true
	})
	dead.Close()
}

// Close closes all cached dialers.
func (p *dialerPool) Close() {
	p.dialers.Range(func(key string, d podDialer) bool {
		d.Close()
		p.dialers.Delete(key)
		return true
	})
}
