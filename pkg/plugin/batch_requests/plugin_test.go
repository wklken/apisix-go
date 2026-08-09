package batch_requests

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/util"
)

func TestMetadataSchemaRejectsNonpositiveLimits(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if p.GetMetadataSchema() == "" {
		t.Fatal("metadata schema is empty")
	}

	for _, metadata := range []map[string]any{
		{"max_body_size": 0},
		{"max_pipeline_items": 0},
	} {
		if err := util.Validate(metadata, p.GetMetadataSchema()); err == nil {
			t.Fatalf("metadata %#v validated, want positive-limit rejection", metadata)
		}
	}
}

func TestHandlerRejectsUnknownPipelineField(t *testing.T) {
	handler := NewHandlerWithLimits(http.NewServeMux(), Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"pipeline": [{"path": "/inner", "unknown_field": "x"}]
	}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400; body=%q", res.Code, res.Body.String())
	}
	if got := res.Header().Get("Content-Type"); got != "application/json; charset=UTF-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if strings.HasSuffix(res.Body.String(), "\n") {
		t.Fatalf("response body has trailing newline: %q", res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "unknown_field") {
		t.Fatalf("response body = %q, want unknown field error", res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "bad request body") {
		t.Fatalf("response body = %q, want schema validation prefix", res.Body.String())
	}
}

func TestHandlerRejectsTrailingJSONValue(t *testing.T) {
	handler := NewHandlerWithLimits(http.NewServeMux(), Limits{})
	req := httptest.NewRequest(
		http.MethodPost,
		DefaultURI,
		strings.NewReader(`{"pipeline":[{"path":"/inner"}]} {}`),
	)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400; body=%q", res.Code, res.Body.String())
	}
}

func TestHandlerRejectsExplicitZeroTimeout(t *testing.T) {
	handler := NewHandlerWithLimits(http.NewServeMux(), Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"timeout": 0,
		"pipeline": [{"path": "/inner"}]
	}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400; body=%q", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "timeout") {
		t.Fatalf("response body = %q, want timeout validation error", res.Body.String())
	}
}

func TestHandlerReportsTimeoutTypeMismatchAsBadRequestBody(t *testing.T) {
	handler := NewHandlerWithLimits(http.NewServeMux(), Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"timeout": "200",
		"pipeline": [{"path": "/inner"}]
	}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400; body=%q", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "bad request body") {
		t.Fatalf("response body = %q, want schema validation prefix", res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "timeout") {
		t.Fatalf("response body = %q, want timeout field name", res.Body.String())
	}
}

func TestHandlerReportsSSLVerifyTypeMismatchAsBadRequestBody(t *testing.T) {
	handler := NewHandlerWithLimits(http.NewServeMux(), Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"pipeline": [{"path": "/inner", "ssl_verify": 1.2}]
	}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400; body=%q", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "bad request body") {
		t.Fatalf("response body = %q, want schema validation prefix", res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "ssl_verify") {
		t.Fatalf("response body = %q, want ssl_verify field name", res.Body.String())
	}
}

func TestHandlerStopsAfterTimedOutPipelineRequest(t *testing.T) {
	var calls atomic.Int32
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/slow" {
			t.Fatalf("unexpected dispatch path %q after timeout", r.URL.Path)
		}
		<-r.Context().Done()
	})
	handler := NewHandlerWithLimits(dispatcher, Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"timeout": 10,
		"pipeline": [
			{"path": "/slow"},
			{"path": "/must-not-run"}
		]
	}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	responses := decodePipelineResponses(t, res.Body.String())
	if responses[0].Status != http.StatusGatewayTimeout {
		t.Fatalf("pipeline status = %d, want 504", responses[0].Status)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("dispatcher calls = %d, want 1", got)
	}
}

func TestHandlerContinuesAfterIntentionalGatewayTimeout(t *testing.T) {
	var calls atomic.Int32
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/gateway-timeout":
			w.WriteHeader(http.StatusGatewayTimeout)
		case "/after":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected dispatch path %q", r.URL.Path)
		}
	})
	handler := NewHandlerWithLimits(dispatcher, Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"pipeline": [
			{"path": "/gateway-timeout"},
			{"path": "/after"}
		]
	}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	var responses []PipelineResponse
	if err := json.Unmarshal(res.Body.Bytes(), &responses); err != nil {
		t.Fatalf("decode responses: %v, body=%q", err, res.Body.String())
	}
	if len(responses) != 2 {
		t.Fatalf("responses length = %d, want 2: %#v", len(responses), responses)
	}
	if responses[0].Status != http.StatusGatewayTimeout || responses[0].Reason != "Gateway Timeout" {
		t.Fatalf("first response = %#v, want intentional 504", responses[0])
	}
	if responses[1].Status != http.StatusNoContent {
		t.Fatalf("second response = %#v, want 204", responses[1])
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("dispatcher calls = %d, want 2", got)
	}
}

func TestHandlerAllowsUnknownTopLevelFields(t *testing.T) {
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/inner" {
			t.Fatalf("dispatch path = %q, want /inner", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	handler := NewHandlerWithLimits(dispatcher, Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"extension": {"trace": true},
		"pipeline": [{"path": "/inner"}]
	}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200; body=%q", res.Code, res.Body.String())
	}
	responses := decodePipelineResponses(t, res.Body.String())
	if responses[0].Status != http.StatusNoContent {
		t.Fatalf("pipeline status = %d, want 204", responses[0].Status)
	}
}

func TestHandlerDistinguishesMissingPipelineFromEmptyPipeline(t *testing.T) {
	handler := NewHandlerWithLimits(http.NewServeMux(), Limits{})

	missing := httptest.NewRecorder()
	handler.ServeHTTP(
		missing,
		httptest.NewRequest(
			http.MethodPost,
			DefaultURI,
			strings.NewReader(`{"pipeline1":[{"path":"/inner"}]}`),
		),
	)
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("missing pipeline response code = %d, want 400; body=%q",
			missing.Code, missing.Body.String())
	}
	if !strings.Contains(missing.Body.String(), "pipeline is required") {
		t.Fatalf("missing pipeline body = %q, want required reason", missing.Body.String())
	}

	empty := httptest.NewRecorder()
	handler.ServeHTTP(
		empty,
		httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{"pipeline":[]}`)),
	)
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("empty pipeline response code = %d, want 400; body=%q",
			empty.Code, empty.Body.String())
	}
	if !strings.Contains(empty.Body.String(), "at least one request") {
		t.Fatalf("empty pipeline body = %q, want minItems reason", empty.Body.String())
	}
}

func TestMetadataHandlerRejectsInvalidInitialMetadata(t *testing.T) {
	handler := newMetadataHandler(http.NewServeMux(), func() (Limits, error) {
		return Limits{}, errors.New("max_body_size must be positive")
	})
	req := httptest.NewRequest(
		http.MethodPost,
		DefaultURI,
		strings.NewReader(`{"pipeline":[{"path":"/inner"}]}`),
	)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400; body=%q", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "invalid configuration") {
		t.Fatalf("response body = %q, want invalid configuration", res.Body.String())
	}
}

func TestMetadataHandlerSeedsBeforeFirstRequest(t *testing.T) {
	var loads atomic.Int32
	handler := newMetadataHandler(http.NewServeMux(), func() (Limits, error) {
		loads.Add(1)
		return Limits{MaxBodySize: defaultMaxBodySize, MaxPipelineItems: defaultMaxPipelineItems}, nil
	})
	if got := loads.Load(); got != 1 {
		t.Fatalf("metadata loads after construction = %d, want 1 seed", got)
	}

	res := httptest.NewRecorder()
	handler.ServeHTTP(
		res,
		httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{"pipeline":[{"path":"/missing"}]}`)),
	)
	if res.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200; body=%q", res.Code, res.Body.String())
	}
	if got := loads.Load(); got != 2 {
		t.Fatalf("metadata loads after request = %d, want seed plus request load", got)
	}
}

func TestHandlerRejectsBodyAboveConfiguredLimit(t *testing.T) {
	handler := NewHandlerWithLimits(http.NewServeMux(), Limits{
		MaxBodySize:      20,
		MaxPipelineItems: defaultMaxPipelineItems,
	})

	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{"pipeline":[{"path":"/ok"}]}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusRequestEntityTooLarge)
	}
	if !strings.Contains(res.Body.String(), "http: request body too large") {
		t.Fatalf("response body = %q, want body size error", res.Body.String())
	}
}

func TestHandlerRejectsPipelineAboveConfiguredLimit(t *testing.T) {
	handler := NewHandlerWithLimits(http.NewServeMux(), Limits{
		MaxBodySize:      defaultMaxBodySize,
		MaxPipelineItems: 1,
	})

	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"pipeline": [
			{"path": "/one"},
			{"path": "/two"}
		]
	}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusBadRequest)
	}
	if !strings.Contains(res.Body.String(), "2 exceeds the maximum of 1") {
		t.Fatalf("response body = %q, want pipeline limit error", res.Body.String())
	}
}

func TestHandlerInjectsRealIPHeaderIntoPipelineRequest(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/inner", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Got-Real-IP", r.Header.Get("X-Real-IP"))
		w.WriteHeader(http.StatusNoContent)
	})
	handler := NewHandlerWithLimits(mux, Limits{
		MaxBodySize:      defaultMaxBodySize,
		MaxPipelineItems: defaultMaxPipelineItems,
	})

	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"headers": {"X-Real-IP": "198.51.100.1"},
		"pipeline": [
			{"path": "/inner", "headers": {"X-Real-IP": "198.51.100.2"}}
		]
	}`))
	req.RemoteAddr = "203.0.113.10:54321"
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("response code = %d, want %d, body=%q", res.Code, http.StatusOK, res.Body.String())
	}
	responses := decodePipelineResponses(t, res.Body.String())
	if got := responses[0].Headers["X-Got-Real-Ip"]; got != "203.0.113.10" {
		t.Fatalf("X-Got-Real-IP = %q, want outer remote IP", got)
	}
}

func TestHandlerRejectsUnsupportedHTTPVersion(t *testing.T) {
	handler := NewHandlerWithLimits(http.NewServeMux(), Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"pipeline": [{"path": "/inner", "version": 2.0}]
	}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusBadRequest)
	}
	if !strings.Contains(res.Body.String(), "pipeline[0].version is invalid") {
		t.Fatalf("response body = %q, want invalid version error", res.Body.String())
	}
}

func TestHandlerReturnsGatewayTimeoutForTimedOutPipelineRequest(t *testing.T) {
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	handler := NewHandlerWithLimits(dispatcher, Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"timeout": 10,
		"pipeline": [{"path": "/slow"}]
	}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	responses := decodePipelineResponses(t, res.Body.String())
	if responses[0].Status != http.StatusGatewayTimeout || responses[0].Reason != "upstream timeout" {
		t.Fatalf("pipeline response = %#v, want 504 upstream timeout", responses[0])
	}
}

func TestHandlerAppliesPipelineHostHeader(t *testing.T) {
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Got-Host", r.Host)
		w.WriteHeader(http.StatusNoContent)
	})
	handler := NewHandlerWithLimits(dispatcher, Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"headers": {"Host": "common.example.com"},
		"pipeline": [{"path": "/inner", "headers": {"Host": "item.example.com"}}]
	}`))
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	responses := decodePipelineResponses(t, res.Body.String())
	if got := responses[0].Headers["X-Got-Host"]; got != "item.example.com" {
		t.Fatalf("pipeline host = %q, want item.example.com", got)
	}
}

func TestHandlerTimeoutJoinsCancellationAwareDispatcher(t *testing.T) {
	entered := make(chan struct{})
	exited := make(chan struct{})
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(exited)
	})
	handler := NewHandlerWithLimits(dispatcher, Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"timeout": 10,
		"pipeline": [{"path": "/slow"}]
	}`))
	res := httptest.NewRecorder()

	start := time.Now()
	handler.ServeHTTP(res, req)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("handler elapsed = %s, want bounded timeout", elapsed)
	}
	select {
	case <-entered:
	default:
		t.Fatal("dispatcher never entered")
	}
	select {
	case <-exited:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("dispatcher worker was not joined after the timeout canceled its context")
	}
	responses := decodePipelineResponses(t, res.Body.String())
	if responses[0].Status != http.StatusGatewayTimeout {
		t.Fatalf("pipeline status = %d, want 504", responses[0].Status)
	}
	if responses[0].Body != "" {
		t.Fatalf("pipeline body = %q, want internal error text discarded", responses[0].Body)
	}
}

func TestHandlerParentCancellationCancelsSubrequestsAndJoinsWorkers(t *testing.T) {
	var started atomic.Int32
	var exited atomic.Int32
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started.Add(1)
		<-r.Context().Done()
		exited.Add(1)
	})
	handler := NewHandlerWithLimits(dispatcher, Limits{})
	req := httptest.NewRequest(http.MethodPost, DefaultURI, strings.NewReader(`{
		"pipeline": [
			{"path": "/slow-1"},
			{"path": "/slow-2"},
			{"path": "/slow-3"}
		]
	}`))
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	res := httptest.NewRecorder()

	served := make(chan struct{})
	go func() {
		handler.ServeHTTP(res, req)
		close(served)
	}()

	// Subrequests run one at a time; cancel while the first is in flight.
	deadline := time.Now().Add(2 * time.Second)
	for started.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("first subrequest never started")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("batch handler did not return after parent cancellation")
	}

	// The handler no longer blocks on the worker, so the cancellation-aware
	// in-flight worker exits asynchronously once its context is canceled.
	exitedDeadline := time.Now().Add(2 * time.Second)
	for exited.Load() != 1 {
		if time.Now().After(exitedDeadline) {
			t.Fatalf("in-flight subrequest worker did not exit after cancellation")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := started.Load(); got != 1 {
		t.Fatalf("started subrequests = %d, want the pipeline to stop after cancellation", got)
	}
	var responses []PipelineResponse
	if err := json.Unmarshal(res.Body.Bytes(), &responses); err != nil {
		t.Fatalf("decode batch response: %v, body=%q", err, res.Body.String())
	}
	if len(responses) != 1 {
		t.Fatalf("responses length = %d, want 1", len(responses))
	}
	if responses[0].Status != http.StatusGatewayTimeout {
		t.Fatalf("response status = %d, want 504", responses[0].Status)
	}
	if responses[0].Body != "" {
		t.Fatalf("response body = %q, want internal error text discarded", responses[0].Body)
	}
}

func TestDispatchPipelineRequestInvalidTargetReturnsBadRequest(t *testing.T) {
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	outer := httptest.NewRequest(http.MethodGet, "http://example.com/batch", nil)
	item := PipelineRequest{Path: "/%zz"}

	response, timedOut := dispatchPipelineRequest(dispatcher, outer, Request{}, item, time.Second)

	if timedOut {
		t.Fatal("invalid target reported a timeout")
	}
	if response.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Status, http.StatusBadRequest)
	}
	if response.Reason != http.StatusText(http.StatusBadRequest) {
		t.Fatalf("reason = %q, want %q", response.Reason, http.StatusText(http.StatusBadRequest))
	}
}

func TestDispatchPipelineRequestTimeoutWhenHandlerIgnoringCancellation(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	})
	outer := httptest.NewRequest(http.MethodGet, "http://example.com/batch", nil)
	item := PipelineRequest{Path: "/slow"}

	start := time.Now()
	response, timedOut := dispatchPipelineRequest(dispatcher, outer, Request{}, item, 20*time.Millisecond)
	elapsed := time.Since(start)

	if !timedOut {
		t.Fatal("timedOut = false, want true")
	}
	if response.Status != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d", response.Status, http.StatusGatewayTimeout)
	}
	if elapsed >= 200*time.Millisecond {
		t.Fatalf("dispatch took %s, want return before 200ms", elapsed)
	}
}

func decodePipelineResponses(t *testing.T, body string) []PipelineResponse {
	t.Helper()

	var responses []PipelineResponse
	if err := json.Unmarshal([]byte(body), &responses); err != nil {
		t.Fatalf("decode batch response: %v, body=%q", err, body)
	}
	if len(responses) != 1 {
		t.Fatalf("responses length = %d, want 1", len(responses))
	}
	return responses
}
