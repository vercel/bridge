package conntrack

import (
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/vercel-eddie/bridge/pkg/ippool"

	"github.com/puzpuzpuz/xsync/v3"
)

const (
	// UnusedEntryTimeout is how long to wait before releasing an IP that was
	// allocated via DNS but never used for a connection (e.g., dig commands)
	UnusedEntryTimeout = 10 * time.Second

	// cleanupInterval is how often to check for stale entries
	cleanupInterval = 5 * time.Second
)

// Entry represents a tracked connection mapping.
type Entry struct {
	Hostname    string    // Original hostname from DNS query
	ResolvedIP  net.IP    // Real IP address the hostname resolved to
	ProxyIP     net.IP    // Allocated IP from the pool that we returned to the client
	AllocatedAt time.Time // When this entry was created (DNS query time)
	UsedAt      time.Time // When this entry was first used for a connection (zero if never used)
}

// Registry tracks mappings between allocated proxy IPs and their real destinations.
type Registry struct {
	pool     *ippool.Pool
	entries  *xsync.MapOf[string, *Entry] // keyed by proxy IP string
	stopCh   chan struct{}
	stopOnce sync.Once
}

// New creates a new connection tracking registry with the given IP pool.
// Starts a background cleanup goroutine to release unused entries.
func New(pool *ippool.Pool) *Registry {
	r := &Registry{
		pool:    pool,
		entries: xsync.NewMapOf[string, *Entry](),
		stopCh:  make(chan struct{}),
	}
	go r.cleanupLoop()
	return r
}

// Register allocates a proxy IP and registers the mapping.
// Returns the allocated proxy IP.
func (r *Registry) Register(hostname string, resolvedIP net.IP) (net.IP, error) {
	proxyIP, err := r.pool.Allocate()
	if err != nil {
		return nil, err
	}

	entry := &Entry{
		Hostname:    hostname,
		ResolvedIP:  resolvedIP,
		ProxyIP:     proxyIP,
		AllocatedAt: time.Now(),
		// UsedAt remains zero until LookupAndMark is called
	}

	r.entries.Store(proxyIP.String(), entry)

	return proxyIP, nil
}

// Lookup finds the entry for a given proxy IP.
func (r *Registry) Lookup(proxyIP net.IP) *Entry {
	entry, _ := r.entries.Load(proxyIP.String())
	return entry
}

// LookupAndMark finds the entry for a given proxy IP and marks it as used.
// This should be called when an actual connection is established to prevent
// the entry from being cleaned up by the timeout mechanism.
func (r *Registry) LookupAndMark(proxyIP net.IP) *Entry {
	entry, ok := r.entries.Load(proxyIP.String())
	if !ok {
		return nil
	}
	if entry.UsedAt.IsZero() {
		entry.UsedAt = time.Now()
	}
	return entry
}

// Release removes the mapping and returns the IP to the pool.
func (r *Registry) Release(proxyIP net.IP) {
	r.entries.Delete(proxyIP.String())
	r.pool.Release(proxyIP)
}

// Pool returns the underlying IP pool.
func (r *Registry) Pool() *ippool.Pool {
	return r.pool
}

// Stop stops the background cleanup goroutine.
func (r *Registry) Stop() {
	r.stopOnce.Do(func() {
		close(r.stopCh)
	})
}

// cleanupLoop periodically checks for and releases unused entries.
func (r *Registry) cleanupLoop() {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.stopCh:
			return
		case <-ticker.C:
			r.cleanupUnused()
		}
	}
}

// cleanupUnused releases entries that were allocated but never used within the timeout.
func (r *Registry) cleanupUnused() {
	now := time.Now()
	var toRelease []net.IP

	r.entries.Range(func(key string, entry *Entry) bool {
		// Only cleanup entries that were never used (UsedAt is zero)
		// and have exceeded the timeout since allocation
		if entry.UsedAt.IsZero() && now.Sub(entry.AllocatedAt) > UnusedEntryTimeout {
			toRelease = append(toRelease, entry.ProxyIP)
		}
		return true
	})

	// Release stale entries
	for _, ip := range toRelease {
		slog.Debug("Releasing unused DNS allocation",
			"proxy_ip", ip,
			"reason", "timeout without connection",
		)
		r.Release(ip)
	}

	if len(toRelease) > 0 {
		slog.Info("Cleaned up unused DNS allocations", "count", len(toRelease))
	}
}
