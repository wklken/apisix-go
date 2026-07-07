package batch_requests

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
