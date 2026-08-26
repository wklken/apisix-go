package proxy

import observercontracts "github.com/wklken/apisix-go/pkg/proxy/observer"

// ClusterObserver keeps the existing proxy API while the shared contract
// remains dependency-free.
type ClusterObserver = observercontracts.ClusterObserver

// UpstreamStatusObserver is kept as an alias for the optional health extension.
type UpstreamStatusObserver = observercontracts.UpstreamStatusObserver

// NopClusterObserver discards every cluster runtime signal. It is the default
// observer used when no external metrics runtime is configured.
type NopClusterObserver struct{}

func (NopClusterObserver) SetInFlight(string, int)        {}
func (NopClusterObserver) ObserveRetry(string, string)    {}
func (NopClusterObserver) SetHealth(string, string, bool) {}
func (NopClusterObserver) ObserveRejected(string)         {}
func (NopClusterObserver) DeleteCluster(string)           {}
