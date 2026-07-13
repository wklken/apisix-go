package request_validation

import (
	"io"
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

func TestPostInitRejectsInvalidNestedSchema(t *testing.T) {
	p := &Plugin{config: Config{
		HeaderSchema: map[string]any{"type": "not-a-json-schema-type"},
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want invalid nested header schema rejected")
	}
}

func TestHandlerValidatesRepeatedHeaderValuesAsArray(t *testing.T) {
	p := newTestPlugin(t, Config{
		HeaderSchema: map[string]any{
			"type":     "object",
			"required": []any{"x-tag"},
			"properties": map[string]any{
				"x-tag": map[string]any{
					"type":     "array",
					"minItems": 2,
					"items":    map[string]any{"type": "string"},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = apisixctx.WithRequestVars(req)
	req.Header.Add("X-Tag", "one")
	req.Header.Add("X-Tag", "two")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body = %q", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func TestHandlerUsesCustomRejectedMessageForMalformedBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		BodySchema: map[string]any{
			"type": "object",
		},
		RejectedMsg: "invalid request body",
	})

	res := performRequest(p, http.MethodPost, "http://example.com/get", "{", map[string]string{
		"Content-Type": "application/json",
	})

	if res.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusBadRequest)
	}
	if got := strings.TrimSpace(res.Body.String()); got != "invalid request body" {
		t.Fatalf("response body = %q, want custom rejected message", got)
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

func TestHandlerNormalizesValidatedJSONBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		BodySchema: map[string]any{
			"type":     "object",
			"required": []any{"name"},
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
			},
		},
	})

	var upstreamBody string
	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/get",
		strings.NewReader(`{"name":"first","name":"last"}`),
	)
	req = apisixctx.WithRequestVars(req)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		upstreamBody = string(body)
		if r.ContentLength != int64(len(upstreamBody)) {
			t.Fatalf("ContentLength = %d, want %d", r.ContentLength, len(upstreamBody))
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusNoContent)
	}
	if upstreamBody != `{"name":"last"}` {
		t.Fatalf("upstream body = %q, want normalized JSON", upstreamBody)
	}
}

func TestHandlerUpdatesCachedRequestBodyAfterJSONNormalization(t *testing.T) {
	p := newTestPlugin(t, Config{
		BodySchema: map[string]any{
			"type":     "object",
			"required": []any{"name"},
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
			},
		},
	})

	var cachedBody string
	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/get",
		strings.NewReader(`{"name":"first","name":"last"}`),
	)
	req = apisixctx.WithRequestVars(req)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := apisixctx.ReadRequestBody(r)
		if err != nil {
			t.Fatalf("ReadRequestBody() error = %v", err)
		}
		cachedBody = string(body)
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusNoContent)
	}
	if cachedBody != `{"name":"last"}` {
		t.Fatalf("cached body = %q, want normalized JSON", cachedBody)
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
