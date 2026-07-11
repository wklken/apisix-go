package echo

import (
	"net/http"
	"net/http/httptest"
	"testing"

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

func TestHandlerReplacesResponseBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		Body: "replacement",
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "8")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte("upstream"))
	})

	if res.Code != http.StatusAccepted {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusAccepted)
	}
	if got := res.Body.String(); got != "replacement" {
		t.Fatalf("body = %q, want replacement", got)
	}
	if got := res.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want removed after body rewrite", got)
	}
}

func TestHandlerAddsBeforeAndAfterBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		BeforeBody: "before-",
		AfterBody:  "-after",
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("upstream"))
	})

	if got := res.Body.String(); got != "before-upstream-after" {
		t.Fatalf("body = %q, want before-upstream-after", got)
	}
}

func TestHandlerSetsResponseHeaders(t *testing.T) {
	p := newTestPlugin(t, Config{
		Headers: map[string]any{
			"X-Echo":  "yes",
			"X-Count": 2,
		},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("upstream"))
	})

	if got := res.Header().Get("X-Echo"); got != "yes" {
		t.Fatalf("X-Echo = %q, want yes", got)
	}
	if got := res.Header().Get("X-Count"); got != "2" {
		t.Fatalf("X-Count = %q, want 2", got)
	}
	if got := res.Body.String(); got != "upstream" {
		t.Fatalf("body = %q, want upstream", got)
	}
}

func TestSchemaMatchesOfficialBodyAndHeaderRequirements(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	tests := []struct {
		name    string
		config  map[string]any
		wantErr bool
	}{
		{
			name:    "headers only is rejected",
			config:  map[string]any{"headers": map[string]any{"X-Echo": "yes"}},
			wantErr: true,
		},
		{
			name: "string and number headers are accepted with body config",
			config: map[string]any{
				"before_body": "",
				"headers":     map[string]any{"X-Echo": "yes", "X-Count": 2},
			},
		},
		{
			name: "boolean header is rejected",
			config: map[string]any{
				"body":    "replacement",
				"headers": map[string]any{"X-Bool": true},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := util.Validate(tt.config, p.GetSchema())
			if tt.wantErr && err == nil {
				t.Fatal("Validate() error = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() error = %v, want nil", err)
			}
		})
	}
}

func performRequest(p *Plugin, upstream func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(upstream)).ServeHTTP(rr, req)
	return rr
}
