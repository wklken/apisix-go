package ai_aws_content_moderation

import (
	"io"
	"net/http"
	"net/http/httptest"
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
	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(body))
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
		t.Fatalf("TextSegments[0] = %#v, want original body text", segments[0])
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

	p := newTestPlugin(t, Config{Comprehend: Comprehend{
		AccessKeyID:     "test-access",
		SecretAccessKey: "test-secret",
		SessionToken:    "temporary-token",
		Region:          "us-east-1",
		Endpoint:        moderation.URL,
	}})
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"prompt":"hello"}`))
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
		ModerationThreshold: floatPtr(0.5),
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"prompt":"bad"}`))
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

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"prompt":"bad"}`))
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

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"prompt":"hello"}`))
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

func floatPtr(v float64) *float64 {
	return &v
}
