package response_rewrite

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"testing"

	brotlienc "github.com/andybalholm/brotli"
	"github.com/wklken/apisix-go/pkg/util"
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

func TestHandlerDecodesGzipBodyBeforeFilters(t *testing.T) {
	p := newTestPlugin(t, Config{
		Filters: []Filter{
			{Regex: `secret`, Replace: "redacted", Scope: "global"},
		},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		body := gzipBody(t, "secret token")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", "42")
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})

	if got := res.Body.String(); got != "redacted token" {
		t.Fatalf("body = %q, want decoded and filtered body", got)
	}
	if got := res.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want removed after decoded filter rewrite", got)
	}
	if got := res.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want removed after decoded filter rewrite", got)
	}
}

func TestHandlerDecodesBrotliBodyBeforeFilters(t *testing.T) {
	p := newTestPlugin(t, Config{
		Filters: []Filter{
			{Regex: `secret`, Replace: "redacted", Scope: "global"},
		},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		body := brotliBody(t, "secret token")
		w.Header().Set("Content-Encoding", "br")
		w.Header().Set("Content-Length", "42")
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})

	if got := res.Body.String(); got != "redacted token" {
		t.Fatalf("body = %q, want decoded and filtered body", got)
	}
	if got := res.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want removed after decoded filter rewrite", got)
	}
}

func TestHandlerSkipsFiltersWhenEncodedBodyCannotBeDecoded(t *testing.T) {
	tests := []struct {
		name     string
		encoding string
	}{
		{name: "unsupported encoding", encoding: "zstd"},
		{name: "invalid brotli", encoding: "br"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				Filters: []Filter{{Regex: `secret`, Replace: "redacted", Scope: "global"}},
			})

			res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Encoding", tt.encoding)
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("secret token"))
			})

			if got := res.Body.String(); got != "secret token" {
				t.Fatalf("body = %q, want encoded body left unfiltered", got)
			}
			if got := res.Header().Get("Content-Encoding"); got != tt.encoding {
				t.Fatalf("Content-Encoding = %q, want %q", got, tt.encoding)
			}
		})
	}
}

func TestHandlerSupportsNestedRestyExpressionOperators(t *testing.T) {
	p := newTestPlugin(t, Config{
		Body: stringPtr("matched"),
		Vars: []any{
			"AND",
			[]any{"status", ">=", 200},
			[]any{"request_method", "in", []any{"GET", "HEAD"}},
			[]any{"remote_addr", "ipmatch", []any{"192.0.2.0/24"}},
			[]any{"sent_http_set_cookie", "has", "session=ok"},
			[]any{"http_x_env", "~*", "^prod$"},
			[]any{"http_x_skip", "!", "==", "yes"},
			[]any{
				"OR",
				[]any{"arg_mode", "==", "rewrite"},
				[]any{"status", "==", 500},
			},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get?mode=rewrite", nil)
	req.RemoteAddr = "192.0.2.40:12345"
	req.Header.Set("X-Env", "PrOd")
	res := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "theme=dark")
		w.Header().Add("Set-Cookie", "session=ok")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte("upstream"))
	})).ServeHTTP(res, req)

	if got := res.Body.String(); got != "matched" {
		t.Fatalf("body = %q, want nested expression rewrite", got)
	}
}

func TestPostInitRejectsInvalidVarsExpression(t *testing.T) {
	tests := []struct {
		name string
		vars []any
	}{
		{name: "unknown operator", vars: []any{[]any{"status", "bogus", 200}}},
		{name: "dangling logic", vars: []any{[]any{"status", "==", 200}, "OR"}},
		{name: "consecutive logic", vars: []any{[]any{"status", "==", 200}, "OR", "AND", []any{"status", "==", 201}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Plugin{config: Config{Vars: tt.vars}}
			if err := p.Init(); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			if err := p.PostInit(); err == nil {
				t.Fatal("PostInit() error = nil, want invalid vars expression rejected")
			}
		})
	}
}

func TestConfigAcceptsNumericHeaderValues(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Parse(map[string]any{
		"headers": map[string]any{
			"set": map[string]any{"X-Retry-After": 12},
		},
	}, p.Config()); err != nil {
		t.Fatalf("Parse() numeric header error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	if got := res.Header().Get("X-Retry-After"); got != "12" {
		t.Fatalf("X-Retry-After = %q, want 12", got)
	}
}

func TestSchemaValidatesOfficialHeaderForms(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	valid := []map[string]any{
		{"headers": map[string]any{"X-Retry-After": 12}},
		{"headers": map[string]any{"add": []any{"Set-Cookie: a=1"}}},
		{"headers": map[string]any{"set": map[string]any{"X-Retry-After": 12}}},
		{"headers": map[string]any{"remove": []any{"X-Legacy"}}},
	}
	for _, config := range valid {
		if err := util.Validate(config, p.GetSchema()); err != nil {
			t.Fatalf("Validate(%v) error = %v", config, err)
		}
	}

	invalid := []map[string]any{
		{"headers": map[string]any{"X-Enabled": true}},
		{"headers": map[string]any{"add": []any{}}},
		{"headers": map[string]any{"remove": []any{"Bad:Header"}}},
	}
	for _, config := range invalid {
		if err := util.Validate(config, p.GetSchema()); err == nil {
			t.Fatalf("Validate(%v) error = nil, want invalid header config rejected", config)
		}
	}
}

func TestPostInitRejectsInvalidBase64Body(t *testing.T) {
	p := &Plugin{config: Config{Body: stringPtr("not-base64"), BodyBase64: boolPtr(true)}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want invalid base64 rejected")
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

func gzipBody(t *testing.T, value string) []byte {
	t.Helper()

	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write([]byte(value)); err != nil {
		t.Fatalf("write gzip body: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close gzip body: %v", err)
	}
	return buf.Bytes()
}

func brotliBody(t *testing.T, value string) []byte {
	t.Helper()

	var buf bytes.Buffer
	writer := brotlienc.NewWriter(&buf)
	if _, err := writer.Write([]byte(value)); err != nil {
		t.Fatalf("write brotli body: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close brotli body: %v", err)
	}
	return buf.Bytes()
}
