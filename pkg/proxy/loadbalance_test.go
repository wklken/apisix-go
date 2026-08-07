package proxy

import "testing"

func TestNewWeightedRRLoadBalanceFirstPickIsDeterministic(t *testing.T) {
	servers := map[string]int{
		"traffic-split-0-1": 2,
		"traffic-split-0-0": 2,
		"traffic-split-0-2": 1,
	}

	first := ""
	for iteration := range 50 {
		lb := NewWeightedRRLoadBalance(servers)
		got := lb.Next()
		if first == "" {
			first = got
		}
		if got != first {
			t.Fatalf("iteration %d: Next() = %q, want stable first pick %q", iteration, got, first)
		}
	}
	if first != "traffic-split-0-0" {
		t.Fatalf("first pick = %q, want traffic-split-0-0 (sorted key order)", first)
	}
}
