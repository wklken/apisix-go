package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const (
	highPriorityTarget = "http://high.example:80"
	lowPriorityTarget  = "http://low.example:80"
	highWeightedTarget = "http://high-weighted.example:80"
	highHealthyTarget  = "http://high-healthy.example:80"
)

func TestPriorityLoadBalanceSelectsHighestPriorityGroup(t *testing.T) {
	lb, err := newUpstreamLoadBalanceWithPriorities(
		map[string]int{lowPriorityTarget: 1, highPriorityTarget: 1},
		map[string]int{lowPriorityTarget: 0, highPriorityTarget: 10},
		nil,
	)
	if err != nil {
		t.Fatalf("newUpstreamLoadBalanceWithPriorities() error = %v", err)
	}
	for range 20 {
		if got := lb.Next(); got != highPriorityTarget {
			t.Fatalf("priority target = %q, want %q", got, highPriorityTarget)
		}
	}
}

func TestPriorityLoadBalanceSkipsZeroWeightHigherPriorityTarget(t *testing.T) {
	lb, err := newUpstreamLoadBalanceWithPriorities(
		map[string]int{highPriorityTarget: 0, lowPriorityTarget: 1},
		map[string]int{highPriorityTarget: 10, lowPriorityTarget: 0},
		nil,
	)
	if err != nil {
		t.Fatalf("newUpstreamLoadBalanceWithPriorities() error = %v", err)
	}
	if got := lb.Next(); got != lowPriorityTarget {
		t.Fatalf("priority target = %q, want enabled lower-priority target %q", got, lowPriorityTarget)
	}
}

func TestPriorityLoadBalanceExhaustsHigherGroupBeforeRetryingLowerPriority(t *testing.T) {
	lb, err := newUpstreamLoadBalanceWithPriorities(
		map[string]int{highPriorityTarget: 1, lowPriorityTarget: 1},
		map[string]int{highPriorityTarget: 10, lowPriorityTarget: 0},
		nil,
	)
	if err != nil {
		t.Fatalf("newUpstreamLoadBalanceWithPriorities() error = %v", err)
	}
	requestAware, ok := lb.(interface {
		NextForRequest(*http.Request) string
	})
	if !ok {
		t.Fatalf("priority load balancer %T has no request-local retry selection", lb)
	}
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil)
	if got := requestAware.NextForRequest(request); got != highPriorityTarget {
		t.Fatalf("initial target = %q, want %q", got, highPriorityTarget)
	}
	if got := requestAware.NextForRequest(request); got != lowPriorityTarget {
		t.Fatalf("retry target = %q, want exhausted high group to fall through", got)
	}
}

func TestPriorityHealthAwareLoadBalanceFallsThroughAfterHigherUnavailable(t *testing.T) {
	lb, err := newUpstreamLoadBalanceWithPriorities(
		map[string]int{lowPriorityTarget: 1, highPriorityTarget: 1},
		map[string]int{lowPriorityTarget: 0, highPriorityTarget: 10},
		map[string]any{"passive": map[string]any{}},
	)
	if err != nil {
		t.Fatalf("newUpstreamLoadBalanceWithPriorities() error = %v", err)
	}
	healthAware, ok := lb.(*HealthAwareLoadBalance)
	if !ok {
		t.Fatalf("priority load balancer = %T, want *HealthAwareLoadBalance", lb)
	}
	if got := healthAware.Next(); got != highPriorityTarget {
		t.Fatalf("healthy priority target = %q, want %q", got, highPriorityTarget)
	}
	healthAware.MarkUnhealthy(highPriorityTarget)
	for range 20 {
		if got := healthAware.Next(); got != lowPriorityTarget {
			t.Fatalf("fallback priority target = %q, want %q", got, lowPriorityTarget)
		}
	}

	healthAware.MarkUnhealthy(lowPriorityTarget)
	got := healthAware.Next()
	if got != highPriorityTarget && got != lowPriorityTarget {
		t.Fatalf("fail-open priority target = %q, want a configured target", got)
	}
}

func TestPriorityHealthAwareLoadBalanceKeepsHealthyPeerSelectable(t *testing.T) {
	lb, err := newUpstreamLoadBalanceWithPriorities(
		map[string]int{
			highWeightedTarget: 100,
			highHealthyTarget:  1,
			lowPriorityTarget:  1,
		},
		map[string]int{
			highWeightedTarget: 10,
			highHealthyTarget:  10,
			lowPriorityTarget:  0,
		},
		map[string]any{"passive": map[string]any{}},
	)
	if err != nil {
		t.Fatalf("newUpstreamLoadBalanceWithPriorities() error = %v", err)
	}
	healthAware := lb.(*HealthAwareLoadBalance)
	healthAware.MarkUnhealthy(highWeightedTarget)
	for range 20 {
		if got := healthAware.Next(); got != highHealthyTarget {
			t.Fatalf("healthy peer target = %q, want %q", got, highHealthyTarget)
		}
	}
}

func TestUpstreamLoadBalanceWithoutMultiplePrioritiesUsesExistingSelector(t *testing.T) {
	servers := map[string]int{lowPriorityTarget: 1, highPriorityTarget: 1}
	withoutPriorities, err := newUpstreamLoadBalanceWithPriorities(servers, nil, nil)
	if err != nil {
		t.Fatalf("newUpstreamLoadBalanceWithPriorities(nil) error = %v", err)
	}
	if _, ok := withoutPriorities.(*priorityLoadBalance); !ok {
		t.Fatalf("without-priority load balancer = %T, want *priorityLoadBalance", withoutPriorities)
	}

	singleGroup, err := newUpstreamLoadBalanceWithPriorities(
		servers,
		map[string]int{lowPriorityTarget: 7, highPriorityTarget: 7},
		nil,
	)
	if err != nil {
		t.Fatalf("newUpstreamLoadBalanceWithPriorities(single group) error = %v", err)
	}
	if _, ok := singleGroup.(*priorityLoadBalance); !ok {
		t.Fatalf("single-priority-group load balancer = %T, want *priorityLoadBalance", singleGroup)
	}
}

func TestSamePriorityLoadBalanceSkipsTriedTargetOnRetry(t *testing.T) {
	lb, err := newUpstreamLoadBalanceWithPriorities(
		map[string]int{lowPriorityTarget: 1, highPriorityTarget: 1},
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("newUpstreamLoadBalanceWithPriorities() error = %v", err)
	}
	requestAware, ok := lb.(interface {
		NextForRequest(*http.Request) string
	})
	if !ok {
		t.Fatalf("same-priority load balancer %T has no request-local retry selection", lb)
	}
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil)
	first := requestAware.NextForRequest(request)
	second := requestAware.NextForRequest(request)
	if first == "" || second == "" {
		t.Fatalf("targets = %q then %q, want two same-priority nodes", first, second)
	}
	if first == second {
		t.Fatalf("retry target = %q, want the remaining same-priority node", second)
	}
}

func TestClusterConfigKeyIncludesUpstreamPriorities(t *testing.T) {
	base := testClusterConfig()
	prioritized := base
	prioritized.Priorities = map[string]int{"http://127.0.0.1:8080": 10}

	baseKey, err := base.Key()
	if err != nil {
		t.Fatalf("base.Key() error = %v", err)
	}
	prioritizedKey, err := prioritized.Key()
	if err != nil {
		t.Fatalf("prioritized.Key() error = %v", err)
	}
	if baseKey == prioritizedKey {
		t.Fatal("upstream priorities did not change the cluster key")
	}
}
