package ippool

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
)

// Pool manages a pool of IP addresses from a CIDR block.
type Pool struct {
	network   *net.IPNet
	baseIP    uint32
	size      uint32
	allocated sync.Map // map[uint32]bool
	nextIndex atomic.Uint32
}

// New creates a new IP pool from a CIDR block (e.g., "10.128.0.0/16").
func New(cidr string) (*Pool, error) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("invalid CIDR: %w", err)
	}

	// Only support IPv4 for now
	ip4 := network.IP.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("only IPv4 CIDR blocks are supported")
	}

	baseIP := binary.BigEndian.Uint32(ip4)
	ones, bits := network.Mask.Size()
	size := uint32(1 << (bits - ones))

	p := &Pool{
		network: network,
		baseIP:  baseIP,
		size:    size,
	}
	p.nextIndex.Store(1) // Start from 1 to skip the network address

	return p, nil
}

// Allocate reserves an IP address from the pool.
// Returns the allocated IP or an error if the pool is exhausted.
func (p *Pool) Allocate() (net.IP, error) {
	// Try to find an available slot starting from nextIndex
	startIdx := p.nextIndex.Load()
	maxAttempts := p.size - 2 // Exclude network and broadcast addresses

	for attempts := uint32(0); attempts < maxAttempts; attempts++ {
		idx := ((startIdx + attempts - 1) % (p.size - 2)) + 1

		// Try to claim this index
		if _, loaded := p.allocated.LoadOrStore(idx, true); !loaded {
			// Successfully allocated
			p.nextIndex.Store(idx + 1)
			ip := make(net.IP, 4)
			binary.BigEndian.PutUint32(ip, p.baseIP+idx)
			return ip, nil
		}
	}

	return nil, fmt.Errorf("IP pool exhausted")
}

// Release returns an IP address to the pool.
func (p *Pool) Release(ip net.IP) {
	ip4 := ip.To4()
	if ip4 == nil {
		return
	}

	ipInt := binary.BigEndian.Uint32(ip4)
	offset := ipInt - p.baseIP

	p.allocated.Delete(offset)
}

// Contains checks if an IP is within this pool's CIDR block.
func (p *Pool) Contains(ip net.IP) bool {
	return p.network.Contains(ip)
}

// CIDR returns the CIDR string for this pool.
func (p *Pool) CIDR() string {
	return p.network.String()
}
