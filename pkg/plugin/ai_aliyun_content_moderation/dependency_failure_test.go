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

func TestModerationDependencyTimeoutFailsClosed(t *testing.T) {
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
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("timed-out moderation request reached downstream")
	})).ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("timeout status = %d, want 503", response.Code)
	}
	if p.client.Timeout != 5*time.Millisecond {
		t.Fatalf("client timeout = %s, want 5ms", p.client.Timeout)
	}
	if !strings.Contains(response.Body.String(), "service unavailable") {
		t.Fatalf("timeout body = %q, want redacted dependency failure", response.Body.String())
	}
}
