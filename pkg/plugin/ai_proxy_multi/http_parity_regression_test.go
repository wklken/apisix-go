package ai_proxy_multi

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

func TestGeminiEmbeddingsRejectedBeforeForwarding(t *testing.T) {
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	p := newTestPlugin(t, Config{
		Instances: []Instance{{
			Name:     "gemini",
			Provider: "gemini",
			Weight:   1,
			Auth:     Auth{Header: map[string]string{"Authorization": "Bearer t"}},
			Override: Override{Endpoint: upstream.URL + "/echo"},
		}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%q, APISIX 3.17 would 400 unsupported protocol", rr.Code, rr.Body.String())
	}
	want := "provider gemini does not support openai-embeddings protocol (supported: openai-chat)"
	if rr.Body.String() != want {
		t.Fatalf("body=%q want %q", rr.Body.String(), want)
	}
	if gotBody != "" {
		t.Fatalf("upstream body=%q", gotBody)
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
		Instances: []Instance{
			{
				Name:     "one",
				Weight:   1,
				Provider: "openai-compatible",
				Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
			},
		},
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
		Instances: []Instance{
			{
				Name:     "one",
				Weight:   1,
				Provider: "openai-compatible",
				Override: Override{Endpoint: upstream.URL + "/v1/chat/completions"},
			},
		},
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
