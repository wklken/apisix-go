package batch_requests

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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
