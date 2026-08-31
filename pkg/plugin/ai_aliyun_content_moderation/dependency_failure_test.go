package ai_aliyun_content_moderation

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type cancellationRoundTripper struct{}

func (cancellationRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	<-request.Context().Done()
	return nil, request.Context().Err()
}

func TestModerationDependencyTimeoutPassesThrough(t *testing.T) {
	p := newTestPlugin(t, Config{
		Endpoint:        "http://aliyun.invalid",
		RegionID:        "cn-shanghai",
		AccessKeyID:     "test-access",
		AccessKeySecret: "test-secret",
		Timeout:         5,
	})
	p.client.Transport = cancellationRoundTripper{}

	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/embeddings",
		strings.NewReader(`{"input":"hello"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	nextCalls := 0
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalls++
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(response, request)

	if response.Code != http.StatusAccepted || nextCalls != 1 {
		t.Fatalf("timeout response = %d, next calls = %d; want 202 and one next", response.Code, nextCalls)
	}
	if p.client.Timeout != 5*time.Millisecond {
		t.Fatalf("client timeout = %s, want 5ms", p.client.Timeout)
	}
}
