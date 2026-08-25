package proxy

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

// NopClusterObserver discards every cluster runtime signal. It is the default
// observer used when no external metrics runtime is configured.
type NopClusterObserver struct{}

func (NopClusterObserver) SetInFlight(string, int)        {}
func (NopClusterObserver) ObserveRetry(string, string)    {}
func (NopClusterObserver) SetHealth(string, string, bool) {}
func (NopClusterObserver) ObserveRejected(string)         {}
func (NopClusterObserver) DeleteCluster(string)           {}
