// Package observer defines the dependency-free observer contracts used by the
// proxy runtime and observability integrations.
package observer

// ClusterObserver receives narrow runtime signals about an upstream cluster.
// Every method is synchronous and must not block the request path. The
// observer is supplied by the compiler worker runtime and may be shared by
// clusters across generation leases; the proxy package never depends on a
// concrete metrics library.
type ClusterObserver interface {
	SetInFlight(cluster string, delta int)
	ObserveRetry(cluster, result string)
	SetHealth(cluster, target string, healthy bool)
	ObserveRejected(cluster string)
	DeleteCluster(cluster string)
}

// UpstreamStatusObserver is an optional extension for observers that expose
// one bounded health value per configured upstream target. Keeping this
// separate preserves the narrow ClusterObserver contract for existing users.
type UpstreamStatusObserver interface {
	SetUpstreamStatus(cluster, target string, healthy bool)
	DeleteUpstreamStatus(cluster, target string)
}
