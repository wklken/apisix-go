package ai_aws_content_moderation

import (
	"context"
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

func TestComprehendDependencyFailureMatrix(t *testing.T) {
	tests := []struct {
		name      string
		server    func(*testing.T) *httptest.Server
		transport http.RoundTripper
	}{
		{
			name: "malformed response",
			server: func(t *testing.T) *httptest.Server {
				t.Helper()
				return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write([]byte(`{"ResultList":`))
				}))
			},
		},
		{name: "connect failure", transport: failingAWSTransport{}},
		{name: "timeout", transport: cancellationRoundTripper{}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			endpoint := "http://comprehend.invalid"
			if test.server != nil {
				server := test.server(t)
				t.Cleanup(server.Close)
				endpoint = server.URL
			}
			p := newTestPlugin(t, Config{Comprehend: Comprehend{
				AccessKeyID: "test-access", SecretAccessKey: "test-secret",
				Region: "us-east-1", Endpoint: endpoint,
			}})
			if test.transport != nil {
				p.client.Transport = test.transport
			}
			if test.name == "timeout" {
				p.client.Timeout = 5 * time.Millisecond
			}

			request := httptest.NewRequest(
				http.MethodPost,
				"/v1/chat/completions",
				strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`),
			)
			response := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("failed moderation dependency reached downstream")
			})).ServeHTTP(response, request)
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("failure status = %d, want 500; body = %q", response.Code, response.Body.String())
			}
		})
	}
}

type failingAWSTransport struct{}

func (failingAWSTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, context.Canceled
}
