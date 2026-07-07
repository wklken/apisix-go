package file_logger

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
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
	t.Cleanup(func() { _ = p.logger.Sync() })
	return p
}

func TestHandlerWritesLogWhenMatchPasses(t *testing.T) {
	path := t.TempDir() + "/access.log"
	p := newTestPlugin(t, Config{
		Path: path,
		LogFormat: map[string]string{
			"path":   "$uri",
			"status": "$status",
		},
		Match: []any{
			[]any{"uri", "==", "/orders"},
			[]any{"status", "==", 201},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/orders", nil)
	req = apisixctx.WithRequestVars(req)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterRequestVar(r, "$status", 201)
		w.WriteHeader(http.StatusCreated)
	})).ServeHTTP(rr, req)
	_ = p.logger.Sync()

	content := readLogFile(t, path)
	if !strings.Contains(content, `"path":"/orders"`) {
		t.Fatalf("log content = %q, want matched request path", content)
	}
	if !strings.Contains(content, `"status":201`) {
		t.Fatalf("log content = %q, want matched response status", content)
	}
}

func TestHandlerSkipsLogWhenMatchFails(t *testing.T) {
	path := t.TempDir() + "/access.log"
	p := newTestPlugin(t, Config{
		Path:      path,
		LogFormat: map[string]string{"path": "$uri"},
		Match:     []any{[]any{"http_x_tenant", "==", "gold"}},
	})

	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	req = apisixctx.WithRequestVars(req)
	req.Header.Set("X-Tenant", "silver")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterRequestVar(r, "$status", 200)
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rr, req)
	_ = p.logger.Sync()

	if content := readLogFile(t, path); content != "" {
		t.Fatalf("log content = %q, want no log line for non-matching request", content)
	}
}

func TestSchemaAcceptsMatchConditionsAndLogicalOperators(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"path": "/tmp/apisix-go-file-logger.log",
		"match": []any{
			[]any{"uri", "==", "/orders"},
			"AND",
			[]any{"status", ">=", 200},
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected match config: %v", err)
	}
}

func readLogFile(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	return string(content)
}
