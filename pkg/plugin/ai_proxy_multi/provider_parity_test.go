package ai_proxy_multi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/ai_auth"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
	"github.com/wklken/apisix-go/pkg/plugin/ai_rate_limiting"
	"github.com/wklken/apisix-go/pkg/plugin/ai_runtime"
	"github.com/wklken/apisix-go/pkg/plugin/ai_stream"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestAIMLAPIMultiDefaultEndpointMatchesAPISIX317(t *testing.T) {
	p := &Plugin{}
	endpoint, err := p.endpoint(
		Instance{Provider: "aimlapi"},
		ai_protocols.OpenAIChat,
		ai_protocols.Document{},
	)
	if err != nil {
		t.Fatalf("endpoint() error = %v", err)
	}
	if endpoint != "https://api.aimlapi.com/chat/completions" {
		t.Fatalf("endpoint = %q, want APISIX 3.17 AIMLAPI endpoint", endpoint)
	}
}

func TestMalformedSSEUsesPrecommit502OrPostcommitTerminalEvent(t *testing.T) {
	for _, test := range []struct {
		name       string
		body       string
		wantStatus int
		wantEvent  bool
	}{
		{
			name: "precommit", body: "event: message\ndata: {malformed\n\n",
			wantStatus: http.StatusBadGateway,
		},
		{
			name: "postcommit",
			body: "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n" +
				"data: {malformed\n\n",
			wantStatus: http.StatusOK,
			wantEvent:  true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(test.body))
			}))
			defer upstream.Close()
			p := newTestPlugin(t, Config{Instances: []Instance{{
				Name: "stream", Provider: "openai-compatible", Weight: 1,
				Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
			}}})
			request := apisixctx.WithRequestVars(httptest.NewRequest(
				http.MethodPost,
				"/v1/chat/completions",
				strings.NewReader(`{"model":"test","stream":true,"messages":[{"role":"user","content":"hello"}]}`),
			))
			response := httptest.NewRecorder()
			p.Handler(http.NotFoundHandler()).ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %q", response.Code, test.wantStatus, response.Body.String())
			}
			if got := strings.Contains(response.Body.String(), "event: error"); got != test.wantEvent {
				t.Fatalf("terminal SSE event = %v, want %v; body = %q", got, test.wantEvent, response.Body.String())
			}
			if got := apisixctx.GetRequestVar(
				request,
				"$ai_stream_outcome",
			); got != string(
				ai_stream.StreamOutcomeError,
			) {
				t.Fatalf("stream outcome = %#v, want error", got)
			}
		})
	}
}

func TestMalformedAWSStreamFlushesFirstFrameAndEmitsTerminalException(t *testing.T) {
	first := testAWSEventStreamFrame(map[string]string{
		":message-type": "event", ":event-type": "contentBlockDelta",
	}, `{"delta":{"text":"first"}}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(append(first, []byte("malformed")...))
	}))
	defer upstream.Close()
	p := newTestPlugin(t, Config{Instances: []Instance{{
		Name: "bedrock", Provider: "bedrock", Weight: 1,
		ProviderConf: map[string]any{"region": "us-east-1"},
		Auth:         Auth{AWS: &ai_auth.AWSConfig{AccessKeyID: "key", SecretAccessKey: "secret"}},
		Options:      map[string]any{"model": "claude"}, Override: Override{Endpoint: upstream.URL},
	}}})
	request := apisixctx.WithRequestVars(httptest.NewRequest(
		http.MethodPost,
		"/model/claude/converse",
		strings.NewReader(`{"messages":[{"role":"user","content":[{"text":"hello"}]}],"stream":true}`),
	))
	response := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(response, request)

	if response.Code != http.StatusOK || !response.Flushed {
		t.Fatalf("status/flushed = %d/%v, want 200/true", response.Code, response.Flushed)
	}
	decoder := eventstream.NewDecoder()
	reader := bytes.NewReader(response.Body.Bytes())
	if _, err := decoder.Decode(reader, nil); err != nil {
		t.Fatalf("decode first AWS event: %v", err)
	}
	terminal, err := decoder.Decode(reader, nil)
	if err != nil {
		t.Fatalf("decode terminal AWS event: %v", err)
	}
	if got := terminal.Headers.Get(":message-type"); got == nil {
		t.Fatal("terminal AWS event has no message type")
	} else if value, _ := got.Get().(string); value != "exception" {
		t.Fatalf("terminal AWS message type = %q, want exception", value)
	}
	if got := apisixctx.GetRequestVar(request, "$ai_stream_outcome"); got != string(ai_stream.StreamOutcomeError) {
		t.Fatalf("stream outcome = %#v, want error", got)
	}
}

func TestBedrockClientStreamingIntentSurvivesProviderConversion(t *testing.T) {
	p := newTestPlugin(t, Config{Instances: []Instance{{
		Name:         "bedrock",
		Provider:     "bedrock",
		ProviderConf: map[string]any{"region": "us-east-1"},
		Weight:       1,
		Auth: Auth{AWS: &ai_auth.AWSConfig{
			AccessKeyID: "key", SecretAccessKey: "secret",
		}},
		Options: map[string]any{"model": "claude"},
	}}})
	request := httptest.NewRequest(
		http.MethodPost,
		"/model/claude/converse",
		strings.NewReader(`{"messages":[{"role":"user","content":[{"text":"hello"}]}],"stream":true}`),
	)
	response := httptest.NewRecorder()

	result := p.RunRequestPhase(response, request)
	if result.Decision != base.RequestContinue {
		t.Fatalf("RunRequestPhase decision = %v, body = %q", result.Decision, response.Body.String())
	}
	state := ai_runtime.FromRequest(result.Request)
	if state == nil || !state.StreamingIntent() {
		t.Fatal("client streaming intent was erased by Bedrock provider conversion")
	}
	if mode := (&ai_rate_limiting.Plugin{}).SelectResponseMode(
		result.Request,
	); mode != base.RequestResponseModeStreaming {
		t.Fatalf("ai-rate-limiting response mode = %v, want streaming", mode)
	}
}

func TestAIProxyMultiChashUsesPinnedKetamaOwner(t *testing.T) {
	p := newTestPlugin(t, Config{
		Balancer: Balancer{Algorithm: "chash", HashOn: "header", Key: "X-Tenant"},
		Instances: []Instance{
			{Name: "provider-a", Provider: "deepseek", Weight: 1},
			{Name: "provider-b", Provider: "deepseek", Weight: 2},
			{Name: "provider-c", Provider: "deepseek", Weight: 1},
		},
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.Header.Set("X-Tenant", "alpha")

	index, ok := p.pickInstance(request, nil)
	if !ok {
		t.Fatal("pickInstance() found no chash owner")
	}
	if got := p.config.Instances[index].Name; got != "provider-c" {
		t.Fatalf("chash owner = %q, want pinned provider-c", got)
	}
}
