package ai_proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/ai_auth"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
	"github.com/wklken/apisix-go/pkg/plugin/ai_stream"
)

type timeoutTransportError struct{}

func (timeoutTransportError) Error() string   { return "connect: timeout" }
func (timeoutTransportError) Timeout() bool   { return true }
func (timeoutTransportError) Temporary() bool { return true }

func TestAIMLAPIDefaultEndpointMatchesAPISIX317(t *testing.T) {
	p := &Plugin{config: Config{Provider: "aimlapi"}}
	endpoint, err := p.endpointDocument(ai_protocols.OpenAIChat, ai_protocols.Document{})
	if err != nil {
		t.Fatalf("endpointDocument() error = %v", err)
	}
	if endpoint != "https://api.aimlapi.com/chat/completions" {
		t.Fatalf("endpoint = %q, want APISIX 3.17 AIMLAPI endpoint", endpoint)
	}
}

func TestMalformedAWSEventStreamBeforeFirstFrameReturns502(t *testing.T) {
	p := newTestPlugin(t, Config{
		Provider:     "bedrock",
		ProviderConf: map[string]any{"region": "us-east-1"},
		Auth: Auth{AWS: &ai_auth.AWSConfig{
			AccessKeyID: "key", SecretAccessKey: "secret",
		}},
		Options: map[string]any{"model": "claude"},
	})
	p.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type":     {"application/vnd.amazon.eventstream"},
				"Content-Length":   {"9"},
				"Content-Encoding": {"gzip"},
				"X-Upstream":       {"must-not-leak"},
			},
			Body: io.NopCloser(strings.NewReader("malformed")),
		}, nil
	})
	request := apisixctx.WithRequestVars(httptest.NewRequest(
		http.MethodPost,
		"/model/claude/converse",
		strings.NewReader(`{"messages":[{"role":"user","content":[{"text":"hello"}]}],"stream":true}`),
	))
	response := httptest.NewRecorder()

	p.Handler(http.NotFoundHandler()).ServeHTTP(response, request)

	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %q", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Length") != "" || response.Header().Get("Content-Encoding") != "" ||
		response.Header().Get("X-Upstream") != "" {
		t.Fatalf("precommit 502 leaked upstream headers: %#v", response.Header())
	}
	if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := apisixctx.GetRequestVar(request, "$ai_stream_outcome"); got != string(ai_stream.StreamOutcomeError) {
		t.Fatalf("stream outcome = %#v, want error", got)
	}
}

func TestProviderTransportErrorsMapStatusWithoutLeakingDetails(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{name: "context deadline", err: context.DeadlineExceeded, want: http.StatusGatewayTimeout},
		{name: "net timeout", err: timeoutTransportError{}, want: http.StatusGatewayTimeout},
		{name: "wrapped ETIMEDOUT", err: fmt.Errorf("dial failed: %w", syscall.ETIMEDOUT), want: http.StatusGatewayTimeout},
		{name: "timeout text only", err: errors.New("connect: timeout but not a timeout error"), want: http.StatusInternalServerError},
		{name: "other network error", err: errors.New("connect: connection refused"), want: http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				Provider: "openai-compatible",
				Override: Override{Endpoint: "http://provider.test"},
			})
			p.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, test.err
			})
			request := httptest.NewRequest(
				http.MethodPost,
				"/v1/chat/completions",
				strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`),
			)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()

			p.Handler(http.NotFoundHandler()).ServeHTTP(response, request)

			if response.Code != test.want {
				t.Fatalf("status = %d, want %d; body = %q", response.Code, test.want, response.Body.String())
			}
			if strings.Contains(response.Body.String(), test.err.Error()) {
				t.Fatalf("response body leaked transport error %q: %q", test.err, response.Body.String())
			}
		})
	}
}

func TestProviderErrorResponsesReturnStatusWithoutBodyOrHeaders(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, 599} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("status-%d-stream-%t", status, streaming), func(t *testing.T) {
				p := newTestPlugin(t, Config{
					Provider: "openai-compatible",
					Override: Override{Endpoint: "http://provider.test"},
				})
				p.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: status,
						Header: http.Header{
							"Content-Type": {"text/plain"},
							"X-Provider":   {"must-not-leak"},
						},
						Body: io.NopCloser(strings.NewReader("provider secret")),
					}, nil
				})
				body := `{"messages":[{"role":"user","content":"hello"}]}`
				if streaming {
					body = `{"messages":[{"role":"user","content":"hello"}],"stream":true}`
				}
				request := httptest.NewRequest(
					http.MethodPost,
					"/v1/chat/completions",
					strings.NewReader(body),
				)
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()

				p.Handler(http.NotFoundHandler()).ServeHTTP(response, request)

				if response.Code != status {
					t.Fatalf("status = %d, want %d; body = %q", response.Code, status, response.Body.String())
				}
				if response.Body.Len() != 0 {
					t.Fatalf("provider body forwarded: %q", response.Body.String())
				}
				for _, header := range []string{"Content-Type", "X-Provider"} {
					if got := response.Header().Get(header); got != "" {
						t.Fatalf("provider header %s forwarded: %q", header, got)
					}
				}
			})
		}
	}
}
