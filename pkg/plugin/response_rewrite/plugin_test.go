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

func TestHandlerResolvesHeaderValueVariables(t *testing.T) {
	p := newTestPlugin(t, Config{
		StatusCode: 201,
		Headers: Headers{
			Set: map[string]string{"X-Rewrite-Status": "$status"},
		},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	if got := res.Header().Get("X-Rewrite-Status"); got != "201" {
		t.Fatalf("X-Rewrite-Status = %q, want 201", got)
	}
}

func TestHandlerSkipsRewriteWhenVarsDoNotMatch(t *testing.T) {
	p := newTestPlugin(t, Config{
		StatusCode: 201,
		Body:       stringPtr("rewritten"),
		Vars:       []any{[]any{"status", "==", 404}},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("upstream"))
	})

	if res.Code != http.StatusOK {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusOK)
	}
	if got := res.Body.String(); got != "upstream" {
		t.Fatalf("body = %q, want upstream", got)
	}
}

func TestHandlerAppliesRewriteWhenVarsMatchResponseStatus(t *testing.T) {
	p := newTestPlugin(t, Config{
		StatusCode: 202,
		Body:       stringPtr("accepted"),
		Vars:       []any{[]any{"status", "==", 404}},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("missing"))
	})

	if res.Code != http.StatusAccepted {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusAccepted)
	}
	if got := res.Body.String(); got != "accepted" {
		t.Fatalf("body = %q, want accepted", got)
	}
}

func TestHandlerAppliesResponseBodyFilters(t *testing.T) {
	p := newTestPlugin(t, Config{
		Filters: []Filter{
			{Regex: `token=\w+`, Replace: "token=hidden"},
			{Regex: `secret`, Replace: "redacted", Scope: "global"},
		},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "42")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("token=abc token=def secret secret"))
	})

	if got := res.Body.String(); got != "token=hidden token=def redacted redacted" {
		t.Fatalf("body = %q, want filtered body", got)
	}
	if got := res.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want removed after filters", got)
	}
}

func TestPostInitRejectsBodyAndFiltersTogether(t *testing.T) {
	p := &Plugin{
		config: Config{
			Body:    stringPtr("body"),
			Filters: []Filter{{Regex: "old", Replace: "new"}},
		},
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want body and filters conflict")
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
