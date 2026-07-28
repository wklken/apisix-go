package batch_requests

import (
	"encoding/json"
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

func TestHandlerTimeoutDoesNotWaitForUncooperativeDispatcher(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	dispatcher := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-block
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
	responses := decodePipelineResponses(t, res.Body.String())
	if responses[0].Status != http.StatusGatewayTimeout {
		t.Fatalf("pipeline status = %d, want 504", responses[0].Status)
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
