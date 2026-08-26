package observer

import "testing"

type clusterOnlyObserver struct{}

func (*clusterOnlyObserver) SetInFlight(string, int)        {}
func (*clusterOnlyObserver) ObserveRetry(string, string)    {}
func (*clusterOnlyObserver) SetHealth(string, string, bool) {}
func (*clusterOnlyObserver) ObserveRejected(string)         {}
func (*clusterOnlyObserver) DeleteCluster(string)           {}

func TestClusterObserverKeepsUpstreamStatusOptional(t *testing.T) {
	var runtimeObserver ClusterObserver = &clusterOnlyObserver{}
	if _, ok := runtimeObserver.(UpstreamStatusObserver); ok {
		t.Fatal("ClusterObserver unexpectedly requires the upstream-status extension")
	}
}
