package request_validation

import (
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

func performRequest(
	p *Plugin,
	method string,
	rawURL string,
	body string,
	headers map[string]string,
) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, rawURL, strings.NewReader(body))
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	return rr
}
