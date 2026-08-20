package prometheus

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestHandlerPassesThrough(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})

	rec := httptest.NewRecorder()
	p.Handler(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/anything", nil))

	if !called {
		t.Fatal("next handler was not called")
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestRunLogPhaseRecordsRequestMetrics(t *testing.T) {
	oldRequests := metrics.Requests
	oldStatus := metrics.HttpStatus
	oldLatency := metrics.HttpLatency
	oldBandwidth := metrics.Bandwidth
	oldLLMLatency := metrics.LLMLatency
	oldLLMPromptTokens := metrics.LLMPromptTokens
	oldLLMCompletionTokens := metrics.LLMCompletionTokens
	metrics.Requests = promclient.NewGauge(promclient.GaugeOpts{Name: "test_prometheus_requests"})
	metrics.HttpStatus = promclient.NewCounterVec(
		promclient.CounterOpts{Name: "test_prometheus_http_status"},
		[]string{
			"code", "route", "matched_uri", "matched_host", "service", "consumer", "node",
			"request_type", "request_llm_model", "llm_model", "response_source",
		},
	)
	metrics.HttpLatency = promclient.NewHistogramVec(
		promclient.HistogramOpts{Name: "test_prometheus_http_latency"},
		[]string{"type", "route", "service", "consumer", "node", "request_type", "request_llm_model", "llm_model"},
	)
	metrics.Bandwidth = promclient.NewCounterVec(
		promclient.CounterOpts{Name: "test_prometheus_bandwidth"},
		[]string{"type", "route", "service", "consumer", "node", "request_type", "request_llm_model", "llm_model"},
	)
	llmLabels := []string{
		"route_id", "service_id", "consumer", "node", "request_type", "request_llm_model", "llm_model",
	}
	metrics.LLMLatency = promclient.NewHistogramVec(
		promclient.HistogramOpts{Name: "test_prometheus_llm_latency"}, llmLabels,
	)
	metrics.LLMPromptTokens = promclient.NewCounterVec(
		promclient.CounterOpts{Name: "test_prometheus_llm_prompt_tokens"}, llmLabels,
	)
	metrics.LLMCompletionTokens = promclient.NewCounterVec(
		promclient.CounterOpts{Name: "test_prometheus_llm_completion_tokens"}, llmLabels,
	)
	t.Cleanup(func() {
		metrics.Requests = oldRequests
		metrics.HttpStatus = oldStatus
		metrics.HttpLatency = oldLatency
		metrics.Bandwidth = oldBandwidth
		metrics.LLMLatency = oldLLMLatency
		metrics.LLMPromptTokens = oldLLMPromptTokens
		metrics.LLMCompletionTokens = oldLLMCompletionTokens
	})

	request := httptest.NewRequest(http.MethodPost, "http://api.example.com/orders/42", nil)
	request.ContentLength = 7
	request = apisixctx.WithApisixVars(request, map[string]string{
		"$route_id":      "route-1",
		"$matched_uri":   "/orders/:id",
		"$matched_host":  "api.example.com",
		"$service_id":    "service-1",
		"$consumer_name": "alice",
		"$balancer_ip":   "10.0.0.8",
	})
	request = apisixctx.WithRequestVars(request)
	apisixctx.RegisterRequestVar(request, "$upstream_latency", int64(1))
	apisixctx.RegisterRequestVar(request, "$request_type", "ai_chat")
	apisixctx.RegisterRequestVar(request, "$request_llm_model", "gpt-request")
	apisixctx.RegisterRequestVar(request, "$llm_model", "gpt-upstream")
	apisixctx.RegisterRequestVar(request, "$response_source", "request-source")
	started := time.Unix(1, 0)
	snapshot := base.BuildLogSnapshot(
		request,
		base.ResponseCaptureSnapshot{},
		apisixctx.ResponseOutcome{Kind: apisixctx.RequestOutcomeCompleted, Status: http.StatusCreated, Bytes: 5},
		apisixctx.ResponseSourceCacheHit,
		started,
		started.Add(12*time.Millisecond),
	)
	p := &Plugin{config: Config{}}
	p.SetResourceContext(
		resource.Route{ID: "route-1", Name: "route-name"},
		resource.Service{ID: "service-1", Name: "service-name"},
	)
	if err := p.RunLogPhase(snapshot); err != nil {
		t.Fatalf("RunLogPhase() error = %v", err)
	}
	if got := counterValue(t, metrics.HttpStatus.WithLabelValues(
		"201", "route-1", "/orders/:id", "api.example.com", "service-1", "alice", "10.0.0.8",
		"ai_chat", "gpt-request", "gpt-upstream", "cache_hit",
	)); got != 1 {
		t.Fatalf("http status count = %v, want 1", got)
	}
	if got := counterValue(t, metrics.Bandwidth.WithLabelValues(
		"egress", "route-1", "service-1", "alice", "10.0.0.8", "ai_chat", "gpt-request", "gpt-upstream",
	)); got != 5 {
		t.Fatalf("egress bytes = %v, want 5", got)
	}
}

func counterValue(t *testing.T, counter promclient.Counter) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := counter.Write(metric); err != nil {
		t.Fatalf("write counter: %v", err)
	}
	return metric.GetCounter().GetValue()
}
