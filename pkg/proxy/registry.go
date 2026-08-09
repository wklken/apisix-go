package proxy

import (
	"errors"
	"sync"
)

// ErrClusterRegistryClosed is returned by Acquire after Close.
var ErrClusterRegistryClosed = errors.New("cluster registry is closed")

// ClusterLease is a reference-counted handle to a shared cluster. Stop is
// idempotent; the final Stop across all leases for one cluster closes it.
type ClusterLease struct {
	cluster *Cluster
	release func()
	once    sync.Once
}

// Cluster returns the leased cluster.
func (l *ClusterLease) Cluster() *Cluster {
	return l.cluster
}

// Stop releases one reference to the cluster. The cluster is closed when the
// last reference is released.
func (l *ClusterLease) Stop() {
	l.once.Do(l.release)
}

type clusterEntry struct {
	cluster *Cluster
	refs    int
}

// ClusterRegistry interns immutable ClusterConfig values by digest. Acquire
// reuses an existing cluster when the effective configuration is
// byte-identical; changed configuration always receives a new cluster. Close
// is terminal and closes every remaining cluster.
type ClusterRegistry struct {
	mu       sync.Mutex
	entries  map[ClusterKey]*clusterEntry
	observer ClusterObserver
	closed   bool
}

// NewClusterRegistry creates a digest-keyed registry owned by the given
// observer. The observer is shared by every cluster the registry creates.
func NewClusterRegistry(observer ClusterObserver) *ClusterRegistry {
	return &ClusterRegistry{
		entries:  make(map[ClusterKey]*clusterEntry),
		observer: observer,
	}
}

// Acquire returns a lease for the cluster matching the effective config,
// creating it on first use. The caller must Stop the lease when the route
// generation that owns it is retired.
func (r *ClusterRegistry) Acquire(config ClusterConfig) (*ClusterLease, error) {
	key, err := config.Key()
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrClusterRegistryClosed
	}
	if entry, ok := r.entries[key]; ok {
		entry.refs++
		return &ClusterLease{cluster: entry.cluster, release: r.release(key)}, nil
	}
	cluster, err := newCluster(config, r.observer)
	if err != nil {
		return nil, err
	}
	r.entries[key] = &clusterEntry{cluster: cluster, refs: 1}
	return &ClusterLease{cluster: cluster, release: r.release(key)}, nil
}

func (r *ClusterRegistry) release(key ClusterKey) func() {
	return func() {
		r.mu.Lock()
		entry, ok := r.entries[key]
		if !ok {
			r.mu.Unlock()
			return
		}
		entry.refs--
		if entry.refs > 0 {
			r.mu.Unlock()
			return
		}
		delete(r.entries, key)
		r.mu.Unlock()
		entry.cluster.Close()
	}
}

// Len returns the number of distinct clusters currently held by the registry.
// It exists for lifecycle tests.
func (r *ClusterRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// Close removes and closes every remaining cluster. It is terminal: later
// Acquire calls fail and later lease releases are no-ops. Close has no return
// value because closing a cluster cannot fail.
func (r *ClusterRegistry) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	entries := make([]*clusterEntry, 0, len(r.entries))
	for _, entry := range r.entries {
		entries = append(entries, entry)
	}
	r.entries = make(map[ClusterKey]*clusterEntry)
	r.mu.Unlock()
	for _, entry := range entries {
		entry.cluster.Close()
	}
}
