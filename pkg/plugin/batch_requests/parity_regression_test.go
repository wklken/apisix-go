package batch_requests

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
)

func TestParityValidTimeoutRejected(t *testing.T) {
	h := NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	rr := httptest.NewRecorder()
	h.ServeHTTP(
		rr,
		httptest.NewRequest(
			"POST",
			"http://gateway/apisix/batch-requests",
			strings.NewReader(`{"timeout":31000,"pipeline":[{"path":"/ok"}]}`),
		),
	)
	t.Logf("observed status=%d body=%q", rr.Code, rr.Body.String())
	if rr.Code != 200 {
		t.Errorf("APISIX timeout schema has no maximum, got %d", rr.Code)
	}
}

func TestParityArrayQueryRejected(t *testing.T) {
	h := NewHandler(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(r.URL.RawQuery)) }),
	)
	rr := httptest.NewRecorder()
	h.ServeHTTP(
		rr,
		httptest.NewRequest(
			"POST",
			"http://gateway/apisix/batch-requests",
			strings.NewReader(`{"pipeline":[{"path":"/ok","query":{"tag":["a","b"]}}]}`),
		),
	)
	t.Logf("observed status=%d body=%q", rr.Code, rr.Body.String())
	if rr.Code != 200 {
		t.Fatalf("APISIX query object supports repeated-value arrays, got %d", rr.Code)
	}
	var responses []PipelineResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &responses); err != nil {
		t.Fatal(err)
	}
	if len(responses) != 1 || responses[0].Body != "tag=a&tag=b" {
		t.Fatalf("pipeline response = %#v", responses)
	}
}

func TestVeryLargeTimeoutDoesNotWrapNegative(t *testing.T) {
	h := NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) < time.Hour {
			t.Errorf("deadline = %v, %t; want a future deadline", deadline, ok)
		}
		_, _ = w.Write([]byte("ok"))
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(
		w,
		httptest.NewRequest(
			"POST",
			DefaultURI,
			strings.NewReader(`{"timeout":9223372036854775807,"pipeline":[{"path":"/ok"}]}`),
		),
	)
	var responses []PipelineResponse
	if err := json.Unmarshal(w.Body.Bytes(), &responses); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(responses) != 1 || responses[0].Status != 200 {
		t.Fatalf("response = %d %s", w.Code, w.Body.String())
	}
}
