package request_context

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestMetricLabelsDefaultUseIDs(t *testing.T) {
	p := &Plugin{
		config: Config{
			RouteID:     "route-1",
			RouteName:   "route-name",
			ServiceID:   "service-1",
			ServiceName: "service-name",
		},
	}

	labels := p.metricLabels()
	if labels.route != "route-1" {
		t.Fatalf("route label = %q, want route-1", labels.route)
	}
	if labels.service != "service-1" {
		t.Fatalf("service label = %q, want service-1", labels.service)
	}
}

func TestMetricLabelsPreferNameUsesNames(t *testing.T) {
	p := &Plugin{
		config: Config{
			RouteID:              "route-1",
			RouteName:            "route-name",
			ServiceID:            "service-1",
			ServiceName:          "service-name",
			PrometheusPreferName: true,
		},
	}

	labels := p.metricLabels()
	if labels.route != "route-name" {
		t.Fatalf("route label = %q, want route-name", labels.route)
	}
	if labels.service != "service-name" {
		t.Fatalf("service label = %q, want service-name", labels.service)
	}
}

func TestMetricLabelsFallbackToNameWhenIDMissing(t *testing.T) {
	p := &Plugin{
		config: Config{
			RouteName:   "route-name",
			ServiceName: "service-name",
		},
	}

	labels := p.metricLabels()
	if labels.route != "route-name" {
		t.Fatalf("route label = %q, want route-name", labels.route)
	}
	if labels.service != "service-name" {
		t.Fatalf("service label = %q, want service-name", labels.service)
	}
}

func TestHandlerRecordsOfficialPrometheusRequestMetrics(t *testing.T) {
	oldRequests := metrics.Requests
	oldStatus := metrics.HttpStatus
	oldLatency := metrics.HttpLatency
	oldBandwidth := metrics.Bandwidth
	oldLLMLatency := metrics.LLMLatency
	oldLLMPromptTokens := metrics.LLMPromptTokens
	oldLLMCompletionTokens := metrics.LLMCompletionTokens
	metrics.Requests = prometheus.NewCounter(prometheus.CounterOpts{Name: "test_prometheus_requests"})
	metrics.HttpStatus = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_prometheus_http_status"},
		[]string{
			"code", "route", "matched_uri", "matched_host", "service", "consumer", "node",
			"request_type", "request_llm_model", "llm_model", "response_source",
		},
	)
	metrics.HttpLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "test_prometheus_http_latency"},
		[]string{"type", "route", "service", "consumer", "node", "request_type", "request_llm_model", "llm_model"},
	)
	metrics.Bandwidth = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_prometheus_bandwidth"},
		[]string{"type", "route", "service", "consumer", "node", "request_type", "request_llm_model", "llm_model"},
	)
	llmLabels := []string{
		"route_id", "service_id", "consumer", "node", "request_type", "request_llm_model", "llm_model",
	}
	metrics.LLMLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "test_prometheus_llm_latency"}, llmLabels,
	)
	metrics.LLMPromptTokens = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_prometheus_llm_prompt_tokens"}, llmLabels,
	)
	metrics.LLMCompletionTokens = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_prometheus_llm_completion_tokens"}, llmLabels,
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

	p := &Plugin{config: Config{
		RouteID:     "route-1",
		MatchedURI:  "/orders/:id",
		MatchedHost: "api.example.com",
		ServiceID:   "service-1",
	}}
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterApisixVar(r, "$consumer_name", "alice")
		apisixctx.RegisterApisixVar(r, "$balancer_ip", "10.0.0.8")
		apisixctx.RegisterRequestVar(r, "$upstream_latency", int64(1))
		apisixctx.RegisterRequestVar(r, "$request_type", "ai_chat")
		apisixctx.RegisterRequestVar(r, "$request_llm_model", "gpt-request")
		apisixctx.RegisterRequestVar(r, "$llm_model", "gpt-upstream")
		apisixctx.RegisterRequestVar(r, "$llm_time_to_first_token", int64(12))
		apisixctx.RegisterRequestVar(r, "$llm_prompt_tokens", int64(23))
		apisixctx.RegisterRequestVar(r, "$llm_completion_tokens", int64(8))
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello"))
	}))
	req := httptest.NewRequest(http.MethodPost, "http://api.example.com/orders/42", nil)
	req.ContentLength = 7
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if got := counterValue(t, metrics.HttpStatus.WithLabelValues(
		"201", "route-1", "/orders/:id", "api.example.com", "service-1", "alice", "10.0.0.8",
		"ai_chat", "gpt-request", "gpt-upstream", "upstream",
	)); got != 1 {
		t.Fatalf("http status count = %v, want 1", got)
	}
	for _, metricType := range []string{"request", "upstream", "apisix"} {
		if got := histogramCount(t, metrics.HttpLatency.WithLabelValues(
			metricType, "route-1", "service-1", "alice", "10.0.0.8", "ai_chat", "gpt-request", "gpt-upstream",
		)); got != 1 {
			t.Fatalf("%s latency count = %d, want 1", metricType, got)
		}
	}
	if got := counterValue(t, metrics.Bandwidth.WithLabelValues(
		"ingress", "route-1", "service-1", "alice", "10.0.0.8", "ai_chat", "gpt-request", "gpt-upstream",
	)); got != 7 {
		t.Fatalf("ingress bandwidth = %v, want 7", got)
	}
	if got := counterValue(t, metrics.Bandwidth.WithLabelValues(
		"egress", "route-1", "service-1", "alice", "10.0.0.8", "ai_chat", "gpt-request", "gpt-upstream",
	)); got != 5 {
		t.Fatalf("egress bandwidth = %v, want 5", got)
	}
	llmLabelValues := []string{
		"route-1", "service-1", "alice", "10.0.0.8", "ai_chat", "gpt-request", "gpt-upstream",
	}
	if got := histogramCount(t, metrics.LLMLatency.WithLabelValues(llmLabelValues...)); got != 1 {
		t.Fatalf("LLM latency count = %d, want 1", got)
	}
	if got := counterValue(t, metrics.LLMPromptTokens.WithLabelValues(llmLabelValues...)); got != 23 {
		t.Fatalf("LLM prompt tokens = %v, want 23", got)
	}
	if got := counterValue(t, metrics.LLMCompletionTokens.WithLabelValues(llmLabelValues...)); got != 8 {
		t.Fatalf("LLM completion tokens = %v, want 8", got)
	}
}

func TestHandlerRecordsMonotonicRequestTotal(t *testing.T) {
	oldRequests := metrics.Requests
	oldStatus := metrics.HttpStatus
	oldLatency := metrics.HttpLatency
	oldBandwidth := metrics.Bandwidth
	oldLLMLatency := metrics.LLMLatency
	oldLLMPromptTokens := metrics.LLMPromptTokens
	oldLLMCompletionTokens := metrics.LLMCompletionTokens
	metrics.Requests = prometheus.NewCounter(prometheus.CounterOpts{Name: "test_monotonic_requests"})
	metrics.HttpStatus = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_monotonic_http_status"},
		[]string{
			"code", "route", "matched_uri", "matched_host", "service", "consumer", "node",
			"request_type", "request_llm_model", "llm_model", "response_source",
		},
	)
	metrics.HttpLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "test_monotonic_http_latency"},
		[]string{"type", "route", "service", "consumer", "node", "request_type", "request_llm_model", "llm_model"},
	)
	metrics.Bandwidth = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_monotonic_bandwidth"},
		[]string{"type", "route", "service", "consumer", "node", "request_type", "request_llm_model", "llm_model"},
	)
	llmLabels := []string{
		"route_id", "service_id", "consumer", "node", "request_type", "request_llm_model", "llm_model",
	}
	metrics.LLMLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "test_monotonic_llm_latency"}, llmLabels,
	)
	metrics.LLMPromptTokens = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_monotonic_llm_prompt_tokens"}, llmLabels,
	)
	metrics.LLMCompletionTokens = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_monotonic_llm_completion_tokens"}, llmLabels,
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

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	p := &Plugin{config: Config{RouteID: "route-1"}}
	handler := p.Handler(next)
	req := httptest.NewRequest(http.MethodGet, "http://api.example.com/orders/42", nil)

	for range 3 {
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}
	if got := counterValue(t, metrics.Requests); got != 3 {
		t.Fatalf("request total = %v, want 3 after three requests", got)
	}
}

func counterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := counter.Write(metric); err != nil {
		t.Fatalf("write counter: %v", err)
	}
	return metric.GetCounter().GetValue()
}

func histogramCount(t *testing.T, observer prometheus.Observer) uint64 {
	t.Helper()
	metric := &dto.Metric{}
	writer, ok := observer.(prometheus.Metric)
	if !ok {
		t.Fatalf("observer %T does not implement prometheus.Metric", observer)
	}
	if err := writer.Write(metric); err != nil {
		t.Fatalf("write histogram: %v", err)
	}
	return metric.GetHistogram().GetSampleCount()
}

func TestHandlerWorksWhenPrometheusCollectorsAreDisabled(t *testing.T) {
	oldRequests := metrics.Requests
	oldStatus := metrics.HttpStatus
	oldLatency := metrics.HttpLatency
	oldBandwidth := metrics.Bandwidth
	oldLLMLatency := metrics.LLMLatency
	oldPrompt := metrics.LLMPromptTokens
	oldCompletion := metrics.LLMCompletionTokens
	metrics.Requests = nil
	metrics.HttpStatus = nil
	metrics.HttpLatency = nil
	metrics.Bandwidth = nil
	metrics.LLMLatency = nil
	metrics.LLMPromptTokens = nil
	metrics.LLMCompletionTokens = nil
	t.Cleanup(func() {
		metrics.Requests = oldRequests
		metrics.HttpStatus = oldStatus
		metrics.HttpLatency = oldLatency
		metrics.Bandwidth = oldBandwidth
		metrics.LLMLatency = oldLLMLatency
		metrics.LLMPromptTokens = oldPrompt
		metrics.LLMCompletionTokens = oldCompletion
	})

	called := false
	handler := (&Plugin{}).Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if apisixctx.GetRequestVars(r) == nil {
			t.Fatal("request variables were not initialized")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil))

	if !called || response.Code != http.StatusNoContent {
		t.Fatalf("downstream called = %v, status = %d", called, response.Code)
	}
}

func TestRequestContextRegistersMetricsAndRecycleFinalizer(t *testing.T) {
	installTestMetrics(t)

	startedAt := time.Now()
	lifecycle := apisixctx.NewRequestLifecycle(startedAt)
	request := apisixctx.WithRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		lifecycle,
	)
	request, _ = apisixctx.EnsureRequestLifecycle(request, startedAt)
	var state *apisixctx.RequestState

	handler := (&Plugin{config: Config{RouteID: "finalizer-route"}}).Handler(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			state = apisixctx.GetRequestState(r)
			w.WriteHeader(http.StatusNoContent)
		}),
	)
	handler.ServeHTTP(httptest.NewRecorder(), request)

	if state == nil || state.ApisixVars == nil || state.RequestVars == nil {
		t.Fatal("request state was recycled before the outer lifecycle finalized")
	}
	if got := counterValue(t, metrics.Requests); got != 0 {
		t.Fatalf("request total before lifecycle finalization = %v, want 0", got)
	}

	lifecycle.SetOutcome(apisixctx.ResponseOutcome{
		Kind:      apisixctx.RequestOutcomeCompleted,
		Status:    http.StatusNoContent,
		Committed: true,
	})
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("lifecycle finalizer failures = %#v, want none", failures)
	}
	if got := counterValue(t, metrics.Requests); got != 1 {
		t.Fatalf("request total after lifecycle finalization = %v, want 1", got)
	}
	if state.ApisixVars == nil || state.RequestVars == nil {
		t.Fatal("request-context finalizer recycled state; outer owner must recycle after finalizers")
	}

	apisixctx.RecycleVars(request)
	if state.ApisixVars != nil || state.RequestVars != nil {
		t.Fatal("outer lifecycle owner did not recycle request state")
	}
}

func TestRequestContextFinalizerRecordsEarlyReturnOutcome(t *testing.T) {
	installTestMetrics(t)

	startedAt := time.Now()
	lifecycle := apisixctx.NewRequestLifecycle(startedAt)
	request := apisixctx.WithRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		lifecycle,
	)
	request, _ = apisixctx.EnsureRequestLifecycle(request, startedAt)

	handler := (&Plugin{config: Config{RouteID: "early-return-route"}}).Handler(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			// The outer owner supplies the final response outcome after this early return.
		}),
	)
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if got := counterValue(t, metrics.Requests); got != 0 {
		t.Fatalf("request total before early-return finalization = %v, want 0", got)
	}

	lifecycle.SetOutcome(apisixctx.ResponseOutcome{
		Kind:      apisixctx.RequestOutcomeCompleted,
		Status:    http.StatusTeapot,
		Bytes:     7,
		Committed: true,
	})
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("lifecycle finalizer failures = %#v, want none", failures)
	}
	if got := counterValue(t, metrics.HttpStatus.WithLabelValues(
		"418", "early-return-route", "", "", "", "", "", "", "", "", "apisix",
	)); got != 1 {
		t.Fatalf("early-return status count = %v, want 1", got)
	}
	if got := counterValue(t, metrics.Bandwidth.WithLabelValues(
		"egress", "early-return-route", "", "", "", "", "", "",
	)); got != 7 {
		t.Fatalf("early-return egress bytes = %v, want 7", got)
	}
	apisixctx.RecycleVars(request)
}

func TestRequestContextFinalizerRunsAfterDownstreamPanic(t *testing.T) {
	installTestMetrics(t)

	startedAt := time.Now()
	lifecycle := apisixctx.NewRequestLifecycle(startedAt)
	request := apisixctx.WithRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		lifecycle,
	)
	request, _ = apisixctx.EnsureRequestLifecycle(request, startedAt)

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		handler := (&Plugin{config: Config{RouteID: "panic-route"}}).Handler(
			http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				panic("downstream panic")
			}),
		)
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}()
	if recovered == nil {
		t.Fatal("downstream panic was not propagated to the outer owner")
	}
	if got := counterValue(t, metrics.Requests); got != 0 {
		t.Fatalf("request total before panic finalization = %v, want 0", got)
	}

	lifecycle.SetOutcome(apisixctx.ResponseOutcome{
		Kind:      apisixctx.RequestOutcomeRecoveredPanic,
		Status:    http.StatusInternalServerError,
		Bytes:     38,
		Committed: true,
	})
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("lifecycle finalizer failures = %#v, want none", failures)
	}
	if got := counterValue(t, metrics.HttpStatus.WithLabelValues(
		"500", "panic-route", "", "", "", "", "", "", "", "", "apisix",
	)); got != 1 {
		t.Fatalf("panic status count = %v, want 1", got)
	}
	if got := counterValue(t, metrics.Requests); got != 1 {
		t.Fatalf("request total after panic finalization = %v, want 1", got)
	}
	apisixctx.RecycleVars(request)
}

func TestRequestContextLegacyDirectHandlerStillFinalizes(t *testing.T) {
	installTestMetrics(t)

	var seen *http.Request
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil)
	handler := (&Plugin{config: Config{RouteID: "legacy-route"}}).Handler(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = r
			w.WriteHeader(http.StatusNoContent)
		}),
	)
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if seen == nil {
		t.Fatal("direct handler did not pass a request downstream")
	}
	if apisixctx.GetRequestLifecycle(seen) == nil {
		t.Fatal("direct handler did not create a local request lifecycle")
	}
	state := apisixctx.GetRequestState(seen)
	if state == nil || state.ApisixVars != nil || state.RequestVars != nil {
		t.Fatal("direct handler did not recycle request state after finalizers")
	}
	if got := counterValue(t, metrics.HttpStatus.WithLabelValues(
		"204", "legacy-route", "", "", "", "", "", "", "", "", "apisix",
	)); got != 1 {
		t.Fatalf("legacy status count = %v, want 1", got)
	}

	var panicRequest *http.Request
	func() {
		defer func() {
			if recover() == nil {
				t.Error("direct downstream panic was not propagated")
			}
		}()
		handler := (&Plugin{}).Handler(
			http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				panicRequest = r
				panic("legacy downstream panic")
			}),
		)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil))
	}()
	if panicRequest == nil {
		t.Fatal("panic path did not pass a request downstream")
	}
	panicState := apisixctx.GetRequestState(panicRequest)
	if panicState == nil || panicState.ApisixVars != nil || panicState.RequestVars != nil {
		t.Fatal("direct panic path leaked request state")
	}
}

func TestRequestContextLegacyPreWritePanicKeepsRequestTotalWithoutSuccessMetrics(t *testing.T) {
	installTestMetrics(t)
	requestTotalBefore := counterValue(t, metrics.Requests)

	var panicRequest *http.Request
	handler := (&Plugin{config: Config{RouteID: "legacy-prewrite-panic-route"}}).Handler(
		http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			panicRequest = r
			panic("legacy pre-write panic")
		}),
	)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("direct pre-write panic was not propagated")
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil))
	}()

	if panicRequest == nil {
		t.Fatal("pre-write panic path did not pass a request downstream")
	}
	state := apisixctx.GetRequestState(panicRequest)
	if state == nil || state.ApisixVars != nil || state.RequestVars != nil {
		t.Fatal("direct pre-write panic path leaked request state")
	}
	if got := counterValue(t, metrics.Requests); got != requestTotalBefore+1 {
		t.Fatalf("request total after pre-write panic = %v, want %v", got, requestTotalBefore+1)
	}
	for metricName, collector := range map[string]prometheus.Collector{
		"status":    metrics.HttpStatus,
		"latency":   metrics.HttpLatency,
		"bandwidth": metrics.Bandwidth,
	} {
		if metricVecHasSeries(t, collector, "legacy-prewrite-panic-route") {
			t.Fatalf("%s metrics recorded a completed/code=200 sample for a pre-write panic", metricName)
		}
	}
}

func TestRequestPhaseRequestContextInitializesStateAndSnapshotFinalizer(t *testing.T) {
	installTestMetrics(t)

	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		time.Now(),
	)
	p := &Plugin{config: Config{RouteID: "phase-route"}}
	result := p.RunRequestPhase(httptest.NewRecorder(), request)
	if result.Decision != base.RequestContinue {
		t.Fatalf("request phase decision = %d, want continue", result.Decision)
	}
	if result.Request == nil || apisixctx.GetRequestState(result.Request) == nil {
		t.Fatal("request phase did not initialize shared request state")
	}

	lifecycle.SetOutcome(apisixctx.ResponseOutcome{
		Kind:      apisixctx.RequestOutcomeCompleted,
		Status:    http.StatusNoContent,
		Committed: true,
	})
	if failures := lifecycle.Finalize(); len(failures) != 0 {
		t.Fatalf("lifecycle finalizer failures = %#v", failures)
	}
	if got := counterValue(t, metrics.Requests); got != 0 {
		t.Fatalf("request total = %v, want 0 before snapshot finalizer", got)
	}
	snapshotRequest := result.Request.Clone(result.Request.Context())
	snapshotRequest.Body = http.NoBody
	if err := p.RunSnapshotFinalizer(base.BuildLogSnapshot(
		snapshotRequest,
		base.ResponseCaptureSnapshot{},
		lifecycle.Outcome(),
		lifecycle.ResponseSource(),
		lifecycle.StartedAt(),
		lifecycle.FinishedAt(),
	)); err != nil {
		t.Fatalf("RunSnapshotFinalizer() error = %v", err)
	}
	if got := counterValue(t, metrics.Requests); got != 1 {
		t.Fatalf("request total = %v, want 1 after snapshot finalization", got)
	}
	apisixctx.RecycleVars(result.Request)
}

func TestRequestPhaseRequestContextAdapterReachesTerminal(t *testing.T) {
	request, lifecycle := apisixctx.EnsureRequestLifecycle(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		time.Now(),
	)
	called := false
	base.AdaptRequestPhase(&Plugin{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if apisixctx.GetRequestState(r) == nil {
			t.Fatal("terminal request has no shared request state")
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(httptest.NewRecorder(), request)
	if !called {
		t.Fatal("request phase adapter did not reach terminal")
	}
	lifecycle.Finalize()
	apisixctx.RecycleVars(request)
}

func metricVecHasSeries(t *testing.T, collector prometheus.Collector, route string) bool {
	t.Helper()
	collected := make(chan prometheus.Metric)
	go func() {
		collector.Collect(collected)
		close(collected)
	}()
	for metric := range collected {
		decoded := &dto.Metric{}
		if err := metric.Write(decoded); err != nil {
			t.Fatalf("decode metric: %v", err)
		}
		for _, label := range decoded.Label {
			if label.GetName() == "route" && label.GetValue() == route {
				return true
			}
		}
	}
	return false
}

func installTestMetrics(t *testing.T) {
	t.Helper()
	oldRequests := metrics.Requests
	oldStatus := metrics.HttpStatus
	oldLatency := metrics.HttpLatency
	oldBandwidth := metrics.Bandwidth
	oldLLMLatency := metrics.LLMLatency
	oldLLMPromptTokens := metrics.LLMPromptTokens
	oldLLMCompletionTokens := metrics.LLMCompletionTokens
	metrics.Requests = prometheus.NewCounter(prometheus.CounterOpts{Name: "test_request_context_requests"})
	metrics.HttpStatus = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_request_context_http_status"},
		[]string{
			"code", "route", "matched_uri", "matched_host", "service", "consumer", "node",
			"request_type", "request_llm_model", "llm_model", "response_source",
		},
	)
	metrics.HttpLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "test_request_context_http_latency"},
		[]string{"type", "route", "service", "consumer", "node", "request_type", "request_llm_model", "llm_model"},
	)
	metrics.Bandwidth = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_request_context_bandwidth"},
		[]string{"type", "route", "service", "consumer", "node", "request_type", "request_llm_model", "llm_model"},
	)
	llmLabels := []string{
		"route_id", "service_id", "consumer", "node", "request_type", "request_llm_model", "llm_model",
	}
	metrics.LLMLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{Name: "test_request_context_llm_latency"}, llmLabels,
	)
	metrics.LLMPromptTokens = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_request_context_llm_prompt_tokens"}, llmLabels,
	)
	metrics.LLMCompletionTokens = prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_request_context_llm_completion_tokens"}, llmLabels,
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
}
