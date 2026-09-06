package ai_proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/ai_stream"
)

func TestMalformedSSEContinuesForwarding(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {malformed\n\n"))
	}))
	defer upstream.Close()

	p := newTestPlugin(t, Config{
		Provider: "openai-compatible",
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer t"}},
		Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"messages":[{"role":"user","content":"hi"}],"stream":true}`,
	))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(rr, req)

	body := rr.Body.String()
	if !strings.Contains(body, `"content":"first"`) {
		t.Fatalf("body missing first event: %q", body)
	}
	if !strings.Contains(body, "{malformed") {
		t.Fatalf("malformed SSE data was dropped: %q", body)
	}
	if strings.Contains(body, "event: error") {
		t.Fatalf("unexpected terminal error event, body=%q", body)
	}
}

func TestEmbeddingsUsesChatRequestType(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","usage":{"prompt_tokens":1}}`))
	}))
	defer upstream.Close()
	p := newTestPlugin(t, Config{
		Provider: "openai-compatible",
		Override: Override{Endpoint: upstream.URL + "/v1/embeddings"},
	})
	req := apisixctx.WithRequestVars(httptest.NewRequest(
		http.MethodPost, "/anything", strings.NewReader(`{"input":"hi"}`),
	))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
	}
	if got := apisixctx.GetRequestVar(req, "$request_type"); got != "ai_chat" {
		t.Fatalf("$request_type=%#v want ai_chat", got)
	}
}

func TestGeminiEmbeddingsRejectedBeforeForwarding(t *testing.T) {
	var gotPath string
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	p := newTestPlugin(t, Config{
		Provider: "gemini",
		Auth:     Auth{Header: map[string]string{"Authorization": "Bearer t"}},
		Override: Override{Endpoint: upstream.URL + "/echo"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf(
			"gemini embeddings status=%d body=%q, APISIX 3.17 would 400 unsupported protocol",
			rr.Code,
			rr.Body.String(),
		)
	}
	if gotPath != "" {
		t.Fatalf("upstream path=%q", gotPath)
	}
	want := "provider gemini does not support openai-embeddings protocol (supported: openai-chat)"
	if rr.Body.String() != want {
		t.Fatalf("body=%q want %q", rr.Body.String(), want)
	}
	if gotBody != "" {
		t.Fatalf("upstream body=%q, want no upstream request", gotBody)
	}
}

func TestStreamDurationForwardsLateFirstChunk(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(400 * time.Millisecond):
		}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"late\"}}]}\n\n"))
	}))
	defer upstream.Close()
	flush := 0
	p := newTestPlugin(t, Config{
		Provider:                 "openai-compatible",
		Override:                 Override{Endpoint: upstream.URL + "/v1/chat/completions"},
		MaxStreamDurationMS:      50,
		StreamingFlushIntervalMS: &flush,
	})
	req := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"messages":[{"role":"user","content":"hi"}],"stream":true}`,
	)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d want 200 with late first chunk", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"content":"late"`) {
		t.Fatalf("body=%q want late first chunk", rr.Body.String())
	}
	if got := apisixctx.GetRequestVar(req, "$ai_stream_outcome"); got != string(ai_stream.StreamOutcomeCanceled) {
		t.Fatalf("outcome=%#v want canceled", got)
	}
}

func TestPassthroughUsesProviderPOST(t *testing.T) {
	var method string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	p := newTestPlugin(t, Config{
		Provider: "openai-compatible",
		Override: Override{Endpoint: upstream.URL + "/v1/images/generations"},
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/images/generations", strings.NewReader(
		`{"model":"dall-e-3","prompt":"otter"}`,
	))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(rr, req)
	if method != http.MethodPost {
		t.Fatalf("upstream method=%q, want POST", method)
	}
}

func TestStreamDurationReturns504WhenConverterSkipsChunk(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(80 * time.Millisecond):
		}
		_, _ = w.Write([]byte(": keepalive\n\n"))
	}))
	defer upstream.Close()
	flush := 0
	p := newTestPlugin(t, Config{
		Provider:                 "openai-compatible",
		Override:                 Override{Endpoint: upstream.URL + "/v1/chat/completions"},
		MaxStreamDurationMS:      50,
		StreamingFlushIntervalMS: &flush,
	})
	req := apisixctx.WithRequestVars(httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"m","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
	)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d want 504 for converter with no output", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "max_stream_duration_ms exceeded") {
		t.Fatalf("body=%q want stream duration message", rr.Body.String())
	}
	if got := apisixctx.GetRequestVar(req, "$ai_stream_outcome"); got != string(ai_stream.StreamOutcomeCanceled) {
		t.Fatalf("outcome=%#v want canceled", got)
	}
}
