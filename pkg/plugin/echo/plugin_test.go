package echo

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

func TestEchoDescriptorSelectsHeaderAndBodyExactly(t *testing.T) {
	tests := []struct {
		name       string
		config     Config
		wantErr    bool
		wantHeader bool
		wantBody   bool
	}{
		{
			name:       "headers",
			config:     Config{Headers: map[string]any{"X-Echo": "yes"}},
			wantHeader: true,
		},
		{name: "body", config: Config{Body: new("body")}, wantBody: true},
		{
			name:     "all body fields",
			config:   Config{BeforeBody: "before", Body: new("body"), AfterBody: "after"},
			wantBody: true,
		},
		{
			name:       "headers and body",
			config:     Config{Headers: map[string]any{"X-Echo": "yes"}, Body: new("body")},
			wantHeader: true,
			wantBody:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin := newTestPlugin(t, tt.config)
			descriptor, err := plugin.Config().(base.BindingPhaseDescriber).DescribeBindingPhases()
			if tt.wantErr {
				if err == nil {
					t.Fatal("DescribeBindingPhases() error = nil, want schema-rejected headers-only config")
				}
				return
			}
			if err != nil {
				t.Fatalf("DescribeBindingPhases() error = %v", err)
			}
			if descriptor.RequestStage != "none" || descriptor.Header != tt.wantHeader ||
				descriptor.BufferedBody != tt.wantBody {
				t.Fatalf("descriptor = %+v, want stage=none header=%t body=%t", descriptor, tt.wantHeader, tt.wantBody)
			}
		})
	}
}

func TestEchoDescriptorTreatsSchemaValidEmptyBodyFieldAsBufferedBody(t *testing.T) {
	plugin := &Plugin{}
	if err := plugin.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{"before_body": ""}
	if err := util.Validate(config, plugin.GetSchema()); err != nil {
		t.Fatalf("Validate() error = %v, want schema-valid empty body field", err)
	}
	if err := util.Parse(config, plugin.Config()); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if err := plugin.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	descriptor, err := plugin.Config().(base.BindingPhaseDescriber).DescribeBindingPhases()
	if err != nil {
		t.Fatalf("DescribeBindingPhases() error = %v", err)
	}
	if descriptor.RequestStage != "none" || descriptor.Header || !descriptor.BufferedBody {
		t.Fatalf("descriptor = %+v, want none/body-only for present empty before_body", descriptor)
	}
}

func TestMigratedHandlersHaveNoDuplicatePostNextResponseWork(t *testing.T) {
	plugin := newTestPlugin(t, Config{Headers: map[string]any{"X-Echo": "yes"}, Body: new("replacement")})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	state := base.ResponseState{
		Status: http.StatusAccepted,
		Header: http.Header{"Content-Length": {"8"}},
		Body:   []byte("upstream"),
	}
	if err := plugin.RunHeaderFilter(req, &state); err != nil {
		t.Fatalf("RunHeaderFilter() error = %v", err)
	}
	if err := plugin.RunBufferedBodyFilter(req, &state); err != nil {
		t.Fatalf("RunBufferedBodyFilter() error = %v", err)
	}
	if got := state.Header.Get("X-Echo"); got != "yes" {
		t.Fatalf("X-Echo = %q, want yes", got)
	}
	if got := string(state.Body); got != "replacement" {
		t.Fatalf("body = %q, want replacement", got)
	}
	if got := state.Header.Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want invalidated once", got)
	}
}

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
		Body: new("replacement"),
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "8")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("upstream"))
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

func TestHandlerBodyReplacementInvalidatesRepresentationHeaders(t *testing.T) {
	p := newTestPlugin(t, Config{Body: new("replacement")})
	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		setRepresentationHeaders(w.Header())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream"))
	})

	for _, field := range representationHeaders() {
		if values := res.Header().Values(field); len(values) != 0 {
			t.Errorf("%s = %v, want removed after body replacement", field, values)
		}
	}
}

func TestHandlerAddsBeforeAndAfterBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		BeforeBody: "before-",
		AfterBody:  "-after",
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream"))
	})

	if got := res.Body.String(); got != "before-upstream-after" {
		t.Fatalf("body = %q, want before-upstream-after", got)
	}
}

func TestHandlerComposesBeforeReplacementAndAfterBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		BeforeBody: "before-",
		Body:       new("replacement"),
		AfterBody:  "-after",
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream"))
	})

	if got := res.Body.String(); got != "before-replacement-after" {
		t.Fatalf("body = %q, want before-replacement-after", got)
	}
}

func TestHandlerComposesBeforeAndAfterWithEmptyReplacement(t *testing.T) {
	p := newTestPlugin(t, Config{
		BeforeBody: "before-",
		Body:       new(""),
		AfterBody:  "-after",
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream"))
	})

	if got := res.Body.String(); got != "before--after" {
		t.Fatalf("body = %q, want before--after", got)
	}
	if got := res.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want removed after empty body replacement", got)
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
		_, _ = w.Write([]byte("upstream"))
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
			name:   "headers only is accepted",
			config: map[string]any{"headers": map[string]any{"X-Echo": "yes"}},
		},
		{
			name: "string and number headers are accepted with body config",
			config: map[string]any{
				"before_body": "",
				"headers":     map[string]any{"X-Echo": "yes", "X-Count": 2},
			},
		},
		{
			name: "non-string after body is rejected",
			config: map[string]any{
				"body":       "replacement",
				"after_body": 10,
			},
			wantErr: true,
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

func representationHeaders() []string {
	return []string{
		"Content-Length", "Content-Encoding", "Content-Range", "Content-MD5",
		"Digest", "Content-Digest", "Repr-Digest", "ETag", "Last-Modified",
	}
}

func setRepresentationHeaders(header http.Header) {
	for _, field := range representationHeaders() {
		header.Set(field, "stale")
	}
}
