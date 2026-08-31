package ai_aliyun_content_moderation

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
	"github.com/wklken/apisix-go/pkg/plugin/ai_runtime"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	secrets, scope, _, cleanup := newScopedSecretHarness(t, name, nil)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

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
	want := map[string]struct{}{
		"stream_check_mode": {}, "stream_check_cache_size": {}, "stream_check_interval": {},
		"endpoint": {}, "region_id": {}, "access_key_id": {}, "access_key_secret": {},
		"check_request": {}, "check_response": {}, "request_check_service": {},
		"request_check_length_limit": {}, "response_check_service": {},
		"response_check_length_limit": {}, "risk_level_bar": {}, "deny_code": {},
		"deny_message": {}, "timeout": {}, "keepalive_pool": {}, "keepalive": {},
		"keepalive_timeout": {}, "ssl_verify": {},
	}
	if len(properties) != len(want) {
		t.Fatalf("schema properties = %v, want APISIX fields", properties)
	}
	for field := range want {
		if _, ok := properties[field]; !ok {
			t.Errorf("schema is missing APISIX field %q", field)
		}
	}
}

func TestAPISIX317RequiresSelectedAIInstanceInRuntimeRequest(t *testing.T) {
	moderation := aliyunServer(t, `{"Data":{"RiskLevel":"low"}}`, http.StatusOK)
	defer moderation.Close()
	p := newTestPlugin(t, Config{
		Endpoint: moderation.URL, RegionID: "cn-shanghai", AccessKeyID: "key", AccessKeySecret: "secret",
	})
	body := `{"messages":[{"role":"user","content":"safe"}]}`

	missing := apisixctx.WithRequestVars(
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)),
	)
	missing.Header.Set("Content-Type", "application/json")
	missingResponse := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called without an AI proxy selection")
	})).ServeHTTP(missingResponse, missing)
	const want = "no ai instance picked, ai-aliyun-content-moderation plugin must be used with " +
		"ai-proxy or ai-proxy-multi plugin"
	if missingResponse.Code != http.StatusInternalServerError || missingResponse.Body.String() != want {
		t.Fatalf(
			"missing selection response = (%d, %q), want (500, %q)",
			missingResponse.Code,
			missingResponse.Body.String(),
			want,
		)
	}

	selected := apisixctx.WithRequestVars(
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)),
	)
	selected = ai_runtime.WithSelectedInstanceName(selected, "openai")
	selected.Header.Set("Content-Type", "application/json")
	selectedResponse := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(selectedResponse, selected)
	if selectedResponse.Code != http.StatusAccepted {
		t.Fatalf("selected response = (%d, %q), want 202", selectedResponse.Code, selectedResponse.Body.String())
	}
}

func TestHandlerCallsAliyunAndPreservesRequestBody(t *testing.T) {
	var gotForm url.Values
	moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Fatalf("Content-Type = %q, want application/x-www-form-urlencoded", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read aliyun request body: %v", err)
		}
		gotForm, err = url.ParseQuery(string(body))
		if err != nil {
			t.Fatalf("parse aliyun form: %v", err)
		}
		if got := gotForm.Get("Action"); got != "TextModerationPlus" {
			t.Fatalf("Action = %q, want TextModerationPlus", got)
		}
		if got := gotForm.Get("Service"); got != "llm_query_moderation" {
			t.Fatalf("Service = %q, want llm_query_moderation", got)
		}
		if got := gotForm.Get("Signature"); got == "" {
			t.Fatal("Signature is empty")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Data":{"RiskLevel":"low","Advice":[{"Answer":"ok"}]}}`))
	}))
	defer moderation.Close()

	p := newTestPlugin(t, Config{
		Endpoint:        moderation.URL,
		RegionID:        "cn-shanghai",
		AccessKeyID:     "test-access",
		AccessKeySecret: "test-secret",
	})

	const body = `{"messages":[{"role":"system","content":"be kind"},{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
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

	var serviceParams map[string]any
	if err := json.Unmarshal([]byte(gotForm.Get("ServiceParameters")), &serviceParams); err != nil {
		t.Fatalf("decode ServiceParameters: %v", err)
	}
	if got := serviceParams["content"]; got != "be kind hello" {
		t.Fatalf("content = %q, want extracted chat messages", got)
	}
}

func TestHandlerRejectsRiskLevelAtBar(t *testing.T) {
	moderation := aliyunServer(t, `{"Data":{"RiskLevel":"high","Advice":[{"Answer":"blocked"}]}}`, http.StatusOK)
	defer moderation.Close()

	p := newTestPlugin(t, Config{
		Endpoint:        moderation.URL,
		RegionID:        "cn-shanghai",
		AccessKeyID:     "test-access",
		AccessKeySecret: "test-secret",
		RiskLevelBar:    "high",
		DenyCode:        451,
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/anything",
		strings.NewReader(`{"messages":[{"role":"user","content":"bad"}]}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called when risk reaches bar")
	})).ServeHTTP(rr, req)

	if rr.Code != 451 {
		t.Fatalf("response code = %d, want 451", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "blocked") {
		t.Fatalf("response body = %q, want Aliyun answer", rr.Body.String())
	}
}

func TestHandlerUsesConfiguredDenyMessage(t *testing.T) {
	moderation := aliyunServer(t, `{"Data":{"RiskLevel":"max","Advice":[{"Answer":"provider answer"}]}}`, http.StatusOK)
	defer moderation.Close()

	p := newTestPlugin(t, Config{
		Endpoint:        moderation.URL,
		RegionID:        "cn-shanghai",
		AccessKeyID:     "test-access",
		AccessKeySecret: "test-secret",
		DenyMessage:     "policy denied",
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"input":"bad"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called when configured deny message is used")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("response code = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "policy denied") {
		t.Fatalf("response body = %q, want configured deny message", rr.Body.String())
	}
}

func TestHandlerUsesAnthropicExtractionAndDenyShape(t *testing.T) {
	var moderatedContent string
	moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		formBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read moderation body: %v", err)
		}
		form, err := url.ParseQuery(string(formBody))
		if err != nil {
			t.Fatalf("parse moderation form: %v", err)
		}
		var parameters map[string]any
		if err := json.Unmarshal([]byte(form.Get("ServiceParameters")), &parameters); err != nil {
			t.Fatalf("decode service parameters: %v", err)
		}
		moderatedContent, _ = parameters["content"].(string)
		_, _ = w.Write([]byte(`{"Data":{"RiskLevel":"high","Advice":[{"Answer":"blocked"}]}}`))
	}))
	defer moderation.Close()
	p := newTestPlugin(t, Config{
		Endpoint: moderation.URL, RegionID: "cn-shanghai", AccessKeyID: "key", AccessKeySecret: "secret",
		RiskLevelBar: "high",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
	  "model":"claude-client",
	  "system":"system text",
	  "messages":[{"role":"user","content":[{"type":"text","text":"user text"}]}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for denied Anthropic request")
	})).ServeHTTP(rr, req)

	if moderatedContent != "system text user text" {
		t.Fatalf("moderated content = %q", moderatedContent)
	}
	var response map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode Anthropic deny response: %v", err)
	}
	if response["type"] != "message" || response["model"] != "claude-client" ||
		response["content"].([]any)[0].(map[string]any)["text"] != "blocked" {
		t.Fatalf("Anthropic deny response = %#v", response)
	}
}

func TestHandlerSplitsModerationContentByCharacters(t *testing.T) {
	contents := make([]string, 0)
	moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		formBody, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(formBody))
		var parameters map[string]any
		_ = json.Unmarshal([]byte(form.Get("ServiceParameters")), &parameters)
		contents = append(contents, parameters["content"].(string))
		_, _ = w.Write([]byte(`{"Data":{"RiskLevel":"low"}}`))
	}))
	defer moderation.Close()
	p := newTestPlugin(t, Config{
		Endpoint: moderation.URL, RegionID: "cn-shanghai", AccessKeyID: "key", AccessKeySecret: "secret",
		RequestCheckLengthLimit: 2,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"input":"你好世界"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if len(contents) != 2 || contents[0] != "你好" || contents[1] != "世界" {
		t.Fatalf("moderated chunks = %#v", contents)
	}
}

func TestHandlerReturnsStreamingDenyForStreamingChat(t *testing.T) {
	moderation := aliyunServer(t, `{"Data":{"RiskLevel":"high","Advice":[{"Answer":"blocked"}]}}`, http.StatusOK)
	defer moderation.Close()
	p := newTestPlugin(t, Config{
		Endpoint: moderation.URL, RegionID: "cn-shanghai", AccessKeyID: "key", AccessKeySecret: "secret",
		RiskLevelBar: "high",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "model":"gpt","stream":true,"messages":[{"role":"user","content":"bad"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for streaming deny")
	})).ServeHTTP(rr, req)

	if rr.Header().Get("Content-Type") != "text/event-stream" ||
		!strings.Contains(rr.Body.String(), `"object":"chat.completion.chunk"`) ||
		!strings.Contains(rr.Body.String(), "data: [DONE]") {
		t.Fatalf("streaming deny response = (%q, %q)", rr.Header().Get("Content-Type"), rr.Body.String())
	}
}

func TestAPISIX317RequestDenyUsesOpenAIModelAndUsageShape(t *testing.T) {
	moderation := aliyunServer(
		t,
		`{"Data":{"RiskLevel":"high","Advice":[{"Answer":"blocked"}]}}`,
		http.StatusOK,
	)
	defer moderation.Close()
	p := newTestPlugin(t, Config{
		Endpoint: moderation.URL, RegionID: "cn-shanghai", AccessKeyID: "key", AccessKeySecret: "secret",
		RiskLevelBar: "high", DenyCode: http.StatusBadRequest,
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-3.5-turbo","messages":[{"role":"user","content":"unsafe"}]}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler called for denied request")
	})).ServeHTTP(rr, req)

	var response map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode OpenAI deny: %v", err)
	}
	usage, _ := response["usage"].(map[string]any)
	if rr.Code != http.StatusBadRequest || response["model"] != "gpt-3.5-turbo" ||
		usage["prompt_tokens"] != float64(0) || usage["completion_tokens"] != float64(0) {
		t.Fatalf("OpenAI request deny = (%d, %#v)", rr.Code, response)
	}
}

func TestHandlerSkipsWhenCheckRequestDisabled(t *testing.T) {
	moderationCalled := false
	moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		moderationCalled = true
	}))
	defer moderation.Close()

	checkRequest := false
	p := newTestPlugin(t, Config{
		Endpoint:        moderation.URL,
		RegionID:        "cn-shanghai",
		AccessKeyID:     "test-access",
		AccessKeySecret: "test-secret",
		CheckRequest:    &checkRequest,
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"input":"skip"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", rr.Code)
	}
	if moderationCalled {
		t.Fatal("moderation server was called even though check_request=false")
	}
}

func TestScopedSecretsResolveCredentialsAndRedactConfig(t *testing.T) {
	t.Setenv("APISIX_GO_ALIYUN_ACCESS_ID", "resolved-access-id")
	t.Setenv("APISIX_GO_ALIYUN_ACCESS_SECRET", "resolved-access-secret")
	p := &Plugin{config: Config{
		Endpoint:        "https://moderation.example",
		RegionID:        "cn-shanghai",
		AccessKeyID:     "$ENV://APISIX_GO_ALIYUN_ACCESS_ID",
		AccessKeySecret: "$ENV://APISIX_GO_ALIYUN_ACCESS_SECRET",
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	secrets, scope, _, cleanup := newScopedSecretHarness(t, name, map[string]string{
		"$ENV://APISIX_GO_ALIYUN_ACCESS_ID":     "resolved-access-id",
		"$ENV://APISIX_GO_ALIYUN_ACCESS_SECRET": "resolved-access-secret",
	})
	defer cleanup()
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if strings.Contains(p.config.AccessKeyID, "resolved-access-id") ||
		strings.Contains(p.config.AccessKeySecret, "resolved-access-secret") {
		t.Fatalf("materialized config exposed plaintext: %#v", p.config)
	}
	for field, value := range map[string]string{
		"access_key_id":     p.config.AccessKeyID,
		"access_key_secret": p.config.AccessKeySecret,
	} {
		if !strings.HasPrefix(value, "plugin_config#sha256:") || len(value) != len("plugin_config#sha256:")+64 {
			t.Fatalf("%s config = %q, want content-only descriptor", field, value)
		}
		if strings.Contains(value, "APISIX_GO_ALIYUN") {
			t.Fatalf("%s config retained source reference: %q", field, value)
		}
	}
	var gotForm url.Values
	moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		formBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read moderation request body: %v", err)
			return
		}
		gotForm, err = url.ParseQuery(string(formBody))
		if err != nil {
			t.Errorf("parse moderation request form: %v", err)
			return
		}
		_, _ = w.Write([]byte(`{"Data":{"RiskLevel":"low"}}`))
	}))
	defer moderation.Close()
	p.config.Endpoint = moderation.URL
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	p.now = func() time.Time { return time.Unix(1, 0) }
	p.nonce = func() string { return "nonce" }
	statusCode, responseBody, err := p.sendModerationRequest(
		context.Background(), "session", "hello", "llm_query_moderation",
	)
	if err != nil {
		t.Fatalf("sendModerationRequest() error = %v", err)
	}
	if statusCode != http.StatusOK || string(responseBody) != `{"Data":{"RiskLevel":"low"}}` {
		t.Fatalf("moderation response = (%d, %q), want 200 and JSON body", statusCode, responseBody)
	}
	if got := gotForm.Get("AccessKeyId"); got != "resolved-access-id" {
		t.Fatalf("AccessKeyId = %q, want resolved-access-id", got)
	}
	if got := gotForm.Get("Signature"); got == "" {
		t.Fatal("Signature is empty")
	}
	p.Stop()
	if _, _, err := p.sendModerationRequest(
		context.Background(), "session", "hello", "llm_query_moderation",
	); err == nil {
		t.Fatal("sendModerationRequest() after Stop error = nil")
	}
}

func TestAPISIX317RealtimeModerationUsesCacheSizeAndInterval(t *testing.T) {
	for _, test := range []struct {
		name          string
		cacheSize     int
		interval      float64
		advanceClock  bool
		wantModerates int
	}{
		{name: "final close", cacheSize: 30000, interval: 30, wantModerates: 1},
		{name: "cache size", cacheSize: 1, interval: 30, wantModerates: 3},
		{name: "interval", cacheSize: 30000, interval: 0.1, advanceClock: true, wantModerates: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			moderationCalls := 0
			moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				moderationCalls++
				_, _ = w.Write([]byte(`{"Data":{"RiskLevel":"low"}}`))
			}))
			defer moderation.Close()
			checkRequest := false
			p := newTestPlugin(t, Config{
				Endpoint: moderation.URL, RegionID: "cn-shanghai", AccessKeyID: "key", AccessKeySecret: "secret",
				CheckRequest: &checkRequest, CheckResponse: true, StreamCheckMode: "realtime",
				StreamCheckCacheSize: test.cacheSize, StreamCheckInterval: test.interval,
			})
			if test.advanceClock {
				started := time.Unix(100, 0)
				clockCalls := 0
				p.streamNow = func() time.Time {
					value := started.Add(time.Duration(clockCalls) * time.Second)
					clockCalls++
					return value
				}
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			rr := httptest.NewRecorder()
			writer := newRealtimeResponseWriter(
				rr,
				req,
				p,
				ai_protocols.OpenAIChat,
				map[string]any{"model": "gpt", "stream": true},
			)
			for _, content := range []string{"one", "two", "three"} {
				_, _ = writer.Write([]byte(
					"data: {\"choices\":[{\"delta\":{\"content\":\"" + content + "\"}}]}\n\n",
				))
			}
			_, _ = writer.Write([]byte("data: [DONE]\n\n"))
			writer.Close()
			if moderationCalls != test.wantModerates {
				t.Fatalf("moderation calls = %d, want %d", moderationCalls, test.wantModerates)
			}
		})
	}
}

func TestModerationSessionReusedAcrossRequestAndResponse(t *testing.T) {
	sessionIDs := make([]string, 0, 2)
	moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		formBody, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(formBody))
		var parameters map[string]any
		_ = json.Unmarshal([]byte(form.Get("ServiceParameters")), &parameters)
		sessionIDs = append(sessionIDs, parameters["sessionId"].(string))
		_, _ = w.Write([]byte(`{"Data":{"RiskLevel":"low"}}`))
	}))
	defer moderation.Close()

	p := newTestPlugin(t, Config{
		Endpoint: moderation.URL, RegionID: "cn-shanghai", AccessKeyID: "key", AccessKeySecret: "secret",
		CheckResponse: true,
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"question"}]}`),
	)
	req.Header.Set("Content-Type", "application/json")
	requestResponse := httptest.NewRecorder()
	result := p.RunRequestPhase(requestResponse, req)
	if result.Decision != base.RequestContinue {
		t.Fatalf("RunRequestPhase() decision = %v, want continue", result.Decision)
	}
	state := &base.ResponseState{
		Status: http.StatusCreated,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   []byte(`{"choices":[{"message":{"content":"answer"}}]}`),
	}
	if err := p.RunBufferedBodyFilter(result.Request, state); err != nil {
		t.Fatalf("RunBufferedBodyFilter() error = %v", err)
	}

	if len(sessionIDs) != 2 || sessionIDs[0] == "" || sessionIDs[0] != sessionIDs[1] {
		t.Fatalf("moderation session IDs = %#v, want one reused ID", sessionIDs)
	}
}

func TestExtractSSETextModeratesAllChoices(t *testing.T) {
	const body = "data: " + `{"choices":[{"delta":{"content":"first"}},{"delta":{"content":"second"}}]}` + "\n"
	got := extractSSEText(ai_protocols.OpenAIChat, []byte(body))
	if got != "firstsecond" {
		t.Fatalf("extractSSEText() = %q, want %q", got, "firstsecond")
	}
}

func aliyunServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		formBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read aliyun request body: %v", err)
		}
		form, err := url.ParseQuery(string(formBody))
		if err != nil {
			t.Fatalf("parse aliyun form: %v", err)
		}
		if got := form.Get("Signature"); got == "" {
			t.Fatal("Signature is empty")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestRealtimeWriterSplitsUTF8AcrossChunkBoundary(t *testing.T) {
	moderation := aliyunServer(t, `{"Data":{"RiskLevel":"low"}}`, http.StatusOK)
	defer moderation.Close()

	checkRequest := false
	p := newTestPlugin(t, Config{
		Endpoint: moderation.URL, RegionID: "cn-shanghai", AccessKeyID: "key", AccessKeySecret: "secret",
		CheckRequest: &checkRequest, CheckResponse: true, StreamCheckMode: "realtime",
		StreamCheckCacheSize: 128, RiskLevelBar: "high",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()

	writer := newRealtimeResponseWriter(rr, req, p, ai_protocols.OpenAIChat, map[string]any{"stream": true})
	// "你好" is 6 UTF-8 bytes; split it mid-character across writes.
	first := "data: {\"choices\":[{\"delta\":{\"content\":\"你"
	second := "好\"}}]}\n\n"
	if _, err := writer.Write([]byte(first)); err != nil {
		t.Fatalf("Write(first) error = %v", err)
	}
	if _, err := writer.Write([]byte(second)); err != nil {
		t.Fatalf("Write(second) error = %v", err)
	}
	writer.Close()

	if got := rr.Body.String(); got != first+second {
		t.Fatalf("forwarded body = %q, want original chunks concatenated", got)
	}
}

func TestRealtimeWriterModeratesRiskSpanningChunks(t *testing.T) {
	var moderatedContent string
	moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		formBody, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(formBody))
		var parameters map[string]any
		_ = json.Unmarshal([]byte(form.Get("ServiceParameters")), &parameters)
		moderatedContent, _ = parameters["content"].(string)
		_, _ = w.Write([]byte(`{"Data":{"RiskLevel":"max","Advice":[{"Answer":"span blocked"}]}}`))
	}))
	defer moderation.Close()

	checkRequest := false
	p := newTestPlugin(t, Config{
		Endpoint: moderation.URL, RegionID: "cn-shanghai", AccessKeyID: "key", AccessKeySecret: "secret",
		CheckRequest: &checkRequest, CheckResponse: true, StreamCheckMode: "realtime",
		StreamCheckCacheSize: 128, StreamCheckInterval: 1e9, RiskLevelBar: "high",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()

	writer := newRealtimeResponseWriter(rr, req, p, ai_protocols.OpenAIChat, map[string]any{"stream": true})
	// The risky token "unsafe" is split across two chunks; the accumulated
	// content must join them before moderation runs.
	first := "data: {\"choices\":[{\"delta\":{\"content\":\"un"
	second := "safe\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\n"
	if _, err := writer.Write([]byte(first)); err != nil {
		t.Fatalf("Write(first) error = %v", err)
	}
	if _, err := writer.Write([]byte(second)); err != nil {
		t.Fatalf("Write(second) error = %v", err)
	}
	writer.Close()

	if moderatedContent != "unsafedone" {
		t.Fatalf("moderated content = %q, want joined \"unsafedone\"", moderatedContent)
	}
	if !strings.Contains(rr.Body.String(), "span blocked") {
		t.Fatalf("blocked stream = %q, want deny message appended", rr.Body.String())
	}
}

func TestRealtimeWriterFlushesPendingOnClose(t *testing.T) {
	var moderatedContent string
	moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		formBody, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(formBody))
		var parameters map[string]any
		_ = json.Unmarshal([]byte(form.Get("ServiceParameters")), &parameters)
		moderatedContent, _ = parameters["content"].(string)
		_, _ = w.Write([]byte(`{"Data":{"RiskLevel":"high","Advice":[{"Answer":"flush blocked"}]}}`))
	}))
	defer moderation.Close()

	checkRequest := false
	p := newTestPlugin(t, Config{
		Endpoint: moderation.URL, RegionID: "cn-shanghai", AccessKeyID: "key", AccessKeySecret: "secret",
		CheckRequest: &checkRequest, CheckResponse: true, StreamCheckMode: "realtime",
		StreamCheckCacheSize: 128, StreamCheckInterval: 1e9, RiskLevelBar: "high",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()

	writer := newRealtimeResponseWriter(rr, req, p, ai_protocols.OpenAIChat, map[string]any{"stream": true})
	// The final line has no trailing newline; Close must flush and moderate it.
	partial := "data: {\"choices\":[{\"delta\":{\"content\":\"tail\"}}]}"
	if _, err := writer.Write([]byte(partial)); err != nil {
		t.Fatalf("Write(partial) error = %v", err)
	}
	writer.Close()

	if moderatedContent != "tail" {
		t.Fatalf("moderated content = %q, want flushed \"tail\"", moderatedContent)
	}
	if !strings.Contains(rr.Body.String(), "flush blocked") {
		t.Fatalf("blocked stream = %q, want deny message", rr.Body.String())
	}
}

func TestHandlerRejectsInvalidRequestJSONAndContentType(t *testing.T) {
	for _, test := range []struct {
		name        string
		body        string
		contentType string
		message     string
	}{
		{name: "invalid JSON", body: `{"messages":`, contentType: "application/json", message: requestInvalidPayloadMessage},
		{
			name: "unsupported content type", body: `{"messages":[]}`, contentType: "text/plain",
			message: "unsupported content-type: text/plain, only application/json is supported",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				Endpoint: "http://127.0.0.1:1", RegionID: "cn-shanghai",
				AccessKeyID: "test-access", AccessKeySecret: "test-secret",
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(test.body))
			req.Header.Set("Content-Type", test.contentType)
			rr := httptest.NewRecorder()
			nextCalls := 0
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				nextCalls++
				w.WriteHeader(http.StatusAccepted)
			})).ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest || nextCalls != 0 {
				t.Fatalf(
					"response = (%d, %q), next calls = %d; want 400 and no next",
					rr.Code,
					rr.Body.String(),
					nextCalls,
				)
			}
			if got := strings.TrimSpace(rr.Body.String()); got != `{"message":"`+test.message+`"}` {
				t.Fatalf("response body = %q, want stable message %q", got, test.message)
			}
		})
	}
}

func TestHandlerPassesThroughUnknownProtocol(t *testing.T) {
	p := newTestPlugin(t, Config{
		Endpoint: "http://127.0.0.1:1", RegionID: "cn-shanghai",
		AccessKeyID: "test-access", AccessKeySecret: "test-secret",
	})
	const body = `{"unexpected":"value"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	nextCalls := 0
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalls++
		forwarded, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read forwarded body: %v", err)
		}
		if string(forwarded) != body {
			t.Fatalf("forwarded body = %q, want %q", forwarded, body)
		}
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted || nextCalls != 1 {
		t.Fatalf("response = (%d, %q), next calls = %d; want 202 and one next", rr.Code, rr.Body.String(), nextCalls)
	}
}

func TestAPISIX317ModeratesOnlyTextFromMultimodalAndToolMessages(t *testing.T) {
	moderated := make([]string, 0, 3)
	moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		formBody, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(formBody))
		var parameters map[string]any
		_ = json.Unmarshal([]byte(form.Get("ServiceParameters")), &parameters)
		content, _ := parameters["content"].(string)
		moderated = append(moderated, content)
		if strings.Contains(content, "kill") {
			_, _ = w.Write([]byte(`{"Data":{"RiskLevel":"high","Advice":[{"Answer":"blocked"}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"Data":{"RiskLevel":"low"}}`))
	}))
	defer moderation.Close()
	p := newTestPlugin(t, Config{
		Endpoint: moderation.URL, RegionID: "cn-shanghai", AccessKeyID: "key", AccessKeySecret: "secret",
		RiskLevelBar: "high",
	})

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantText   string
	}{
		{
			name: "text and image denied from text",
			body: `{"messages":[{"role":"user","content":[` +
				`{"type":"text","text":"I want to kill you"},` +
				`{"type":"image_url","image_url":{"url":"data:image/jpg;base64,abc"}}]}]}`,
			wantStatus: http.StatusOK,
			wantText:   "I want to kill you",
		},
		{
			name: "safe text and image allowed",
			body: `{"messages":[{"role":"user","content":[` +
				`{"type":"text","text":"What is 1+1?"},` +
				`{"type":"image_url","image_url":{"url":"data:image/jpg;base64,abc"}}]}]}`,
			wantStatus: http.StatusAccepted,
			wantText:   "What is 1+1?",
		},
		{
			name: "tool role text is inspectable",
			body: `{"messages":[{"role":"user","content":"hello"},` +
				`{"role":"assistant","tool_calls":[{"id":"call_1","type":"function",` +
				`"function":{"name":"get_weather"}}]},` +
				`{"role":"tool","tool_call_id":"call_1","content":"sunny"}]}`,
			wantStatus: http.StatusAccepted,
			wantText:   "hello sunny",
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(test.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusAccepted)
			})).ServeHTTP(rr, req)
			if rr.Code != test.wantStatus || moderated[index] != test.wantText {
				t.Fatalf(
					"response = %d, moderated = %q; want %d and %q",
					rr.Code,
					moderated[index],
					test.wantStatus,
					test.wantText,
				)
			}
		})
	}
}

func TestAPISIX317AllowsEmptyAndImageOnlyRequestsWithoutModeration(t *testing.T) {
	moderationCalls := 0
	moderation := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		moderationCalls++
	}))
	defer moderation.Close()
	p := newTestPlugin(t, Config{
		Endpoint: moderation.URL, RegionID: "cn-shanghai", AccessKeyID: "key", AccessKeySecret: "secret",
	})

	for _, test := range []struct {
		name string
		body string
	}{
		{name: "empty string", body: `{"messages":[{"role":"user","content":""}]}`},
		{
			name: "image only",
			body: `{"messages":[{"role":"user","content":[` +
				`{"type":"image_url","image_url":{"url":"data:image/jpg;base64,abc"}}]}]}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(test.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusAccepted)
			})).ServeHTTP(rr, req)
			if rr.Code != http.StatusAccepted {
				t.Fatalf("response = (%d, %q), want 202 passthrough", rr.Code, rr.Body.String())
			}
		})
	}
	if moderationCalls != 0 {
		t.Fatalf("moderation calls for requests without text = %d, want 0", moderationCalls)
	}
}

func TestAPISIX317ResponsesAPIUsesNativeDenyWireShape(t *testing.T) {
	moderation := aliyunServer(
		t,
		`{"Data":{"RiskLevel":"high","Advice":[{"Answer":"blocked response"}]}}`,
		http.StatusOK,
	)
	defer moderation.Close()
	p := newTestPlugin(t, Config{
		Endpoint: moderation.URL, RegionID: "cn-shanghai", AccessKeyID: "key", AccessKeySecret: "secret",
		RiskLevelBar: "high",
	})

	for _, test := range []struct {
		name string
		body string
	}{
		{name: "non streaming", body: `{"input":"I want to kill you","model":"gpt-4o"}`},
		{name: "streaming", body: `{"input":"I want to kill you","model":"gpt-4o","stream":true}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(test.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("next handler called for denied Responses API request")
			})).ServeHTTP(rr, req)

			if test.name == "streaming" {
				if rr.Header().Get("Content-Type") != "text/event-stream" ||
					!strings.Contains(rr.Body.String(), "event: response.output_text.delta") ||
					!strings.Contains(rr.Body.String(), "event: response.completed") ||
					!strings.Contains(rr.Body.String(), `"object":"response"`) {
					t.Fatalf("streaming Responses deny = (%q, %q)", rr.Header().Get("Content-Type"), rr.Body.String())
				}
				return
			}
			var response map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode Responses deny: %v", err)
			}
			usage, _ := response["usage"].(map[string]any)
			if response["object"] != "response" || response["model"] != "gpt-4o" ||
				usage["input_tokens"] != float64(0) || usage["output_tokens"] != float64(0) {
				t.Fatalf("Responses deny = %#v", response)
			}
			if _, exists := usage["prompt_tokens"]; exists {
				t.Fatalf("Responses usage contains prompt_tokens: %#v", usage)
			}
		})
	}
}

func TestAPISIX317ResponsesAPIWithNonOpenAIInstanceDoesNotPanic(t *testing.T) {
	moderation := aliyunServer(t, `{"Data":{"RiskLevel":"low"}}`, http.StatusOK)
	defer moderation.Close()
	p := newTestPlugin(t, Config{
		Endpoint: moderation.URL, RegionID: "cn-shanghai", AccessKeyID: "key", AccessKeySecret: "secret",
	})
	req := apisixctx.WithRequestVars(httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"input":"safe prompt","model":"deepseek-chat"}`),
	))
	req = ai_runtime.WithSelectedInstanceName(req, "deepseek")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("non-OpenAI Responses status = %d, want downstream 400", rr.Code)
	}
}

func TestHandlerPassesThroughRequestTransportFailure(t *testing.T) {
	const requestBody = `{"input":"hello"}`
	p := newTestPlugin(t, Config{
		Endpoint:        "http://127.0.0.1:1",
		RegionID:        "cn-shanghai",
		AccessKeyID:     "test-access",
		AccessKeySecret: "test-secret",
	})
	p.client.Transport = failingRoundTripper{}
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	nextCalls := 0
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalls++
		forwarded, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read forwarded body: %v", err)
		}
		if string(forwarded) != requestBody {
			t.Fatalf("forwarded body = %q, want %q", forwarded, requestBody)
		}
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted || nextCalls != 1 {
		t.Fatalf("response = (%d, %q), next calls = %d; want 202 and one next", rr.Code, rr.Body.String(), nextCalls)
	}
}

func TestHandlerPassesThroughVendorModerationFailures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "backend 500", status: http.StatusInternalServerError, body: `{"error":"backend"}`},
		{name: "malformed JSON", status: http.StatusOK, body: `{"Data":`},
		{name: "missing risk", status: http.StatusOK, body: `{"Data":{}}`},
		{name: "unknown risk", status: http.StatusOK, body: `{"Data":{"RiskLevel":"mystery"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			moderation := aliyunServer(t, tt.body, tt.status)
			defer moderation.Close()
			p := newTestPlugin(t, Config{
				Endpoint:        moderation.URL,
				RegionID:        "cn-shanghai",
				AccessKeyID:     "test-access",
				AccessKeySecret: "test-secret",
			})
			const requestBody = `{"input":"hello"}`
			req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(requestBody))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			nextCalls := 0
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalls++
				forwarded, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read forwarded body: %v", err)
				}
				if string(forwarded) != requestBody {
					t.Fatalf("forwarded body = %q, want %q", forwarded, requestBody)
				}
				w.WriteHeader(http.StatusAccepted)
			})).ServeHTTP(rr, req)
			if rr.Code != http.StatusAccepted || nextCalls != 1 {
				t.Fatalf(
					"response = (%d, %q), next calls = %d; want 202 and one next",
					rr.Code,
					rr.Body.String(),
					nextCalls,
				)
			}
		})
	}
}

func TestRealtimeWriterPassesThroughModerationFailure(t *testing.T) {
	p := newTestPlugin(t, Config{
		Endpoint:             "http://127.0.0.1:1",
		RegionID:             "cn-shanghai",
		AccessKeyID:          "test-access",
		AccessKeySecret:      "test-secret",
		CheckResponse:        true,
		StreamCheckMode:      "realtime",
		StreamCheckCacheSize: 1,
	})
	p.client.Transport = failingRoundTripper{}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	rr := httptest.NewRecorder()
	writer := newRealtimeResponseWriter(rr, req, p, ai_protocols.OpenAIChat, map[string]any{"stream": true})
	body := `data: {"choices":[{"delta":{"content":"safe"}}]}` + "\n\n"
	if _, err := writer.Write([]byte(body)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	writer.Close()
	if rr.Code != http.StatusOK || rr.Body.String() != body {
		t.Fatalf("stream response = (%d, %q), want original body", rr.Code, rr.Body.String())
	}
}

type failingRoundTripper struct{}

func (failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("deterministic moderation transport failure")
}
