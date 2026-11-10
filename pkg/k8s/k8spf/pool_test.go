package k8spf

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type fakePodDialer struct {
	closed     bool
	closeCalls int
}

func (f *fakePodDialer) DialContext(context.Context, string) (net.Conn, error) { return nil, nil }
func (f *fakePodDialer) Closed() bool                                          { return f.closed }
func (f *fakePodDialer) Close() error {
	f.closeCalls++
	f.closed = true
	return nil
}

// newFakePool returns a pool whose dialers are fakes, and the fakes it made in
// order.
func newFakePool() (*dialerPool, *[]*fakePodDialer) {
	made := &[]*fakePodDialer{}
	p := newDialerPool()
	p.newDialer = func(*rest.Config, kubernetes.Interface, string, string, int) (podDialer, error) {
		d := &fakePodDialer{}
		*made = append(*made, d)
		return d, nil
	}
	return p, made
}

var testTarget = Target{Name: "bridge-proxy-abc", Namespace: "default", Port: 8080, Workload: "pod"}

func TestPoolReusesAnOpenDialer(t *testing.T) {
	p, made := newFakePool()

	first, err := p.Get(testTarget, nil, nil)
	require.NoError(t, err)
	second, err := p.Get(testTarget, nil, nil)
	require.NoError(t, err)

	assert.Same(t, first, second)
	assert.Len(t, *made, 1, "an open SPDY connection is shared, not redialed")
}

// After the API server or kubelet drops the SPDY connection, gRPC redials
// through the pool. Handing back the dead dialer would fail every redial for
// as long as the pod lives.
func TestPoolReplacesADialerWhoseConnectionClosed(t *testing.T) {
	p, made := newFakePool()

	first, err := p.Get(testTarget, nil, nil)
	require.NoError(t, err)
	(*made)[0].closed = true

	second, err := p.Get(testTarget, nil, nil)
	require.NoError(t, err)

	require.Len(t, *made, 2, "a closed connection must be replaced")
	assert.NotSame(t, first, second)
	assert.False(t, second.Closed())
	assert.Equal(t, 1, (*made)[0].closeCalls, "the dead dialer is closed")

	third, err := p.Get(testTarget, nil, nil)
	require.NoError(t, err)
	assert.Same(t, second, third, "the replacement is what the pool hands out next")
}

func TestPoolEvictKeepsAReplacementAnotherCallerStored(t *testing.T) {
	p, _ := newFakePool()
	dead := &fakePodDialer{closed: true}
	fresh := &fakePodDialer{}
	key := testTarget.String()
	p.dialers.Store(key, fresh)

	p.evict(key, dead)

	got, ok := p.dialers.Load(key)
	require.True(t, ok)
	assert.Same(t, fresh, got)
	assert.Equal(t, 0, fresh.closeCalls)
}
