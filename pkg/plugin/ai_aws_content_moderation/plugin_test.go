package ai_aws_content_moderation

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	return newTestPluginWithScopedValues(t, cfg, nil)
}

func newTestPluginWithScopedValues(t *testing.T, cfg Config, values map[string]string) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	secrets, scope, _, cleanup := newScopedSecretHarness(t, name, values)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	return p
}

func TestSchemaMatchesAPISIXPublicFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal([]byte(p.GetSchema()), &document); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	properties, ok := document["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %#v", document["properties"])
	}
	wantTopLevel := map[string]struct{}{
		"comprehend": {}, "moderation_categories": {}, "moderation_threshold": {},
	}
	if len(properties) != len(wantTopLevel) {
		t.Fatalf("schema properties = %v, want APISIX fields", properties)
	}
	for field := range wantTopLevel {
		if _, ok := properties[field]; !ok {
			t.Fatalf("schema is missing APISIX field %q", field)
		}
	}
	comprehend, ok := properties["comprehend"].(map[string]any)
	if !ok {
		t.Fatalf("comprehend schema = %#v", properties["comprehend"])
	}
	comprehendProperties, ok := comprehend["properties"].(map[string]any)
	if !ok {
		t.Fatalf("comprehend properties = %#v", comprehend["properties"])
	}
	wantComprehend := map[string]struct{}{
		"access_key_id": {}, "secret_access_key": {}, "region": {}, "endpoint": {}, "ssl_verify": {},
	}
	if len(comprehendProperties) != len(wantComprehend) {
		t.Fatalf("comprehend properties = %v, want APISIX fields", comprehendProperties)
	}
	for field := range wantComprehend {
		if _, ok := comprehendProperties[field]; !ok {
			t.Fatalf("comprehend schema is missing APISIX field %q", field)
		}
	}
}

func TestHandlerCallsComprehendAndPreservesRequestBody(t *testing.T) {
	var gotModerationBody map[string]any
	moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Amz-Target"); got != "Comprehend_20171127.DetectToxicContent" {
			t.Fatalf("X-Amz-Target = %q, want Comprehend detect target", got)
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "AWS4-HMAC-SHA256 ") {
			t.Fatalf("Authorization = %q, want SigV4 header", got)
		}
		if got := r.Header.Get("X-Amz-Date"); got == "" {
			t.Fatal("X-Amz-Date header is empty")
		}
		if err := json.NewDecoder(r.Body).Decode(&gotModerationBody); err != nil {
			t.Fatalf("decode moderation request: %v", err)
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_, _ = w.Write([]byte(`{"ResultList":[{"Toxicity":0.1,"Labels":[{"Name":"PROFANITY","Score":0.01}]}]}`))
	}))
	defer moderation.Close()

	p := newTestPlugin(t, Config{
		Comprehend: Comprehend{
			AccessKeyID:     "test-access",
			SecretAccessKey: "test-secret",
			Region:          "us-east-1",
			Endpoint:        moderation.URL,
		},
	})

	const body = `{"messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rewound, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body in next handler: %v", err)
		}
		if string(rewound) != body {
			t.Fatalf("forwarded body = %q, want original body", string(rewound))
		}
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("response code = %d, want 202, body %q", rr.Code, rr.Body.String())
	}

	segments, ok := gotModerationBody["TextSegments"].([]any)
	if !ok || len(segments) != 1 {
		t.Fatalf("TextSegments = %#v, want one segment", gotModerationBody["TextSegments"])
	}
	segment, ok := segments[0].(map[string]any)
	if !ok || segment["Text"] != body {
		t.Fatalf("TextSegments[0] = %#v, want raw request body", segments[0])
	}
}

func TestHandlerDefaultsToAPISIX317RawBodyModerationWithoutAIInstance(t *testing.T) {
	var moderationCalls int
	moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		moderationCalls++
		var request comprehendRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode moderation request: %v", err)
		}
		if len(request.TextSegments) != 1 || request.TextSegments[0].Text != "raw request body" {
			t.Fatalf("TextSegments = %#v, want raw request body", request.TextSegments)
		}
		_, _ = w.Write([]byte(`{"ResultList":[{"Toxicity":0.1,"Labels":[]}]}`))
	}))
	defer moderation.Close()

	p := newTestPlugin(t, Config{Comprehend: Comprehend{
		AccessKeyID: "access", SecretAccessKey: "secret",
		Region: "us-east-1", Endpoint: moderation.URL,
	}})
	req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader("raw request body"))
	response := httptest.NewRecorder()
	var forwarded string
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read forwarded body: %v", err)
		}
		forwarded = string(body)
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(response, req)

	if response.Code != http.StatusAccepted || moderationCalls != 1 || forwarded != "raw request body" {
		t.Fatalf("response/calls/forwarded = %d/%d/%q, want 202/1/raw request body",
			response.Code, moderationCalls, forwarded)
	}
}

func TestHandlerAPISIX317RawBodyToxicityReturnsBadRequest(t *testing.T) {
	moderation := moderationServer(
		t,
		`{"ResultList":[{"Toxicity":0.9,"Labels":[]}]}`,
		http.StatusOK,
	)
	defer moderation.Close()
	p := newTestPlugin(t, Config{Comprehend: Comprehend{
		AccessKeyID: "access", SecretAccessKey: "secret",
		Region: "us-east-1", Endpoint: moderation.URL,
	}})
	req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader("toxic"))
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for toxic raw body")
	})).ServeHTTP(response, req)

	if response.Code != http.StatusBadRequest || response.Body.String() != "request body exceeds toxicity threshold" {
		t.Fatalf("response = %d/%q, want 400/toxicity threshold", response.Code, response.Body.String())
	}
}

func TestHandlerSignsSessionToken(t *testing.T) {
	moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Amz-Security-Token"); got != "temporary-token" {
			t.Fatalf("X-Amz-Security-Token = %q, want temporary-token", got)
		}
		if got := r.Header.Get("Authorization"); !strings.Contains(got, "x-amz-security-token") {
			t.Fatalf("Authorization = %q, want session token in SignedHeaders", got)
		}
		_, _ = w.Write([]byte(`{"ResultList":[{"Toxicity":0,"Labels":[]}]}`))
	}))
	defer moderation.Close()

	p := &Plugin{config: Config{Comprehend: Comprehend{
		AccessKeyID:     "test-access",
		SecretAccessKey: "test-secret",
		SessionToken:    "temporary-token",
		Region:          "us-east-1",
		Endpoint:        moderation.URL,
	}}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	secrets, scope, _, cleanup := newScopedSecretHarness(t, name, nil)
	defer cleanup()
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`),
	)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}
}

func TestHandlerRejectsToxicityAboveThreshold(t *testing.T) {
	moderation := moderationServer(t, `{"ResultList":[{"Toxicity":0.9,"Labels":[]}]}`, http.StatusOK)
	defer moderation.Close()

	p := newTestPlugin(t, Config{
		Comprehend: Comprehend{
			AccessKeyID:     "test-access",
			SecretAccessKey: "test-secret",
			Region:          "us-east-1",
			Endpoint:        moderation.URL,
		},
		ModerationThreshold: new(0.5),
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"bad"}]}`),
	)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called when toxicity exceeds threshold")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "toxicity threshold") {
		t.Fatalf("response body = %q, want toxicity threshold message", rr.Body.String())
	}
}

func TestHandlerRejectsConfiguredModerationCategory(t *testing.T) {
	moderation := moderationServer(
		t,
		`{"ResultList":[{"Toxicity":0.1,"Labels":[{"Name":"PROFANITY","Score":0.7}]}]}`,
		http.StatusOK,
	)
	defer moderation.Close()

	p := newTestPlugin(t, Config{
		Comprehend: Comprehend{
			AccessKeyID:     "test-access",
			SecretAccessKey: "test-secret",
			Region:          "us-east-1",
			Endpoint:        moderation.URL,
		},
		ModerationCategories: map[string]float64{"PROFANITY": 0.2},
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"bad"}]}`),
	)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called when category exceeds threshold")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "PROFANITY threshold") {
		t.Fatalf("response body = %q, want category threshold message", rr.Body.String())
	}
}

func TestHandlerReturnsServiceErrorForInvalidModerationResponse(t *testing.T) {
	moderation := moderationServer(t, `{"ResultList":[]}`, http.StatusOK)
	defer moderation.Close()

	p := newTestPlugin(t, Config{
		Comprehend: Comprehend{
			AccessKeyID:     "test-access",
			SecretAccessKey: "test-secret",
			Region:          "us-east-1",
			Endpoint:        moderation.URL,
		},
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`),
	)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called when moderation response is invalid")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("response code = %d, want 500", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "failed to get moderation results") {
		t.Fatalf("response body = %q, want moderation result message", rr.Body.String())
	}
}

func TestScopedSecretsUsePluginConfigDescriptorsAndSignResolvedCredentials(t *testing.T) {
	t.Setenv("AWS_CONTENT_MODERATION_ACCESS_KEY", "environment-access")
	t.Setenv("AWS_CONTENT_MODERATION_SECRET_KEY", "environment-secret")
	t.Setenv("AWS_CONTENT_MODERATION_SESSION_TOKEN", "environment-session-token")
	moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); !strings.Contains(got, "Credential=environment-access/") {
			t.Fatalf("Authorization = %q, want resolved access credential", got)
		}
		if got := r.Header.Get("X-Amz-Security-Token"); got != "environment-session-token" {
			t.Fatalf("X-Amz-Security-Token = %q, want resolved session token", got)
		}
		_, _ = w.Write([]byte(`{"ResultList":[{"Toxicity":0,"Labels":[]}]}`))
	}))
	defer moderation.Close()

	rawConfig := map[string]any{"comprehend": map[string]any{
		"access_key_id":     "$ENV://AWS_CONTENT_MODERATION_ACCESS_KEY",
		"secret_access_key": "$ENV://AWS_CONTENT_MODERATION_SECRET_KEY",
		"session_token":     "$ENV://AWS_CONTENT_MODERATION_SESSION_TOKEN",
		"region":            "us-east-1",
		"endpoint":          moderation.URL,
	}}
	schemaPlugin := &Plugin{}
	if err := schemaPlugin.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Validate(rawConfig, schemaPlugin.GetSchema()); err != nil {
		t.Fatalf("validate config with APISIX session_token extension: %v", err)
	}
	var config Config
	if err := util.Parse(rawConfig, &config); err != nil {
		t.Fatalf("parse config with session_token: %v", err)
	}
	p := newTestPluginWithScopedValues(t, config, map[string]string{
		"$ENV://AWS_CONTENT_MODERATION_ACCESS_KEY":    "environment-access",
		"$ENV://AWS_CONTENT_MODERATION_SECRET_KEY":    "environment-secret",
		"$ENV://AWS_CONTENT_MODERATION_SESSION_TOKEN": "environment-session-token",
	})
	p.now = func() time.Time { return time.Unix(1, 0) }

	if want := wantPluginConfigDescriptor("environment-access"); p.config.Comprehend.AccessKeyID != want {
		t.Fatalf("access key id descriptor = %q, want %q", p.config.Comprehend.AccessKeyID, want)
	}
	if want := wantPluginConfigDescriptor("environment-secret"); p.config.Comprehend.SecretAccessKey != want {
		t.Fatalf("secret access key descriptor = %q, want %q", p.config.Comprehend.SecretAccessKey, want)
	}
	if want := wantPluginConfigDescriptor("environment-session-token"); p.config.Comprehend.SessionToken != want {
		t.Fatalf("session token descriptor = %q, want %q", p.config.Comprehend.SessionToken, want)
	}
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`),
	)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}

	p.Stop()
	if p.scopedSet || p.scopedSessionTokenSet {
		t.Fatal("scoped credential state retained after Stop()")
	}
}

func moderationServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "AWS4-HMAC-SHA256 ") {
			t.Fatalf("Authorization = %q, want SigV4 header", got)
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}
