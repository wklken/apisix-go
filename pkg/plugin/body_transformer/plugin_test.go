package body_transformer

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestHandlerTransformsJSONRequestBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			InputFormat: "json",
			Template:    `{"full_name":"{{name}}","raw":{{_escape_json(_body)}}}`,
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"name":"alice"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		if string(body) != `{"full_name":"alice","raw":"{\"name\":\"alice\"}"}` {
			t.Fatalf("transformed body = %q", body)
		}
		if r.ContentLength != int64(len(body)) {
			t.Fatalf("ContentLength = %d, want %d", r.ContentLength, len(body))
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerUsesArgsForGETRequest(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			Template: `{"name":"{{name}}"}`,
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/anything?name=bob", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		if string(body) != `{"name":"bob"}` {
			t.Fatalf("transformed body = %q", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerSupportsBase64TemplateAndCtxVars(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			Template:         base64.StdEncoding.EncodeToString([]byte(`{"name":"{{_ctx.var.arg_name}}"}`)),
			TemplateIsBase64: true,
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/anything?name=carol", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed body: %v", err)
		}
		if string(body) != `{"name":"carol"}` {
			t.Fatalf("transformed body = %q", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestHandlerTransformsResponseBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		Response: &Transform{
			InputFormat: "json",
			Template:    `{"result":"{{message}}"}`,
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":"ok"}`))
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if rr.Body.String() != `{"result":"ok"}` {
		t.Fatalf("response body = %q, want transformed result", rr.Body.String())
	}
	if rr.Header().Get("Content-Length") != "" {
		t.Fatalf("Content-Length = %q, want empty after rewrite", rr.Header().Get("Content-Length"))
	}
}

func TestHandlerRejectsInvalidJSONRequestBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		Request: &Transform{
			InputFormat: "json",
			Template:    `{"name":"{{name}}"}`,
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/anything", strings.NewReader(`{"name"`))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "request body decode") {
		t.Fatalf("body = %q, want decode error", rr.Body.String())
	}
}
