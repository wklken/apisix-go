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

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := base.MaterializePluginSecrets(p); err != nil {
		t.Fatalf("MaterializePluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestSchemaValidatesDenyCodeBounds(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	baseConfig := map[string]any{
		"endpoint":          "https://moderation.example.com",
		"region_id":         "cn-shanghai",
		"access_key_id":     "access-key",
		"access_key_secret": "secret-key",
	}
	for _, test := range []struct {
		name    string
		value   any
		wantErr bool
	}{
		{name: "fractional", value: 200.5, wantErr: true},
		{name: "below minimum", value: 99, wantErr: true},
		{name: "informational", value: 100, wantErr: true},
		{name: "early informational", value: 103, wantErr: true},
		{name: "last informational", value: 199, wantErr: true},
		{name: "minimum", value: 200},
		{name: "maximum", value: 599},
		{name: "above maximum", value: 600, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := map[string]any{
				"endpoint":          baseConfig["endpoint"],
				"region_id":         baseConfig["region_id"],
				"access_key_id":     baseConfig["access_key_id"],
				"access_key_secret": baseConfig["access_key_secret"],
				"deny_code":         test.value,
			}
			err := util.Validate(config, p.GetSchema())
			if test.wantErr && err == nil {
				t.Fatalf("Validate() error = nil, want deny_code rejection for %v", test.value)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("Validate() error = %v, want deny_code %v accepted", err, test.value)
			}
		})
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

func TestMaterializeSecretsResolvesCredentialsAndRedactsConfig(t *testing.T) {
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
	if err := p.MaterializeSecrets(); err != nil {
		t.Fatalf("MaterializeSecrets() error = %v", err)
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

func TestHandlerRejectsOnModerationServiceErrorByDefault(t *testing.T) {
	moderation := aliyunServer(t, `{"Data":{}}`, http.StatusOK)
	defer moderation.Close()

	p := newTestPlugin(t, Config{
		Endpoint:        moderation.URL,
		RegionID:        "cn-shanghai",
		AccessKeyID:     "test-access",
		AccessKeySecret: "test-secret",
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	nextCalls := 0

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalls++
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable || nextCalls != 0 {
		t.Fatalf("response = (%d, %q), next calls = %d; want 503 and no next", rr.Code, rr.Body.String(), nextCalls)
	}
}

func TestHandlerModeratesAndPreservesSafeResponse(t *testing.T) {
	var services []string
	var contents []string
	moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		formBody, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(formBody))
		var parameters map[string]any
		_ = json.Unmarshal([]byte(form.Get("ServiceParameters")), &parameters)
		services = append(services, form.Get("Service"))
		contents = append(contents, parameters["content"].(string))
		_, _ = w.Write([]byte(`{"Data":{"RiskLevel":"low"}}`))
	}))
	defer moderation.Close()

	checkRequest := false
	p := newTestPlugin(t, Config{
		Endpoint: moderation.URL, RegionID: "cn-shanghai", AccessKeyID: "key", AccessKeySecret: "secret",
		CheckRequest: &checkRequest, CheckResponse: true,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "model":"gpt","messages":[{"role":"user","content":"hello"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"safe answer"}}]}`))
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated || rr.Body.String() != `{"choices":[{"message":{"content":"safe answer"}}]}` {
		t.Fatalf("response = (%d, %q), want preserved upstream response", rr.Code, rr.Body.String())
	}
	if len(services) != 1 || services[0] != "llm_response_moderation" || contents[0] != "safe answer" {
		t.Fatalf("response moderation calls = services %#v, contents %#v", services, contents)
	}
}

func TestHandlerReplacesRiskyResponseWithProtocolDeny(t *testing.T) {
	moderation := aliyunServer(
		t,
		`{"Data":{"RiskLevel":"high","Advice":[{"Answer":"blocked response"}]}}`,
		http.StatusOK,
	)
	defer moderation.Close()

	checkRequest := false
	p := newTestPlugin(t, Config{
		Endpoint: moderation.URL, RegionID: "cn-shanghai", AccessKeyID: "key", AccessKeySecret: "secret",
		CheckRequest: &checkRequest, CheckResponse: true, DenyCode: http.StatusUnavailableForLegalReasons,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
	  "model":"claude","messages":[{"role":"user","content":"hello"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"unsafe answer"}]}`))
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnavailableForLegalReasons || !strings.Contains(rr.Body.String(), "blocked response") ||
		!strings.Contains(rr.Body.String(), `"type":"message"`) {
		t.Fatalf("denied response = (%d, %q)", rr.Code, rr.Body.String())
	}
}

func TestHandlerSkipsResponseModerationForUpstreamError(t *testing.T) {
	moderationCalled := false
	moderation := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		moderationCalled = true
	}))
	defer moderation.Close()

	checkRequest := false
	p := newTestPlugin(t, Config{
		Endpoint: moderation.URL, RegionID: "cn-shanghai", AccessKeyID: "key", AccessKeySecret: "secret",
		CheckRequest: &checkRequest, CheckResponse: true,
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream failed"))
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway || rr.Body.String() != "upstream failed" || moderationCalled {
		t.Fatalf("error response = (%d, %q), moderationCalled = %t", rr.Code, rr.Body.String(), moderationCalled)
	}
}

func TestHandlerAddsRiskLevelToFinalStreamingPacket(t *testing.T) {
	moderation := aliyunServer(t, `{"Data":{"RiskLevel":"medium"}}`, http.StatusOK)
	defer moderation.Close()

	checkRequest := false
	p := newTestPlugin(t, Config{
		Endpoint: moderation.URL, RegionID: "cn-shanghai", AccessKeyID: "key", AccessKeySecret: "secret",
		CheckRequest: &checkRequest, CheckResponse: true, StreamCheckMode: "final_packet", RiskLevelBar: "high",
	})
	req := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "model":"gpt","stream":true,"messages":[{"role":"user","content":"hello"}]
	}`)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})).ServeHTTP(rr, req)

	if rr.Header().Get("Content-Type") != "text/event-stream" ||
		!strings.Contains(rr.Body.String(), `"risk_level":"medium"`) ||
		!strings.HasSuffix(rr.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("moderated stream = (%q, %q)", rr.Header().Get("Content-Type"), rr.Body.String())
	}
	if got := apisixctx.GetRequestVar(req, "$llm_content_risk_level"); got != "medium" {
		t.Fatalf("$llm_content_risk_level = %#v, want medium", got)
	}
}

func TestHandlerReplacesRiskyRealtimeStream(t *testing.T) {
	moderation := aliyunServer(t, `{"Data":{"RiskLevel":"max","Advice":[{"Answer":"stop stream"}]}}`, http.StatusOK)
	defer moderation.Close()

	checkRequest := false
	p := newTestPlugin(t, Config{
		Endpoint: moderation.URL, RegionID: "cn-shanghai", AccessKeyID: "key", AccessKeySecret: "secret",
		CheckRequest: &checkRequest, CheckResponse: true, StreamCheckMode: "realtime", StreamCheckCacheSize: 1,
		FailMode:     "warn",
		RiskLevelBar: "high",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "model":"gpt","stream":true,"messages":[{"role":"user","content":"hello"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"unsafe answer\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "stop stream") ||
		strings.Contains(
			rr.Body.String(),
			"unsafe answer",
		) || !strings.HasSuffix(rr.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("realtime moderated stream = (%d, %q)", rr.Code, rr.Body.String())
	}
}

func TestHandlerChecksRealtimeStreamWhenIntervalElapses(t *testing.T) {
	var moderatedContent string
	moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		formBody, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(formBody))
		var parameters map[string]any
		_ = json.Unmarshal([]byte(form.Get("ServiceParameters")), &parameters)
		moderatedContent, _ = parameters["content"].(string)
		_, _ = w.Write([]byte(`{"Data":{"RiskLevel":"high","Advice":[{"Answer":"interval blocked"}]}}`))
	}))
	defer moderation.Close()

	checkRequest := false
	p := newTestPlugin(t, Config{
		Endpoint: moderation.URL, RegionID: "cn-shanghai", AccessKeyID: "key", AccessKeySecret: "secret",
		CheckRequest: &checkRequest, CheckResponse: true, StreamCheckMode: "realtime",
		FailMode:             "warn",
		StreamCheckCacheSize: 128, StreamCheckInterval: 0.1, RiskLevelBar: "high",
	})
	started := time.Unix(100, 0)
	clockCalls := 0
	p.streamNow = func() time.Time {
		clockCalls++
		if clockCalls == 1 {
			return started
		}
		return started.Add(time.Second)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
	  "model":"gpt","stream":true,"messages":[{"role":"user","content":"hello"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"bad\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})).ServeHTTP(rr, req)

	if moderatedContent != "bad" || !strings.Contains(rr.Body.String(), "interval blocked") ||
		strings.Contains(rr.Body.String(), `"content":"bad"`) {
		t.Fatalf("interval moderated stream = content %q, body %q", moderatedContent, rr.Body.String())
	}
}

func TestHandlerReusesModerationSessionAcrossRequestAndResponse(t *testing.T) {
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
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"answer"}}]}`))
	})).ServeHTTP(rr, req)

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
		FailMode:             "warn",
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
		FailMode:             "warn",
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
		FailMode:             "warn",
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

func TestPostInitDefaultsFailModeToError(t *testing.T) {
	p := newTestPlugin(t, Config{
		Endpoint:        "http://127.0.0.1:1",
		RegionID:        "cn-shanghai",
		AccessKeyID:     "test-access",
		AccessKeySecret: "test-secret",
	})
	if p.config.FailMode != "error" {
		t.Fatalf("fail_mode = %q, want error", p.config.FailMode)
	}
}

func TestPostInitRejectsRealtimeResponseFailClosedMode(t *testing.T) {
	p := &Plugin{config: Config{
		Endpoint:        "http://127.0.0.1:1",
		RegionID:        "cn-shanghai",
		AccessKeyID:     "test-access",
		AccessKeySecret: "test-secret",
		CheckResponse:   true,
		StreamCheckMode: "realtime",
		FailMode:        "error",
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := base.MaterializePluginSecrets(p); err != nil {
		t.Fatalf("MaterializePluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want realtime fail-closed configuration error")
	}
}

func TestHandlerRejectsUnknownOrEmptyRequestContent(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		message string
	}{
		{name: "invalid JSON", body: `{"messages":`, message: requestInvalidPayloadMessage},
		{name: "unknown protocol", body: `{"unexpected":"value"}`, message: requestUnknownProtocolMessage},
		{
			name:    "empty extracted content",
			body:    `{"messages":[{"role":"user","content":""}]}`,
			message: requestEmptyContentMessage,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				Endpoint:        "http://127.0.0.1:1",
				RegionID:        "cn-shanghai",
				AccessKeyID:     "test-access",
				AccessKeySecret: "test-secret",
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			nextCalls := 0
			p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				nextCalls++
			})).ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest || nextCalls != 0 {
				t.Fatalf(
					"response = (%d, %q), next calls = %d; want 400 and no next",
					rr.Code,
					rr.Body.String(),
					nextCalls,
				)
			}
			if got := strings.TrimSpace(rr.Body.String()); got != `{"message":"`+tt.message+`"}` {
				t.Fatalf("response body = %q, want stable message %q", got, tt.message)
			}
		})
	}
}

func TestHandlerMapsRequestModerationFailures(t *testing.T) {
	p := newTestPlugin(t, Config{
		Endpoint:        "http://127.0.0.1:1",
		RegionID:        "cn-shanghai",
		AccessKeyID:     "test-access",
		AccessKeySecret: "test-secret",
	})
	p.client.Transport = failingRoundTripper{}
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	nextCalls := 0
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalls++ })).ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable || nextCalls != 0 {
		t.Fatalf("response = (%d, %q), next calls = %d; want 503 and no next", rr.Code, rr.Body.String(), nextCalls)
	}
}

func TestHandlerDegradesExplicitlyOnRequestModerationFailure(t *testing.T) {
	const body = `{"input":"hello"}`
	p := newTestPlugin(t, Config{
		Endpoint:        "http://127.0.0.1:1",
		RegionID:        "cn-shanghai",
		AccessKeyID:     "test-access",
		AccessKeySecret: "test-secret",
		FailMode:        "warn",
	})
	p.client.Transport = failingRoundTripper{}
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	nextCalls := 0
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalls++
		forwarded, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read forwarded request body: %v", err)
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

func TestHandlerRejectsInvalidNonStreamingUpstreamResponseWith502(t *testing.T) {
	for _, upstreamBody := range []string{
		"not-json",
		`{"choices":[42]}`,
	} {
		t.Run(upstreamBody, func(t *testing.T) {
			checkRequest := false
			p := newTestPlugin(t, Config{
				Endpoint:        "http://127.0.0.1:1",
				RegionID:        "cn-shanghai",
				AccessKeyID:     "test-access",
				AccessKeySecret: "test-secret",
				CheckRequest:    &checkRequest,
				CheckResponse:   true,
			})
			req := httptest.NewRequest(
				http.MethodPost,
				"/v1/chat/completions",
				strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`),
			)
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(upstreamBody))
			})).ServeHTTP(rr, req)
			if rr.Code != http.StatusBadGateway {
				t.Fatalf("response code = %d, want 502; body = %q", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestHandlerContinuesResponseModerationAfterRequestFailureDegradation(t *testing.T) {
	for _, failMode := range []string{"warn", "skip"} {
		t.Run(failMode, func(t *testing.T) {
			moderationCalls := 0
			moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				moderationCalls++
				if moderationCalls == 1 {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				_, _ = w.Write([]byte(`{"Data":{"RiskLevel":"high","Advice":[{"Answer":"blocked"}]}}`))
			}))
			defer moderation.Close()

			p := newTestPlugin(t, Config{
				Endpoint:        moderation.URL,
				RegionID:        "cn-shanghai",
				AccessKeyID:     "test-access",
				AccessKeySecret: "test-secret",
				CheckResponse:   true,
				FailMode:        failMode,
			})
			req := httptest.NewRequest(
				http.MethodPost,
				"/v1/chat/completions",
				strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`),
			)
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			nextCalls := 0
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				nextCalls++
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"unsafe"}}]}`))
			})).ServeHTTP(rr, req)

			if moderationCalls != 2 || nextCalls != 1 {
				t.Fatalf("moderation calls = %d, next calls = %d; want 2 and 1", moderationCalls, nextCalls)
			}
			if strings.Contains(rr.Body.String(), "unsafe") || !strings.Contains(rr.Body.String(), "blocked") {
				t.Fatalf("response body = %q, want protocol deny without unsafe content", rr.Body.String())
			}
		})
	}
}

func TestHandlerRejectsRiskyFinalPacketInsteadOfForwardingCapturedStream(t *testing.T) {
	moderation := aliyunServer(
		t,
		`{"Data":{"RiskLevel":"high","Advice":[{"Answer":"blocked"}]}}`,
		http.StatusOK,
	)
	defer moderation.Close()
	checkRequest := false
	p := newTestPlugin(t, Config{
		Endpoint:        moderation.URL,
		RegionID:        "cn-shanghai",
		AccessKeyID:     "test-access",
		AccessKeySecret: "test-secret",
		CheckRequest:    &checkRequest,
		CheckResponse:   true,
	})
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"stream":true,"messages":[{"role":"user","content":"hello"}]}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"unsafe\"}}]}\n\ndata: [DONE]\n\n"))
	})).ServeHTTP(rr, req)

	if strings.Contains(rr.Body.String(), "unsafe") || !strings.Contains(rr.Body.String(), "blocked") {
		t.Fatalf("response body = %q, want streaming deny without captured unsafe content", rr.Body.String())
	}
}

func TestHandlerRejectsMixedMalformedFinalPacketWith502(t *testing.T) {
	for _, invalidEvent := range []string{"not-json", `{"unexpected":"value"}`} {
		t.Run(invalidEvent, func(t *testing.T) {
			moderation := aliyunServer(t, `{"Data":{"RiskLevel":"low"}}`, http.StatusOK)
			defer moderation.Close()
			checkRequest := false
			p := newTestPlugin(t, Config{
				Endpoint:        moderation.URL,
				RegionID:        "cn-shanghai",
				AccessKeyID:     "test-access",
				AccessKeySecret: "test-secret",
				CheckRequest:    &checkRequest,
				CheckResponse:   true,
			})
			req := httptest.NewRequest(
				http.MethodPost,
				"/v1/chat/completions",
				strings.NewReader(`{"stream":true,"messages":[{"role":"user","content":"hello"}]}`),
			)
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(
					"data: {\"choices\":[{\"delta\":{\"content\":\"safe\"}}]}\n\ndata: " +
						invalidEvent + "\n\ndata: [DONE]\n\n",
				))
			})).ServeHTTP(rr, req)

			if rr.Code != http.StatusBadGateway || strings.Contains(rr.Body.String(), "safe") {
				t.Fatalf("response = (%d, %q), want 502 without captured stream", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestHandlerFailsClosedOnBufferedResponseModerationFailure(t *testing.T) {
	checkRequest := false
	p := newTestPlugin(t, Config{
		Endpoint:        "http://127.0.0.1:1",
		RegionID:        "cn-shanghai",
		AccessKeyID:     "test-access",
		AccessKeySecret: "test-secret",
		CheckRequest:    &checkRequest,
		CheckResponse:   true,
	})
	p.client.Transport = failingRoundTripper{}
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`),
	)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"safe"}}]}`))
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("response code = %d, want 503; body = %q", rr.Code, rr.Body.String())
	}
}

func TestModerateContentPreservesFailureClass(t *testing.T) {
	p := newTestPlugin(t, Config{
		Endpoint:        "http://127.0.0.1:1",
		RegionID:        "cn-shanghai",
		AccessKeyID:     "test-access",
		AccessKeySecret: "test-secret",
	})
	p.client.Transport = failingRoundTripper{}
	_, _, _, err := p.checkSingleContent(
		httptest.NewRequest(http.MethodPost, "/", nil),
		"session",
		"hello",
		"llm_query_moderation",
	)
	if err == nil {
		t.Fatal("checkSingleContent() error = nil, want classified moderation failure")
	}
	var classified interface{ Classify() string }
	if !errors.As(err, &classified) || classified.Classify() != "backend_unavailable" {
		t.Fatalf("error = %T %v, want backend_unavailable classification", err, err)
	}
}

func TestHandlerRecordsAliyunAllowDenyDegradedAndErrorOutcomes(t *testing.T) {
	vector := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_aliyun_safety_total"},
		[]string{"plugin", "phase", "outcome", "reason"},
	)
	previous := metrics.AISafetyOutcomes
	metrics.AISafetyOutcomes = vector
	t.Cleanup(func() { metrics.AISafetyOutcomes = previous })

	moderation := aliyunServer(t, `{"Data":{"RiskLevel":"low"}}`, http.StatusOK)
	defer moderation.Close()
	p := newTestPlugin(t, Config{
		Endpoint:        moderation.URL,
		RegionID:        "cn-shanghai",
		AccessKeyID:     "test-access",
		AccessKeySecret: "test-secret",
	})
	request := func(plugin *Plugin, body string) {
		req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		plugin.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
		})).ServeHTTP(rr, req)
	}
	request(p, `{"input":"safe"}`)

	denyModeration := aliyunServer(t, `{"Data":{"RiskLevel":"high","Advice":[{"Answer":"blocked"}]}}`, http.StatusOK)
	defer denyModeration.Close()
	deny := newTestPlugin(t, Config{
		Endpoint:        denyModeration.URL,
		RegionID:        "cn-shanghai",
		AccessKeyID:     "test-access",
		AccessKeySecret: "test-secret",
		RiskLevelBar:    "high",
	})
	request(deny, `{"input":"unsafe"}`)

	warn := newTestPlugin(t, Config{
		Endpoint:        "http://127.0.0.1:1",
		RegionID:        "cn-shanghai",
		AccessKeyID:     "test-access",
		AccessKeySecret: "test-secret",
		FailMode:        "warn",
	})
	warn.client.Transport = failingRoundTripper{}
	request(warn, `{"input":"degraded"}`)

	errorMode := newTestPlugin(t, Config{
		Endpoint:        "http://127.0.0.1:1",
		RegionID:        "cn-shanghai",
		AccessKeyID:     "test-access",
		AccessKeySecret: "test-secret",
	})
	errorMode.client.Transport = failingRoundTripper{}
	request(errorMode, `{"input":"error"}`)

	for _, labels := range [][4]string{
		{name, "request", "allow", "clean"},
		{name, "request", "deny", "risk_threshold"},
		{name, "request", "degraded", "backend_unavailable"},
		{name, "request", "error", "backend_unavailable"},
	} {
		if got := counterValue(t, vector.WithLabelValues(labels[:]...)); got != 1 {
			t.Errorf("outcome %v = %v, want 1", labels, got)
		}
	}
}

func counterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := counter.Write(metric); err != nil {
		t.Fatalf("write counter metric: %v", err)
	}
	return metric.GetCounter().GetValue()
}

func TestHandlerRecordsAliyunResponseOutcomes(t *testing.T) {
	vector := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "test_aliyun_response_safety_total"},
		[]string{"plugin", "phase", "outcome", "reason"},
	)
	previous := metrics.AISafetyOutcomes
	metrics.AISafetyOutcomes = vector
	t.Cleanup(func() { metrics.AISafetyOutcomes = previous })

	run := func(p *Plugin, upstreamBody string) {
		t.Helper()
		req := httptest.NewRequest(
			http.MethodPost,
			"/v1/chat/completions",
			strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`),
		)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(upstreamBody))
		})).ServeHTTP(rr, req)
	}

	checkRequest := false
	cleanServer := aliyunServer(t, `{"Data":{"RiskLevel":"low"}}`, http.StatusOK)
	defer cleanServer.Close()
	run(newTestPlugin(t, Config{
		Endpoint:        cleanServer.URL,
		RegionID:        "cn-shanghai",
		AccessKeyID:     "test-access",
		AccessKeySecret: "test-secret",
		CheckRequest:    &checkRequest,
		CheckResponse:   true,
	}), `{"choices":[{"message":{"content":"safe"}}]}`)

	denyServer := aliyunServer(
		t,
		`{"Data":{"RiskLevel":"high","Advice":[{"Answer":"blocked"}]}}`,
		http.StatusOK,
	)
	defer denyServer.Close()
	run(newTestPlugin(t, Config{
		Endpoint:        denyServer.URL,
		RegionID:        "cn-shanghai",
		AccessKeyID:     "test-access",
		AccessKeySecret: "test-secret",
		CheckRequest:    &checkRequest,
		CheckResponse:   true,
	}), `{"choices":[{"message":{"content":"unsafe"}}]}`)

	warn := newTestPlugin(t, Config{
		Endpoint:        "http://127.0.0.1:1",
		RegionID:        "cn-shanghai",
		AccessKeyID:     "test-access",
		AccessKeySecret: "test-secret",
		CheckRequest:    &checkRequest,
		CheckResponse:   true,
		FailMode:        "warn",
	})
	warn.client.Transport = failingRoundTripper{}
	run(warn, `{"choices":[{"message":{"content":"degraded"}}]}`)

	run(newTestPlugin(t, Config{
		Endpoint:        "http://127.0.0.1:1",
		RegionID:        "cn-shanghai",
		AccessKeyID:     "test-access",
		AccessKeySecret: "test-secret",
		CheckRequest:    &checkRequest,
		CheckResponse:   true,
	}), `{"choices":[42]}`)

	for _, labels := range [][4]string{
		{name, "response", "allow", "clean"},
		{name, "response", "deny", "risk_threshold"},
		{name, "response", "degraded", "backend_unavailable"},
		{name, "response", "error", "upstream_invalid_response"},
	} {
		if got := counterValue(t, vector.WithLabelValues(labels[:]...)); got != 1 {
			t.Errorf("response outcome %v = %v, want 1", labels, got)
		}
	}
}

func TestHandlerMapsBackendResponseFailures(t *testing.T) {
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
			req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"input":"hello"}`))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			nextCalls := 0
			p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalls++ })).ServeHTTP(rr, req)
			if rr.Code != http.StatusServiceUnavailable || nextCalls != 0 {
				t.Fatalf(
					"response = (%d, %q), next calls = %d; want 503 and no next",
					rr.Code,
					rr.Body.String(),
					nextCalls,
				)
			}
		})
	}
}

func TestHandlerDegradesExplicitSkipOnBackendResponseFailure(t *testing.T) {
	const body = `{"input":"hello"}`
	moderation := aliyunServer(t, `{"Data":{}}`, http.StatusOK)
	defer moderation.Close()
	p := newTestPlugin(t, Config{
		Endpoint:        moderation.URL,
		RegionID:        "cn-shanghai",
		AccessKeyID:     "test-access",
		AccessKeySecret: "test-secret",
		FailMode:        "skip",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	nextCalls := 0
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalls++
		forwarded, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read forwarded request body: %v", err)
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

func TestHandlerAcceptsValidNoTextResponses(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
	}{
		{
			name: "openai tool call",
			path: "/v1/chat/completions",
			body: `{"id":"resp-1","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		},
		{
			name: "embeddings",
			path: "/v1/embeddings",
			body: `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkRequest := false
			p := newTestPlugin(t, Config{
				Endpoint:        "http://127.0.0.1:1",
				RegionID:        "cn-shanghai",
				AccessKeyID:     "test-access",
				AccessKeySecret: "test-secret",
				CheckRequest:    &checkRequest,
				CheckResponse:   true,
			})
			requestBody := `{"messages":[{"role":"user","content":"hello"}]}`
			if tt.path == "/v1/embeddings" {
				requestBody = `{"input":"hello"}`
			}
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(requestBody))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			})).ServeHTTP(rr, req)
			if rr.Code != http.StatusOK || rr.Body.String() != tt.body {
				t.Fatalf("response = (%d, %q), want unchanged valid no-text response", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestRealtimeWriterDegradesOnModerationFailure(t *testing.T) {
	for _, failMode := range []string{"warn", "skip"} {
		t.Run(failMode, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				Endpoint:             "http://127.0.0.1:1",
				RegionID:             "cn-shanghai",
				AccessKeyID:          "test-access",
				AccessKeySecret:      "test-secret",
				CheckResponse:        true,
				StreamCheckMode:      "realtime",
				FailMode:             failMode,
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
		})
	}
}

type failingRoundTripper struct{}

func (failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("deterministic moderation transport failure")
}
