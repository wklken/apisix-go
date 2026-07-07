package ai_aliyun_content_moderation

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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
