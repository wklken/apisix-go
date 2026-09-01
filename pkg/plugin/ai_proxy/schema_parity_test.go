package ai_proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/ai_auth"
	"github.com/wklken/apisix-go/pkg/util"
)

// TestSchemaMatchesAPISIX317BaseProviderContracts mirrors the direct
// plugin.check_schema calls in t/plugin/ai-proxy.t TEST 1 and TEST 2.  These
// are plugin-schema decisions, so they should not depend on first-generation
// route publication or recovery behavior.
func TestSchemaMatchesAPISIX317BaseProviderContracts(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	valid := map[string]any{
		"provider": "openai",
		"options":  map[string]any{"model": "gpt-4"},
		"auth": map[string]any{
			"header": map[string]any{"some_header": "some_value"},
		},
	}
	if err := util.Validate(valid, p.GetSchema()); err != nil {
		t.Fatalf("minimal APISIX 3.17 configuration rejected: %v", err)
	}

	unsupportedProvider := map[string]any{
		"provider": "some-unique",
		"options":  map[string]any{"model": "gpt-4"},
		"auth": map[string]any{
			"header": map[string]any{"some_header": "some_value"},
		},
	}
	if err := util.Validate(unsupportedProvider, p.GetSchema()); err == nil {
		t.Fatal("unsupported provider was accepted")
	}
}

func TestSchemaMatchesAPISIX317GCPAuthContract(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	tests := []struct {
		name    string
		gcp     map[string]any
		wantErr bool
	}{
		{name: "empty uses environment credentials", gcp: map[string]any{}},
		{
			name: "explicit service account and ttl",
			gcp: map[string]any{
				"service_account_json": `{"type":"service_account"}`,
				"max_ttl":              3600,
				"expire_early_secs":    60,
			},
		},
		{name: "max ttl must be positive", gcp: map[string]any{"max_ttl": 0}, wantErr: true},
		{name: "max ttl must be integer", gcp: map[string]any{"max_ttl": 1.5}, wantErr: true},
		{name: "expire early must be non-negative", gcp: map[string]any{"expire_early_secs": -1}, wantErr: true},
		{name: "service account must be string", gcp: map[string]any{"service_account_json": 42}, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := map[string]any{
				"provider": "vertex-ai",
				"provider_conf": map[string]any{
					"project_id": "project",
					"region":     "us-central1",
				},
				"auth": map[string]any{"gcp": test.gcp},
			}
			err := util.Validate(config, p.GetSchema())
			if test.wantErr && err == nil {
				t.Fatalf("schema accepted invalid GCP auth %#v", test.gcp)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("schema rejected valid GCP auth %#v: %v", test.gcp, err)
			}
		})
	}
}

func TestSchemaKeepsAPISIX317VertexEndpointAlternatives(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	for _, config := range []map[string]any{
		{
			"provider":      "vertex-ai",
			"provider_conf": map[string]any{"project_id": "project", "region": "us-central1"},
			"auth":          map[string]any{},
		},
		{
			"provider": "vertex-ai",
			"override": map[string]any{"endpoint": "https://vertex.example.test"},
			"auth":     map[string]any{},
		},
	} {
		if err := util.Validate(config, p.GetSchema()); err != nil {
			t.Fatalf("schema rejected valid Vertex endpoint alternative %#v: %v", config, err)
		}
	}
}

func TestAPISIX317GCPServiceAccountJSONIsValidatedBeforeMaterialization(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "valid literal", value: `{"type":"service_account"}`},
		{name: "invalid literal", value: `{"type":`, wantErr: true},
		{name: "secret reference is deferred", value: "$secret://ai/gcp"},
		{name: "environment reference is deferred", value: "$ENV://GCP_SERVICE_ACCOUNT"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := &Plugin{config: Config{Auth: Auth{GCP: &ai_auth.GCPConfig{
				ServiceAccountJSON: test.value,
			}}}}
			err := p.ValidatePreMaterialization()
			if test.wantErr && err == nil {
				t.Fatal("ValidatePreMaterialization() error = nil")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("ValidatePreMaterialization() error = %v", err)
			}
		})
	}
}

// TestSchemaRejectsAPISIX317UnknownLLMOption mirrors
// t/plugin/ai-proxy-request-body-override.t TEST 1.  The route publication
// layer may choose its own invalid-generation status; the plugin contract is
// that an unknown llm_options field is rejected by schema validation.
func TestSchemaRejectsAPISIX317UnknownLLMOption(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{
		"provider": "openai",
		"auth": map[string]any{
			"header": map[string]any{"Authorization": "Bearer t"},
		},
		"override": map[string]any{
			"endpoint":    "http://localhost:6732",
			"llm_options": map[string]any{"temperature": 0.5},
		},
	}
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("unknown llm_options field was accepted")
	}
}

// TestSchemaRejectsAPISIX317MissingBedrockSecret mirrors
// t/plugin/ai-proxy-bedrock-single.t TEST 5.  Missing AWS credentials are a
// plugin schema rejection, while the 400/404 chosen for an unpublished route
// belongs to the surrounding publication pipeline.
func TestSchemaRejectsAPISIX317MissingBedrockSecret(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{
		"provider": "bedrock",
		"auth": map[string]any{
			"aws": map[string]any{"access_key_id": "AKIAIOSFODNN7EXAMPLE"},
		},
		"provider_conf": map[string]any{"region": "us-east-1"},
		"options":       map[string]any{"model": "anthropic.claude-3-5-sonnet-20241022-v2:0"},
	}
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("Bedrock configuration without secret_access_key was accepted")
	}
}

// TestSchemaRejectsAPISIX317NegativeStreamingFlushInterval mirrors
// t/plugin/ai-proxy-flush.t TEST 5.  The route-level 404 is an artifact of
// failed publication; the plugin-owned contract is the non-negative schema
// bound on streaming_flush_interval_ms.
func TestSchemaRejectsAPISIX317NegativeStreamingFlushInterval(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{
		"provider":                    "openai",
		"auth":                        map[string]any{"header": map[string]any{"Authorization": "Bearer test-key"}},
		"options":                     map[string]any{"model": "gpt-4"},
		"streaming_flush_interval_ms": -1,
	}
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("negative streaming_flush_interval_ms was accepted")
	}
}

// TestSchemaAcceptsAPISIX317ZeroStreamingFlushInterval mirrors
// t/plugin/ai-proxy-flush.t TEST 6.  Zero is an intentional value that
// disables the background flush loop, so it remains inside the schema bound.
func TestSchemaAcceptsAPISIX317ZeroStreamingFlushInterval(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{
		"provider":                    "openai",
		"auth":                        map[string]any{"header": map[string]any{"Authorization": "Bearer x"}},
		"options":                     map[string]any{"model": "gpt-4"},
		"streaming_flush_interval_ms": 0,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("zero streaming_flush_interval_ms rejected: %v", err)
	}
}

// TestPostInitAppliesAPISIX317StreamingFlushIntervalDefault mirrors
// t/plugin/ai-proxy-flush.t TEST 7.  APISIX injects the default while checking
// the schema; the Go lifecycle materializes the same default in PostInit.
func TestPostInitAppliesAPISIX317StreamingFlushIntervalDefault(t *testing.T) {
	p := newTestPlugin(t, Config{
		Provider: "openai",
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer x"}},
		Options:  map[string]any{"model": "gpt-4"},
	})
	if p.config.StreamingFlushIntervalMS == nil {
		t.Fatal("streaming_flush_interval_ms default is nil")
	}
	if got := *p.config.StreamingFlushIntervalMS; got != 10 {
		t.Fatalf("streaming_flush_interval_ms default = %d, want 10", got)
	}
}

// TestSchemaRejectsAPISIX317ZeroStreamDuration mirrors
// t/plugin/ai-proxy-stream-limits.t TEST 7.  max_stream_duration_ms is a
// positive duration when configured; zero must be rejected by plugin schema.
func TestSchemaRejectsAPISIX317ZeroStreamDuration(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{
		"provider":               "openai",
		"auth":                   map[string]any{"header": map[string]any{"Authorization": "Bearer x"}},
		"options":                map[string]any{"model": "gpt-3.5-turbo"},
		"max_stream_duration_ms": 0,
	}
	if err := util.Validate(config, p.GetSchema()); err == nil {
		t.Fatal("zero max_stream_duration_ms was accepted")
	}
}

// TestHandlerPreservesDefaultOpenAIUnauthorized mirrors
// t/plugin/ai-proxy2.t TEST 5 and TEST 6 without requiring a public network
// hop. The production handler still has to select the official endpoint,
// apply the configured credential, and pass the provider's 401 through.
func TestHandlerPreservesDefaultOpenAIUnauthorized(t *testing.T) {
	p := newTestPlugin(t, Config{
		Provider: "openai",
		Auth:     Auth{Header: map[string]string{"Authorization": "some-key"}},
		Options:  map[string]any{"model": "gpt-4"},
	})

	var providerURL string
	p.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		providerURL = request.URL.String()
		if got := request.Header.Get("Authorization"); got != "some-key" {
			t.Errorf("provider Authorization = %q, want configured key", got)
		}
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": {"text/plain"}},
			Body:       io.NopCloser(strings.NewReader("Unauthorized")),
			Request:    request,
		}, nil
	})

	request := httptest.NewRequest(
		http.MethodPost,
		"/anything",
		strings.NewReader(`{"messages":[{"role":"user","content":"What is 1+1?"}]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(response, request)

	if providerURL != "https://api.openai.com/v1/chat/completions" {
		t.Fatalf("provider URL = %q, want official OpenAI chat endpoint", providerURL)
	}
	if response.Code != http.StatusUnauthorized || response.Body.String() != "Unauthorized" {
		t.Fatalf("response = %d %q, want 401 Unauthorized", response.Code, response.Body.String())
	}
}
