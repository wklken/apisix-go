package prometheus

import (
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
)

// BenchmarkVerifiedHotPath measures the metrics scrape endpoint: handler
// construction plus the gather/serve path.
func BenchmarkVerifiedHotPath(b *testing.B) {
	request := httptest.NewRequest("GET", MetricsURI, nil)

	b.Run("scrape-handler", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			recorder := httptest.NewRecorder()
			MetricsHandler(recorder, request)
			if recorder.Body.Len() == 0 {
				b.Fatal("empty scrape response")
			}
		}
	})
}

// BenchmarkSnapshotMetricsFinalizer measures the detached request-metrics log
// callback with one immutable snapshot. Reusing the snapshot is intentional:
// the callback must only read detached data and must not rebuild a live request
// or mutate the snapshot between iterations.
func BenchmarkSnapshotMetricsFinalizer(b *testing.B) {
	if err := metrics.Init(nil); err != nil {
		b.Fatal(err)
	}
	p := &Plugin{}
	p.SetResourceContext(
		resource.Route{ID: "benchmark-route"},
		resource.Service{ID: "benchmark-service"},
	)
	snapshot := benchmarkMetricsSnapshot()

	b.Run("default", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if err := p.RunLogPhase(snapshot); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func benchmarkMetricsSnapshot() base.LogSnapshot {
	return base.LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{
			Method:        "GET",
			URI:           "/bench?item=1",
			URL:           "http://gateway.test/bench?item=1",
			Host:          "gateway.test",
			Proto:         "HTTP/1.1",
			ContentLength: 128,
			APISIXVars: map[string]any{
				"$route_id":     "benchmark-route",
				"$service_id":   "benchmark-service",
				"$matched_uri":  "/bench",
				"$matched_host": "gateway.test",
				"$balancer_ip":  "192.0.2.10",
			},
			RequestVars: map[string]any{
				"$upstream_latency": int64(2),
				"$request_type":     "http",
			},
			Consumer: apisixlog.SafeConsumerLogIdentity{Username: "benchmark-consumer"},
		},
		Outcome: apisixctx.ResponseOutcome{
			Kind:      apisixctx.RequestOutcomeCompleted,
			Status:    200,
			Bytes:     256,
			Committed: true,
		},
		Source:   apisixctx.ResponseSourceUpstream,
		Started:  time.Unix(1_700_000_000, 0),
		Finished: time.Unix(1_700_000_000, int64(3*time.Millisecond)),
	}
}
