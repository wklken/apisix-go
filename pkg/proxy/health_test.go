package proxy

import (
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

type recordingHealthReporter struct {
	target string
	status int
	tcp    bool
}

type healthTransition struct {
	cluster string
	target  string
	healthy bool
}

type recordingClusterHealthObserver struct {
	NopClusterObserver
	transitions []healthTransition
}

func (o *recordingClusterHealthObserver) SetHealth(cluster, target string, healthy bool) {
	o.transitions = append(o.transitions, healthTransition{cluster: cluster, target: target, healthy: healthy})
}

func (r *recordingHealthReporter) ReportHTTP(target string, status int) {
	r.target = target
	r.status = status
}

func (r *recordingHealthReporter) ReportTCPFailure(target string, timeout bool) {
	r.target = target
	r.tcp = timeout
}

func TestHealthReporterContextCarriesSelectedTarget(t *testing.T) {
	reporter := &recordingHealthReporter{}
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test", nil)
	req = WithHealthReporter(req, reporter)
	SetSelectedTarget(req, "http://upstream.test:80")
	ReportHTTPOutcome(req, http.StatusBadGateway)
	if reporter.target != "http://upstream.test:80" || reporter.status != http.StatusBadGateway {
		t.Fatalf("HTTP outcome = %#v, want selected target and status", reporter)
	}
	ReportTCPFailureOutcome(req, true)
	if reporter.target != "http://upstream.test:80" || !reporter.tcp {
		t.Fatalf("TCP outcome = %#v, want selected target and timeout", reporter)
	}
}

func TestHealthAwareLoadBalanceActiveProbeRecoversTarget(t *testing.T) {
	lb, err := NewHealthAwareLoadBalance(
		map[string]int{"http://one.example:80": 1, "http://two.example:80": 1},
		map[string]any{"passive": map[string]any{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	target := lb.Next()
	lb.MarkUnhealthy(target)
	if lb.IsHealthy(target) {
		t.Fatal("target remained healthy")
	}
	lb.MarkHealthy(target)
	if !lb.IsHealthy(target) {
		t.Fatal("active success did not recover target")
	}
}

func TestHealthAwareLoadBalanceHealthSnapshotReflectsActiveMarks(t *testing.T) {
	lb, err := NewHealthAwareLoadBalance(
		map[string]int{"http://one.example:80": 1, "http://two.example:80": 1},
		map[string]any{"passive": map[string]any{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	lb.MarkUnhealthy("http://one.example:80")
	snapshot := lb.HealthSnapshot()
	if !snapshot["http://two.example:80"] || snapshot["http://one.example:80"] {
		t.Fatalf("HealthSnapshot() = %v, want two healthy and one quarantined", snapshot)
	}
}

func TestHealthAwareLoadBalanceQuarantinesTCPFailures(t *testing.T) {
	lb, err := NewHealthAwareLoadBalance(
		map[string]int{"http://one.example:80": 1, "http://two.example:80": 1},
		map[string]any{
			"passive": map[string]any{
				"unhealthy": map[string]any{"tcp_failures": 1},
			},
		},
	)
	if err != nil {
		t.Fatalf("NewHealthAwareLoadBalance() error = %v", err)
	}

	failed := lb.Next()
	lb.ReportTCPFailure(failed, false)
	for range 8 {
		if got := lb.Next(); got == failed {
			t.Fatalf("quarantined target %q was selected", failed)
		}
	}
}

func TestHealthAwareLoadBalanceQuarantinesHTTPStatusesAfterThreshold(t *testing.T) {
	lb, err := NewHealthAwareLoadBalance(
		map[string]int{
			"http://one.example:80":   1,
			"http://two.example:80":   1,
			"http://three.example:80": 1,
		},
		map[string]any{
			"passive": map[string]any{
				"unhealthy": map[string]any{
					"http_statuses": []any{500},
					"http_failures": 2,
				},
			},
		},
	)
	if err != nil {
		t.Fatalf("NewHealthAwareLoadBalance() error = %v", err)
	}

	failed := lb.Next()
	lb.ReportHTTP(failed, 500)
	lb.ReportHTTP(failed, 500)
	for range 12 {
		if got := lb.Next(); got == failed {
			t.Fatalf("target %q was selected after the HTTP failure threshold", failed)
		}
	}
}

func TestPassiveHealthTransitionsNotifyClusterObserver(t *testing.T) {
	const target = "http://127.0.0.1:8080"
	tests := []struct {
		name   string
		checks map[string]any
		report func(*HealthAwareLoadBalance)
	}{
		{
			name: "http threshold",
			checks: map[string]any{"passive": map[string]any{
				"unhealthy": map[string]any{"http_failures": 1, "http_statuses": []any{500}},
			}},
			report: func(lb *HealthAwareLoadBalance) { lb.ReportHTTP(target, 500) },
		},
		{
			name: "tcp threshold",
			checks: map[string]any{"passive": map[string]any{
				"type":      "tcp",
				"unhealthy": map[string]any{"tcp_failures": 1},
			}},
			report: func(lb *HealthAwareLoadBalance) { lb.ReportTCPFailure(target, false) },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lb, err := NewHealthAwareLoadBalance(map[string]int{target: 1}, test.checks)
			if err != nil {
				t.Fatalf("NewHealthAwareLoadBalance() error = %v", err)
			}
			observer := &recordingClusterHealthObserver{}
			lb.setObserver("orders", observer)
			test.report(lb)
			want := []healthTransition{{cluster: "orders", target: target, healthy: false}}
			if !reflect.DeepEqual(observer.transitions, want) {
				t.Fatalf("health transitions = %#v, want %#v", observer.transitions, want)
			}
		})
	}
}

func TestHealthAwareLoadBalanceQuarantinesTimeouts(t *testing.T) {
	lb, err := NewHealthAwareLoadBalance(
		map[string]int{"http://one.example:80": 1, "http://two.example:80": 1},
		map[string]any{
			"passive": map[string]any{
				"unhealthy": map[string]any{"timeouts": 1},
			},
		},
	)
	if err != nil {
		t.Fatalf("NewHealthAwareLoadBalance() error = %v", err)
	}

	failed := lb.Next()
	lb.ReportTCPFailure(failed, true)
	for range 8 {
		if got := lb.Next(); got == failed {
			t.Fatalf("timed-out target %q was selected", failed)
		}
	}
}

func TestHealthAwareLoadBalanceFailsOpenWhenAllTargetsAreUnhealthy(t *testing.T) {
	lb, err := NewHealthAwareLoadBalance(
		map[string]int{"http://one.example:80": 1, "http://two.example:80": 1},
		map[string]any{
			"passive": map[string]any{
				"unhealthy": map[string]any{"tcp_failures": 1},
			},
		},
	)
	if err != nil {
		t.Fatalf("NewHealthAwareLoadBalance() error = %v", err)
	}

	first := lb.Next()
	lb.ReportTCPFailure(first, false)
	second := lb.Next()
	lb.ReportTCPFailure(second, false)
	if got := lb.Next(); got != first && got != second {
		t.Fatalf("fail-open target = %q, want one of %q or %q", got, first, second)
	}
}

func TestHealthAwareLoadBalanceRejectsMalformedPassiveChecks(t *testing.T) {
	_, err := NewHealthAwareLoadBalance(
		map[string]int{"http://one.example:80": 1},
		map[string]any{
			"passive": map[string]any{
				"unhealthy": map[string]any{"tcp_failures": "one"},
			},
		},
	)
	if err == nil {
		t.Fatal("NewHealthAwareLoadBalance() error = nil, want malformed check rejection")
	}
}

func TestWithHealthReporterReturnsOriginalRequestWhenDisabled(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test", nil)
	if got := WithHealthReporter(req, nil); got != req {
		t.Fatal("WithHealthReporter() replaced the request for a nil reporter")
	}
}

func TestWithHealthReporterDisabledAllocatesNothing(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://gateway.test", nil)
	allocations := testing.AllocsPerRun(1000, func() {
		if WithHealthReporter(req, nil) != req {
			t.Fatal("WithHealthReporter() changed request identity")
		}
	})
	if allocations != 0 {
		t.Fatalf("disabled WithHealthReporter allocations = %v, want 0", allocations)
	}
}

func TestNewUpstreamLoadBalanceEnablesHealthOnlyWhenConfigured(t *testing.T) {
	servers := map[string]int{"http://one.example:80": 1}
	withoutHealth, err := NewUpstreamLoadBalance(servers, nil)
	if err != nil {
		t.Fatalf("NewUpstreamLoadBalance(nil) error = %v", err)
	}
	if _, ok := withoutHealth.(*HealthAwareLoadBalance); ok {
		t.Fatal("no checks unexpectedly enabled health state")
	}

	withActive, err := NewUpstreamLoadBalance(servers, map[string]any{"active": map[string]any{}})
	if err != nil {
		t.Fatalf("NewUpstreamLoadBalance(active) error = %v", err)
	}
	if _, ok := withActive.(*HealthAwareLoadBalance); !ok {
		t.Fatalf("active checks returned %T, want *HealthAwareLoadBalance so probes can recover targets", withActive)
	}

	withPassive, err := NewUpstreamLoadBalance(servers, map[string]any{"passive": map[string]any{}})
	if err != nil {
		t.Fatalf("NewUpstreamLoadBalance(passive) error = %v", err)
	}
	if _, ok := withPassive.(*HealthAwareLoadBalance); !ok {
		t.Fatalf("passive checks returned %T, want *HealthAwareLoadBalance", withPassive)
	}
}

func TestPassiveHealthConfigDefaultsForNilOrEmptyChecks(t *testing.T) {
	for name, checks := range map[string]map[string]any{
		"nil checks":      nil,
		"empty checks":    {},
		"passive nil":     {"passive": nil},
		"passive missing": {"healthy": map[string]any{}},
	} {
		t.Run(name, func(t *testing.T) {
			config, err := parsePassiveHealthConfig(checks)
			if err != nil {
				t.Fatalf("parsePassiveHealthConfig() error = %v", err)
			}
			want := PassiveHealthConfig{
				Type:              "http",
				HTTPFailures:      5,
				TCPFailures:       2,
				Timeouts:          7,
				HealthyStatuses:   defaultHealthyStatuses(),
				UnhealthyStatuses: map[int]struct{}{429: {}, 500: {}, 503: {}},
			}
			if config.Type != want.Type || config.HTTPFailures != want.HTTPFailures ||
				config.TCPFailures != want.TCPFailures || config.Timeouts != want.Timeouts {
				t.Fatalf("parsePassiveHealthConfig() = %#v, want defaults %#v", config, want)
			}
			if len(config.HealthyStatuses) != len(want.HealthyStatuses) {
				t.Fatalf("healthy statuses = %#v, want %#v", config.HealthyStatuses, want.HealthyStatuses)
			}
			if len(config.UnhealthyStatuses) != len(want.UnhealthyStatuses) {
				t.Fatalf("unhealthy statuses = %#v, want %#v", config.UnhealthyStatuses, want.UnhealthyStatuses)
			}
		})
	}
}

func TestPassiveHealthConfigParsesOfficialNumericShapes(t *testing.T) {
	config, err := parsePassiveHealthConfig(map[string]any{
		"passive": map[string]any{
			"type": "HTTPS",
			"healthy": map[string]any{
				"http_statuses": []int{200, 204},
			},
			"unhealthy": map[string]any{
				"http_statuses": []any{int16(429), float64(500)},
				"http_failures": uint8(3),
				"tcp_failures":  int64(4),
				"timeouts":      uint32(5),
			},
		},
	})
	if err != nil {
		t.Fatalf("parsePassiveHealthConfig() error = %v", err)
	}
	if config.Type != "https" || config.HTTPFailures != 3 || config.TCPFailures != 4 || config.Timeouts != 5 {
		t.Fatalf("parsed passive config = %#v", config)
	}
	for _, status := range []int{200, 204} {
		if _, ok := config.HealthyStatuses[status]; !ok {
			t.Fatalf("healthy status %d missing from %#v", status, config.HealthyStatuses)
		}
	}
	for _, status := range []int{429, 500} {
		if _, ok := config.UnhealthyStatuses[status]; !ok {
			t.Fatalf("unhealthy status %d missing from %#v", status, config.UnhealthyStatuses)
		}
	}
}

func TestPassiveHealthConfigRejectsMalformedFields(t *testing.T) {
	tests := []struct {
		name      string
		passive   any
		wantError string
	}{
		{name: "passive not object", passive: "enabled", wantError: "checks.passive must be an object"},
		{name: "type not string", passive: map[string]any{"type": 1}, wantError: "type must be a string"},
		{name: "unsupported type", passive: map[string]any{"type": "udp"}, wantError: "is unsupported"},
		{name: "healthy not object", passive: map[string]any{"healthy": "yes"}, wantError: "healthy must be an object"},
		{
			name:      "healthy statuses not array",
			passive:   map[string]any{"healthy": map[string]any{"http_statuses": "200"}},
			wantError: "must be an array",
		},
		{
			name:      "status not integer",
			passive:   map[string]any{"healthy": map[string]any{"http_statuses": []any{"200"}}},
			wantError: "must be an integer",
		},
		{
			name:      "status out of HTTP range",
			passive:   map[string]any{"healthy": map[string]any{"http_statuses": []any{99}}},
			wantError: "between 100 and 599",
		},
		{
			name:      "unhealthy not object",
			passive:   map[string]any{"unhealthy": "yes"},
			wantError: "unhealthy must be an object",
		},
		{
			name:      "negative threshold",
			passive:   map[string]any{"unhealthy": map[string]any{"timeouts": -1}},
			wantError: "must be non-negative",
		},
		{
			name:      "fractional threshold",
			passive:   map[string]any{"unhealthy": map[string]any{"timeouts": 1.5}},
			wantError: "must be an integer",
		},
		{
			name:      "overflowing threshold",
			passive:   map[string]any{"unhealthy": map[string]any{"timeouts": uint64(math.MaxUint64)}},
			wantError: "is out of range",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parsePassiveHealthConfig(map[string]any{"passive": test.passive})
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("parsePassiveHealthConfig() error = %v, want containing %q", err, test.wantError)
			}
		})
	}
}

func TestHealthAwareLoadBalanceResetsFailuresAndIgnoresUnknownTargets(t *testing.T) {
	lb, err := NewHealthAwareLoadBalance(
		map[string]int{"http://one.example:80": 1},
		map[string]any{"passive": map[string]any{
			"healthy": map[string]any{"http_statuses": []any{204}},
			"unhealthy": map[string]any{
				"http_statuses": []any{500},
				"http_failures": 2,
			},
		}},
	)
	if err != nil {
		t.Fatalf("NewHealthAwareLoadBalance() error = %v", err)
	}
	target := lb.Next()
	lb.ReportHTTP(target, 500)
	lb.ReportHTTP(target, 204)
	lb.ReportHTTP(target, 500)
	if !lb.IsHealthy(target) {
		t.Fatal("healthy status did not reset the accumulated HTTP failure")
	}
	lb.ReportHTTP("unknown", 500)
	lb.ReportTCPFailure("unknown", false)
	if lb.IsHealthy("unknown") {
		t.Fatal("unknown target was reported as healthy")
	}

	empty, err := NewHealthAwareLoadBalance(nil, map[string]any{"passive": map[string]any{}})
	if err != nil {
		t.Fatalf("NewHealthAwareLoadBalance(empty) error = %v", err)
	}
	if got := empty.Next(); got != "" {
		t.Fatalf("empty Next() = %q, want empty target", got)
	}
}

func TestTCPPassiveHealthIgnoresHTTPOutcomes(t *testing.T) {
	lb, err := NewHealthAwareLoadBalance(
		map[string]int{"tcp://one.example:9000": 1},
		map[string]any{"passive": map[string]any{
			"type": "tcp",
			"unhealthy": map[string]any{
				"http_statuses": []any{500},
				"http_failures": 1,
			},
		}},
	)
	if err != nil {
		t.Fatalf("NewHealthAwareLoadBalance() error = %v", err)
	}
	target := lb.Next()
	lb.ReportHTTP(target, 500)
	if !lb.IsHealthy(target) {
		t.Fatal("HTTP outcome quarantined a TCP passive-health target")
	}
}
