// Package dnsx resolves endpoint hostnames and tracks the addresses seen over
// time.
//
// AWS service endpoints answer with a rotating subset of a much larger address
// pool, so a single lookup is not a complete picture. The tracker therefore
// keeps the union of everything seen within a TTL window and expires addresses
// that stop being returned.
package dnsx

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/tapitalee/prefix_updater/internal/endpoints"
)

// Lookup resolves a hostname to addresses.
type Lookup func(ctx context.Context, host string) ([]netip.Addr, error)

// SystemLookup resolves through the system resolver.
func SystemLookup(ctx context.Context, host string) ([]netip.Addr, error) {
	var r net.Resolver
	addrs, err := r.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.Unmap())
	}
	return out, nil
}

// HostResult is the outcome of resolving one hostname.
type HostResult struct {
	Host  endpoints.Host
	Addrs []netip.Addr
	Err   error
}

// ResolveAll resolves every host concurrently. Failures are reported per host
// rather than aborting the round, because losing one endpoint's answer must not
// discard the answers of the others.
func ResolveAll(ctx context.Context, lookup Lookup, hosts []endpoints.Host) []HostResult {
	if lookup == nil {
		lookup = SystemLookup
	}

	results := make([]HostResult, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func(i int, h endpoints.Host) {
			defer wg.Done()
			defer func() {
				// A panic inside the resolver must not take the process down.
				if r := recover(); r != nil {
					results[i] = HostResult{Host: h, Err: fmt.Errorf("panic resolving %s: %v", h.Name, r)}
				}
			}()
			addrs, err := lookup(ctx, h.Name)
			if err == nil && len(addrs) == 0 {
				err = fmt.Errorf("no addresses returned for %s", h.Name)
			}
			results[i] = HostResult{Host: h, Addrs: addrs, Err: err}
		}(i, h)
	}
	wg.Wait()
	return results
}

// Tracker remembers when each address was last seen for each host.
type Tracker struct {
	ttl time.Duration

	mu   sync.Mutex
	seen map[netip.Addr]*addrState
}

type addrState struct {
	lastSeen time.Time
	services map[string]struct{}
}

// NewTracker returns a tracker with the given retention window. A ttl of 0
// keeps only the addresses from the most recent observation.
func NewTracker(ttl time.Duration) *Tracker {
	return &Tracker{ttl: ttl, seen: make(map[netip.Addr]*addrState)}
}

// Observe records the addresses returned for a service at time now.
func (t *Tracker) Observe(service string, addrs []netip.Addr, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for _, a := range addrs {
		a = a.Unmap()
		if !a.IsValid() {
			continue
		}
		st, ok := t.seen[a]
		if !ok {
			st = &addrState{services: make(map[string]struct{})}
			t.seen[a] = st
		}
		if now.After(st.lastSeen) {
			st.lastSeen = now
		}
		st.services[service] = struct{}{}
	}
}

// TrackedAddr is a live address plus the services that resolved to it.
type TrackedAddr struct {
	Addr     netip.Addr
	Services []string
	LastSeen time.Time
}

// Live returns the addresses last seen within the TTL window and drops the
// ones that expired.
func (t *Tracker) Live(now time.Time) []TrackedAddr {
	t.mu.Lock()
	defer t.mu.Unlock()

	cutoff := now.Add(-t.ttl)
	out := make([]TrackedAddr, 0, len(t.seen))
	for addr, st := range t.seen {
		if st.lastSeen.Before(cutoff) {
			delete(t.seen, addr)
			continue
		}
		services := make([]string, 0, len(st.services))
		for svc := range st.services {
			services = append(services, svc)
		}
		sort.Strings(services)
		out = append(out, TrackedAddr{Addr: addr, Services: services, LastSeen: st.lastSeen})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr.Less(out[j].Addr) })
	return out
}

// Len returns the number of tracked addresses.
func (t *Tracker) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.seen)
}
