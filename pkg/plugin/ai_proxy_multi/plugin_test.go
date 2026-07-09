package ai_proxy_multi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerRoundRobinBalancesAcrossInstances(t *testing.T) {
	var oneCalls atomic.Int64
	var twoCalls atomic.Int64

	one := newLLMServer(t, "one", "Bearer one-token", &oneCalls, http.StatusOK)
	defer one.Close()
	two := newLLMServer(t, "two", "Bearer two-token", &twoCalls, http.StatusOK)
	defer two.Close()

	p := newTestPlugin(t, Config{
		Balancer: Balancer{Algorithm: "roundrobin"},
		Instances: []Instance{
			{
				Name:     "one",
				Provider: "openai-compatible",
				Weight:   1,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer one-token"}},
				Options:  map[string]any{"model": "gpt-4"},
				Override: Override{Endpoint: one.URL + "/v1/chat/completions"},
			},
			{
				Name:     "two",
				Provider: "openai-compatible",
				Weight:   1,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer two-token"}},
				Options:  map[string]any{"model": "gpt-4o"},
				Override: Override{Endpoint: two.URL + "/v1/chat/completions"},
			},
		},
	})

	first := serveChat(t, p, "")
	second := serveChat(t, p, "")

	if oneCalls.Load() != 1 || twoCalls.Load() != 1 {
		t.Fatalf("upstream calls one=%d two=%d, want one call each", oneCalls.Load(), twoCalls.Load())
	}
	if first == second {
		t.Fatalf("round-robin responses = %q and %q, want different instances", first, second)
	}
}

func TestHandlerRetriesHTTP5xxFallback(t *testing.T) {
	var oneCalls atomic.Int64
	var twoCalls atomic.Int64

	one := newLLMServer(t, "one", "Bearer one-token", &oneCalls, http.StatusInternalServerError)
	defer one.Close()
	two := newLLMServer(t, "two", "Bearer two-token", &twoCalls, http.StatusOK)
	defer two.Close()

	p := newTestPlugin(t, Config{
		Balancer:         Balancer{Algorithm: "roundrobin"},
		FallbackStrategy: "http_5xx",
		MaxRetries:       intPtr(1),
		Instances: []Instance{
			{
				Name:     "one",
				Provider: "openai-compatible",
				Weight:   1,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer one-token"}},
				Override: Override{Endpoint: one.URL + "/v1/chat/completions"},
			},
			{
				Name:     "two",
				Provider: "openai-compatible",
				Weight:   1,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer two-token"}},
				Override: Override{Endpoint: two.URL + "/v1/chat/completions"},
			},
		},
	})

	body := serveChat(t, p, "")

	if oneCalls.Load() != 1 || twoCalls.Load() != 1 {
		t.Fatalf("upstream calls one=%d two=%d, want fallback to second instance", oneCalls.Load(), twoCalls.Load())
	}
	if !strings.Contains(body, `"instance":"two"`) {
		t.Fatalf("response body = %q, want second instance response", body)
	}
}

func TestHandlerChashUsesHeaderKey(t *testing.T) {
	var oneCalls atomic.Int64
	var twoCalls atomic.Int64

	one := newLLMServer(t, "one", "Bearer one-token", &oneCalls, http.StatusOK)
	defer one.Close()
	two := newLLMServer(t, "two", "Bearer two-token", &twoCalls, http.StatusOK)
	defer two.Close()

	p := newTestPlugin(t, Config{
		Balancer: Balancer{Algorithm: "chash", HashOn: "header", Key: "X-Tenant"},
		Instances: []Instance{
			{
				Name:     "one",
				Provider: "openai-compatible",
				Weight:   1,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer one-token"}},
				Override: Override{Endpoint: one.URL + "/v1/chat/completions"},
			},
			{
				Name:     "two",
				Provider: "openai-compatible",
				Weight:   1,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer two-token"}},
				Override: Override{Endpoint: two.URL + "/v1/chat/completions"},
			},
		},
	})

	for range 4 {
		serveChat(t, p, "tenant-a")
	}

	if oneCalls.Load() != 0 && twoCalls.Load() != 0 {
		t.Fatalf(
			"chash calls one=%d two=%d, want same header to choose one stable instance",
			oneCalls.Load(),
			twoCalls.Load(),
		)
	}
}

func TestHandlerMergesRequestBodyOverrideWithoutForce(t *testing.T) {
	var upstreamBody map[string]any
	upstream := newBodyCaptureLLMServer(t, "Bearer token", &upstreamBody)
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Instances: []Instance{
			{
				Name:     "one",
				Provider: "openai-compatible",
				Weight:   1,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer token"}},
				Override: Override{
					Endpoint: upstream.URL + "/v1/chat/completions",
					RequestBody: map[string]any{
						"openai-chat": map[string]any{
							"temperature": float64(0),
							"stream":      false,
							"metadata": map[string]any{
								"client":  "override",
								"gateway": "apisix-go",
							},
						},
					},
				},
			},
		},
	})

	serveChatWithBody(t, p, `{
	  "messages": [{"role": "user", "content": "ping"}],
	  "temperature": 1,
	  "metadata": {"client": "caller"}
	}`)

	if got := upstreamBody["temperature"]; got != float64(1) {
		t.Fatalf("temperature = %v, want client value to win without force", got)
	}
	if got := upstreamBody["stream"]; got != false {
		t.Fatalf("stream = %v, want override to fill missing field", got)
	}
	metadata, ok := upstreamBody["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata = %#v, want object", upstreamBody["metadata"])
	}
	if got := metadata["client"]; got != "caller" {
		t.Fatalf("metadata.client = %v, want caller", got)
	}
	if got := metadata["gateway"]; got != "apisix-go" {
		t.Fatalf("metadata.gateway = %v, want apisix-go", got)
	}
}

func TestHandlerForceMergesRequestBodyOverride(t *testing.T) {
	var upstreamBody map[string]any
	upstream := newBodyCaptureLLMServer(t, "Bearer token", &upstreamBody)
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Instances: []Instance{
			{
				Name:     "one",
				Provider: "openai-compatible",
				Weight:   1,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer token"}},
				Override: Override{
					Endpoint:                 upstream.URL + "/v1/chat/completions",
					RequestBodyForceOverride: boolPtr(true),
					RequestBody: map[string]any{
						"openai-chat": map[string]any{
							"temperature": float64(0),
							"metadata": map[string]any{
								"client":  "override",
								"gateway": "apisix-go",
							},
						},
					},
				},
			},
		},
	})

	serveChatWithBody(t, p, `{
	  "messages": [{"role": "user", "content": "ping"}],
	  "temperature": 1,
	  "metadata": {"client": "caller"}
	}`)

	if got := upstreamBody["temperature"]; got != float64(0) {
		t.Fatalf("temperature = %v, want override value with force", got)
	}
	metadata, ok := upstreamBody["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata = %#v, want object", upstreamBody["metadata"])
	}
	if got := metadata["client"]; got != "override" {
		t.Fatalf("metadata.client = %v, want override", got)
	}
	if got := metadata["gateway"]; got != "apisix-go" {
		t.Fatalf("metadata.gateway = %v, want apisix-go", got)
	}
}

func TestHandlerOmitsModelForAzureOpenAI(t *testing.T) {
	var upstreamBody map[string]any
	upstream := newBodyCaptureLLMServer(t, "Bearer azure-token", &upstreamBody)
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Instances: []Instance{
			{
				Name:     "azure",
				Provider: "azure-openai",
				Weight:   1,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer azure-token"}},
				Options: map[string]any{
					"model":       "gpt-4",
					"temperature": float64(0),
				},
				Override: Override{
					Endpoint: upstream.URL + "/openai/deployments/gpt-4/chat/completions?api-version=2024-02-15-preview",
				},
			},
		},
	})

	serveChatWithBody(t, p, `{
	  "model": "caller-model",
	  "messages": [{"role": "user", "content": "ping"}]
	}`)

	if _, ok := upstreamBody["model"]; ok {
		t.Fatalf("upstream body model = %v, want omitted for azure-openai", upstreamBody["model"])
	}
	if got := upstreamBody["temperature"]; got != float64(0) {
		t.Fatalf("temperature = %v, want configured option", got)
	}
}

func TestHandlerRejectsOversizedBodyBeforeProxy(t *testing.T) {
	p := newTestPlugin(t, Config{
		Instances: []Instance{
			{
				Name:     "one",
				Provider: "openai-compatible",
				Weight:   1,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer token"}},
				Override: Override{Endpoint: "http://127.0.0.1/v1/chat/completions"},
			},
		},
		MaxReqBodySize: 4,
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"messages":[]}`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called for oversized request")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("response code = %d, want 413", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "request body exceeds max_req_body_size") {
		t.Fatalf("response body = %q, want size message", rr.Body.String())
	}
}

func TestPostInitRejectsOpenAICompatibleWithoutEndpoint(t *testing.T) {
	p := &Plugin{config: Config{
		Instances: []Instance{
			{
				Name:     "one",
				Provider: "openai-compatible",
				Weight:   1,
				Auth:     Auth{Header: map[string]string{"Authorization": "Bearer token"}},
			},
		},
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := p.PostInit(); err == nil || !strings.Contains(err.Error(), "override.endpoint is required") {
		t.Fatalf("PostInit() error = %v, want override endpoint error", err)
	}
}

func newLLMServer(t *testing.T, instance string, wantAuth string, calls *atomic.Int64, status int) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost {
			t.Fatalf("%s upstream method = %s, want POST", instance, r.Method)
		}
		if got := r.URL.Path; got != "/v1/chat/completions" {
			t.Fatalf("%s upstream path = %s, want /v1/chat/completions", instance, got)
		}
		if got := r.Header.Get("Authorization"); got != wantAuth {
			t.Fatalf("%s Authorization header = %q, want %q", instance, got, wantAuth)
		}

		var upstreamBody map[string]any
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("%s decode upstream body: %v", instance, err)
		}
		if upstreamBody["messages"] == nil {
			t.Fatalf("%s upstream body missing messages: %#v", instance, upstreamBody)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"instance":"` + instance + `"}`))
	}))
}

func newBodyCaptureLLMServer(t *testing.T, wantAuth string, body *map[string]any) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != wantAuth {
			t.Fatalf("Authorization header = %q, want %q", got, wantAuth)
		}
		if err := json.NewDecoder(r.Body).Decode(body); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
}

func serveChat(t *testing.T, p *Plugin, tenant string) string {
	t.Helper()

	return serveChatWithBody(t, p, `{
	  "messages": [{"role": "user", "content": "ping"}],
	  "temperature": 1
	}`, tenant)
}

func serveChatWithBody(t *testing.T, p *Plugin, body string, tenant ...string) string {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{
	  "messages": [{"role": "user", "content": "ping"}],
	  "temperature": 1
	}`))
	if body != "" {
		req = httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(body))
	}
	req.Header.Set("Content-Type", "application/json")
	if len(tenant) > 0 && tenant[0] != "" {
		req.Header.Set("X-Tenant", tenant[0])
	}
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called by ai-proxy-multi")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200, body %q", rr.Code, rr.Body.String())
	}

	return strings.TrimSpace(rr.Body.String())
}

func intPtr(v int) *int {
	return &v
}

func boolPtr(v bool) *bool {
	return &v
}
