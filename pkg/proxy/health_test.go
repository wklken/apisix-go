package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

type recordingHealthReporter struct {
	target string
	status int
	tcp    bool
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

func TestHealthAwareLoadBalanceQuarantinesTCPFailures(t *testing.T) {
	lb, err := NewHealthAwareLoadBalance(
		map[string]int{"http://one.example:80": 1, "http://two.example:80": 1},
		map[string]interface{}{
			"passive": map[string]interface{}{
				"unhealthy": map[string]interface{}{"tcp_failures": 1},
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
		map[string]interface{}{
			"passive": map[string]interface{}{
				"unhealthy": map[string]interface{}{
					"http_statuses": []interface{}{500},
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

func TestHealthAwareLoadBalanceQuarantinesTimeouts(t *testing.T) {
	lb, err := NewHealthAwareLoadBalance(
		map[string]int{"http://one.example:80": 1, "http://two.example:80": 1},
		map[string]interface{}{
			"passive": map[string]interface{}{
				"unhealthy": map[string]interface{}{"timeouts": 1},
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
		map[string]interface{}{
			"passive": map[string]interface{}{
				"unhealthy": map[string]interface{}{"tcp_failures": 1},
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
		map[string]interface{}{
			"passive": map[string]interface{}{
				"unhealthy": map[string]interface{}{"tcp_failures": "one"},
			},
		},
	)
	if err == nil {
		t.Fatal("NewHealthAwareLoadBalance() error = nil, want malformed check rejection")
	}
}
