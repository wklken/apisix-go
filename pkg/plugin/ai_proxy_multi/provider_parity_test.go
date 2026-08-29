package ai_proxy_multi

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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

func TestOpenAIMultiDefaultEndpointAndUpstream401MatchAPISIX317(t *testing.T) {
	p := newTestPlugin(t, Config{Instances: []Instance{{
		Name:     "openai-official",
		Provider: "openai",
		Weight:   1,
		Auth: Auth{Header: map[string]string{
			"Authorization": "some-key",
		}},
		Options: map[string]any{
			"model":       "gpt-4",
			"max_tokens":  512,
			"temperature": 1.0,
		},
	}}})

	var providerRequest *http.Request
	p.client = &http.Client{Transport: multiStreamRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		providerRequest = request
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": []string{"text/plain"}},
			Body:       io.NopCloser(strings.NewReader("Unauthorized")),
			Request:    request,
		}, nil
	})}

	request := httptest.NewRequest(
		http.MethodPost,
		"/anything",
		strings.NewReader(`{"messages":[{"role":"user","content":"What is 1+1?"}]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(response, request)

	if providerRequest == nil {
		t.Fatal("provider request = nil")
	}
	if got := providerRequest.URL.String(); got != "https://api.openai.com/v1/chat/completions" {
		t.Fatalf("provider URL = %q, want OpenAI default chat endpoint", got)
	}
	if got := providerRequest.Header.Get("Authorization"); got != "some-key" {
		t.Fatalf("provider Authorization = %q, want configured header", got)
	}
	if response.Code != http.StatusUnauthorized || response.Body.String() != "Unauthorized" {
		t.Fatalf("response = %d %q, want 401 Unauthorized", response.Code, response.Body.String())
	}
}

func TestAPISIX317DefaultOpenAIHealthTarget(t *testing.T) {
	target, err := healthTarget(Instance{
		Name: "openai-gpt4", Provider: "openai",
		Auth: Auth{Header: map[string]string{"Authorization": "Bearer token"}},
	}, ActiveHealthCheck{Type: "https", HTTPPath: "/"})
	if err != nil {
		t.Fatalf("healthTarget() error = %v", err)
	}
	if got := target.String(); got != "https://api.openai.com/" {
		t.Fatalf("health target = %q, want APISIX 3.17 OpenAI default", got)
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

func TestAPISIX317BalancerWeightDistributions(t *testing.T) {
	instances := []Instance{
		{Name: "openai", Provider: "openai", Weight: 4},
		{Name: "deepseek", Provider: "deepseek", Weight: 1},
	}

	roundRobin := newTestPlugin(t, Config{
		Balancer:  Balancer{Algorithm: "roundrobin"},
		Instances: instances,
	})
	roundRobinCounts := map[string]int{}
	for range 10 {
		index, ok := roundRobin.pickInstance(nil, nil)
		if !ok {
			t.Fatal("round-robin picker found no instance")
		}
		roundRobinCounts[roundRobin.config.Instances[index].Name]++
	}
	if roundRobinCounts["openai"] != 8 || roundRobinCounts["deepseek"] != 2 {
		t.Fatalf("round-robin distribution = %#v, want openai=8 deepseek=2", roundRobinCounts)
	}

	consistentHash := newTestPlugin(t, Config{
		Balancer: Balancer{Algorithm: "chash", HashOn: "vars", Key: "query_string"},
		Instances: []Instance{
			{Name: "openai", Provider: "openai", Weight: 4},
			{Name: "deepseek", Provider: "deepseek", Weight: 1},
		},
	})
	consistentHashCounts := map[string]int{}
	for i := 1; i <= 10; i++ {
		request := httptest.NewRequest(http.MethodPost, "/anything?index="+strconv.Itoa(i), nil)
		index, ok := consistentHash.pickInstance(request, nil)
		if !ok {
			t.Fatal("consistent-hash picker found no instance")
		}
		consistentHashCounts[consistentHash.config.Instances[index].Name]++
	}
	if consistentHashCounts["openai"] != 8 || consistentHashCounts["deepseek"] != 2 {
		t.Fatalf("consistent-hash distribution = %#v, want openai=8 deepseek=2", consistentHashCounts)
	}
}

func TestAPISIX317FallbackStatusAndTransportMatrix(t *testing.T) {
	type providerOutcome struct {
		name         string
		status       int
		transportErr bool
	}
	tests := []struct {
		name       string
		strategy   []string
		providers  []providerOutcome
		wantStatus int
		wantBody   string
	}{
		{
			name: "500 falls back", strategy: []string{"http_5xx"},
			providers:  []providerOutcome{{name: "fail-500", status: 500}, {name: "success", status: 200}},
			wantStatus: 200, wantBody: "success",
		},
		{
			name: "429 falls back", strategy: []string{"http_429"},
			providers:  []providerOutcome{{name: "fail-429", status: 429}, {name: "success", status: 200}},
			wantStatus: 200, wantBody: "success",
		},
		{
			name: "transport failure falls back", strategy: []string{"http_5xx"},
			providers: []providerOutcome{
				{name: "unreachable", transportErr: true},
				{name: "success", status: 200},
			},
			wantStatus: 200, wantBody: "success",
		},
		{
			name: "503 falls back", strategy: []string{"http_5xx"},
			providers:  []providerOutcome{{name: "fail-503", status: 503}, {name: "success", status: 200}},
			wantStatus: 200, wantBody: "success",
		},
		{
			name: "priority chain reaches success", strategy: []string{"http_429", "http_5xx"},
			providers: []providerOutcome{
				{name: "fail-429", status: 429},
				{name: "fail-500", status: 500},
				{name: "success", status: 200},
			},
			wantStatus: 200, wantBody: "success",
		},
		{
			name: "exhausted multi-instance fallback returns 502", strategy: []string{"http_429", "http_5xx"},
			providers: []providerOutcome{
				{name: "fail-429", status: 429},
				{name: "fail-500", status: 500},
			},
			wantStatus: 502,
		},
		{
			name: "single instance preserves 500", strategy: []string{"http_5xx"},
			providers:  []providerOutcome{{name: "fail-500", status: 500}},
			wantStatus: 500,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			instances := make([]Instance, 0, len(test.providers))
			outcomes := make(map[string]providerOutcome, len(test.providers))
			for i, outcome := range test.providers {
				instances = append(instances, Instance{
					Name: outcome.name, Provider: "openai-compatible", Weight: 1,
					Priority: len(test.providers) - i,
					Override: Override{Endpoint: "http://" + outcome.name + ".test"},
				})
				outcomes[outcome.name+".test"] = outcome
			}
			plugin := newTestPlugin(t, Config{
				FallbackStrategy: test.strategy,
				Instances:        instances,
			})
			calls := make([]string, 0, len(test.providers))
			plugin.client = &http.Client{
				Transport: multiStreamRoundTripFunc(func(request *http.Request) (*http.Response, error) {
					outcome := outcomes[request.URL.Hostname()]
					calls = append(calls, outcome.name)
					if outcome.transportErr {
						return nil, errors.New("connection refused")
					}
					body := outcome.name
					if outcome.status == http.StatusOK {
						body = "success"
					}
					return &http.Response{
						StatusCode: outcome.status,
						Header:     http.Header{"Content-Type": []string{"text/plain"}},
						Body:       io.NopCloser(strings.NewReader(body)),
						Request:    request,
					}, nil
				}),
			}

			request := httptest.NewRequest(
				http.MethodPost,
				"/anything",
				strings.NewReader(`{"messages":[{"role":"user","content":"What is 1+1?"}]}`),
			)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			plugin.Handler(http.NotFoundHandler()).ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf(
					"response status = %d, want %d; body = %q",
					response.Code,
					test.wantStatus,
					response.Body.String(),
				)
			}
			if test.wantBody != "" && response.Body.String() != test.wantBody {
				t.Fatalf("response body = %q, want %q", response.Body.String(), test.wantBody)
			}
			wantCalls := make([]string, 0, len(test.providers))
			for _, outcome := range test.providers {
				wantCalls = append(wantCalls, outcome.name)
			}
			if strings.Join(calls, ",") != strings.Join(wantCalls, ",") {
				t.Fatalf("provider calls = %v, want %v", calls, wantCalls)
			}
		})
	}
}
