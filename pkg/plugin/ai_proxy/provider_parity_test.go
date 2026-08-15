package ai_proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/ai_auth"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
	"github.com/wklken/apisix-go/pkg/plugin/ai_stream"
)

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
