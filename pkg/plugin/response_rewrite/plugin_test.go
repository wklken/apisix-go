package response_rewrite

import (
	"net/http"
	"net/http/httptest"
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

func TestHandlerRewritesStatusAndBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		StatusCode: 201,
		Body:       stringPtr(`{"ok":true}`),
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "8")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte("upstream"))
	})

	if res.Code != http.StatusCreated {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusCreated)
	}
	if got := res.Body.String(); got != `{"ok":true}` {
		t.Fatalf("body = %q, want %q", got, `{"ok":true}`)
	}
	if got := res.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want removed after body rewrite", got)
	}
}

func TestHandlerDecodesBase64Body(t *testing.T) {
	p := newTestPlugin(t, Config{
		Body:       stringPtr("aGVsbG8="),
		BodyBase64: boolPtr(true),
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("upstream"))
	})

	if got := res.Body.String(); got != "hello" {
		t.Fatalf("body = %q, want hello", got)
	}
}

func TestHandlerAppliesHeaderOperations(t *testing.T) {
	p := newTestPlugin(t, Config{
		Headers: Headers{
			Add:    []string{"Set-Cookie: a=1", "Set-Cookie: b=2"},
			Set:    map[string]string{"X-Mode": "rewritten"},
			Remove: []string{"X-Remove"},
		},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Mode", "upstream")
		w.Header().Set("X-Remove", "delete-me")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte("upstream"))
	})

	if got := res.Header().Get("X-Mode"); got != "rewritten" {
		t.Fatalf("X-Mode = %q, want rewritten", got)
	}
	if got := res.Header().Get("X-Remove"); got != "" {
		t.Fatalf("X-Remove = %q, want removed", got)
	}
	if got := res.Header().Values("Set-Cookie"); len(got) != 2 || got[0] != "a=1" || got[1] != "b=2" {
		t.Fatalf("Set-Cookie values = %v, want [a=1 b=2]", got)
	}
	if got := res.Body.String(); got != "upstream" {
		t.Fatalf("body = %q, want upstream", got)
	}
}

func TestHandlerSupportsOldHeaderSetForm(t *testing.T) {
	p := newTestPlugin(t, Config{
		Headers: Headers{
			LegacySet: map[string]string{"X-Legacy": "yes"},
		},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("upstream"))
	})

	if got := res.Header().Get("X-Legacy"); got != "yes" {
		t.Fatalf("X-Legacy = %q, want yes", got)
	}
}

func performRequest(p *Plugin, upstream func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(upstream)).ServeHTTP(rr, req)
	return rr
}

func stringPtr(v string) *string {
	return &v
}

func boolPtr(v bool) *bool {
	return &v
}
