package request_validation

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
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

func TestHandlerRejectsInvalidHeaders(t *testing.T) {
	p := newTestPlugin(t, Config{
		HeaderSchema: map[string]any{
			"type":     "object",
			"required": []any{"test"},
			"properties": map[string]any{
				"test": map[string]any{
					"type": "string",
					"enum": []any{"a", "b", "c"},
				},
			},
		},
		RejectedMsg: "invalid header",
	})

	res := performRequest(p, http.MethodGet, "http://example.com/get", "", nil)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusBadRequest)
	}
	if got := strings.TrimSpace(res.Body.String()); got != "invalid header" {
		t.Fatalf("response body = %q, want invalid header", got)
	}
}

func TestHandlerAcceptsValidHeaders(t *testing.T) {
	p := newTestPlugin(t, Config{
		HeaderSchema: map[string]any{
			"type":     "object",
			"required": []any{"test"},
			"properties": map[string]any{
				"test": map[string]any{
					"type": "string",
					"enum": []any{"a", "b", "c"},
				},
			},
		},
	})

	res := performRequest(p, http.MethodGet, "http://example.com/get", "", map[string]string{"test": "a"})

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusNoContent)
	}
}

func TestHandlerValidatesHeadersBeforeBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		HeaderSchema: map[string]any{
			"type":     "object",
			"required": []any{"test"},
			"properties": map[string]any{
				"test": map[string]any{
					"type": "string",
				},
			},
		},
		BodySchema: map[string]any{
			"type":     "object",
			"required": []any{"name"},
		},
		RejectedMsg: "schema rejected",
	})

	res := performRequest(p, http.MethodPost, "http://example.com/get", "not-json", nil)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusBadRequest)
	}
	if got := strings.TrimSpace(res.Body.String()); got != "schema rejected" {
		t.Fatalf("response body = %q, want schema rejected", got)
	}
}

func TestHandlerAcceptsURLEncodedBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		BodySchema: map[string]any{
			"type":     "object",
			"required": []any{"required_payload"},
			"properties": map[string]any{
				"required_payload": map[string]any{"type": "string"},
			},
		},
	})

	res := performRequest(
		p,
		http.MethodPost,
		"http://example.com/get",
		"a=b&required_payload=hello",
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
	)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusNoContent)
	}
}

func TestHandlerAcceptsURLEncodedBodyWithCharset(t *testing.T) {
	p := newTestPlugin(t, Config{
		BodySchema: map[string]any{
			"type":     "object",
			"required": []any{"required_payload"},
			"properties": map[string]any{
				"required_payload": map[string]any{"type": "string"},
			},
		},
	})

	res := performRequest(
		p,
		http.MethodPost,
		"http://example.com/get",
		"a=b&required_payload=hello",
		map[string]string{"Content-Type": "application/x-www-form-urlencoded; charset=utf-8"},
	)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusNoContent)
	}
}

func TestHandlerAcceptsRepeatedURLEncodedBodyValues(t *testing.T) {
	p := newTestPlugin(t, Config{
		BodySchema: map[string]any{
			"type":     "object",
			"required": []any{"tag"},
			"properties": map[string]any{
				"tag": map[string]any{
					"type":     "array",
					"minItems": 2,
					"items":    map[string]any{"type": "string"},
				},
			},
		},
	})

	res := performRequest(
		p,
		http.MethodPost,
		"http://example.com/get",
		"tag=a&tag=b",
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
	)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusNoContent)
	}
}

func TestHandlerRejectsInvalidURLEncodedBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		BodySchema: map[string]any{
			"type":     "object",
			"required": []any{"required_payload"},
			"properties": map[string]any{
				"required_payload": map[string]any{"type": "string"},
			},
		},
		RejectedMsg: "invalid form body",
	})

	res := performRequest(
		p,
		http.MethodPost,
		"http://example.com/get",
		"a=b",
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
	)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusBadRequest)
	}
	if got := strings.TrimSpace(res.Body.String()); got != "invalid form body" {
		t.Fatalf("response body = %q, want invalid form body", got)
	}
}

func performRequest(
	p *Plugin,
	method string,
	rawURL string,
	body string,
	headers map[string]string,
) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, rawURL, strings.NewReader(body))
	req = apisixctx.WithRequestVars(req)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	return rr
}
