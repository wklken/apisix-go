package ai_aliyun_content_moderation

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
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

func TestHandlerPassesThroughOnModerationServiceError(t *testing.T) {
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

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("response code = %d, want 202", rr.Code)
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
