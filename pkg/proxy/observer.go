package proxy

// ClusterObserver receives narrow runtime signals about an upstream cluster.
// Every method is synchronous and must not block the request path. The
// observer is owned by the cluster registry and shared by every cluster it
// creates; the proxy package never depends on a concrete metrics library.
type ClusterObserver interface {
	SetInFlight(cluster string, delta int)
	ObserveRetry(cluster, result string)
	SetHealth(cluster, target string, healthy bool)
	ObserveRejected(cluster string)
	DeleteCluster(cluster string)
}

// NopClusterObserver discards every cluster runtime signal. It is the default
// observer used when no external metrics runtime is configured.
type NopClusterObserver struct{}

func (NopClusterObserver) SetInFlight(string, int)        {}
func (NopClusterObserver) ObserveRetry(string, string)    {}
func (NopClusterObserver) SetHealth(string, string, bool) {}
func (NopClusterObserver) ObserveRejected(string)         {}
func (NopClusterObserver) DeleteCluster(string)           {}
