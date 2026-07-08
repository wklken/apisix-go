package file_logger

import (
	"bytes"
	"encoding/json"
	"io"
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

func TestHandlerIncludesRequestAndResponseBody(t *testing.T) {
	path := t.TempDir() + "/access.log"
	p := newTestPlugin(t, Config{
		Path:             path,
		IncludeReqBody:   true,
		IncludeRespBody:  true,
		MaxReqBodyBytes:  32,
		MaxRespBodyBytes: 32,
	})

	req := httptest.NewRequest(http.MethodPost, "/orders", bytes.NewBufferString(`{"order":1}`))
	req = apisixctx.WithRequestVars(req)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream request body: %v", err)
		}
		if string(body) != `{"order":1}` {
			t.Fatalf("upstream body = %q, want original request body", body)
		}

		apisixctx.RegisterRequestVar(r, "$status", http.StatusCreated)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})).ServeHTTP(rr, req)
	_ = p.logger.Sync()

	if rr.Code != http.StatusCreated {
		t.Fatalf("response status = %d, want %d", rr.Code, http.StatusCreated)
	}
	if body := rr.Body.String(); body != `{"ok":true}` {
		t.Fatalf("response body = %q, want upstream response body", body)
	}

	var logged map[string]any
	line := strings.TrimSpace(readLogFile(t, path))
	if err := json.Unmarshal([]byte(line), &logged); err != nil {
		t.Fatalf("decode log line %q: %v", line, err)
	}

	request, ok := logged["request"].(map[string]any)
	if !ok {
		t.Fatalf("logged request = %#v, want object", logged["request"])
	}
	if request["body"] != `{"order":1}` {
		t.Fatalf("logged request body = %#v, want original request body", request["body"])
	}

	response, ok := logged["response"].(map[string]any)
	if !ok {
		t.Fatalf("logged response = %#v, want object", logged["response"])
	}
	if response["body"] != `{"ok":true}` {
		t.Fatalf("logged response body = %#v, want upstream response body", response["body"])
	}
}

func TestHandlerIncludesBodiesWhenExpressionsMatch(t *testing.T) {
	path := t.TempDir() + "/access.log"
	p := newTestPlugin(t, Config{
		Path:                path,
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  []any{[]any{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: []any{[]any{"status", "==", "201"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
	})

	req := httptest.NewRequest(http.MethodPost, "/orders", bytes.NewBufferString(`{"order":2}`))
	req = apisixctx.WithRequestVars(req)
	req.Header.Set("X-Log-Body", "yes")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apisixctx.RegisterRequestVar(r, "$status", http.StatusCreated)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	})).ServeHTTP(rr, req)
	_ = p.logger.Sync()

	var logged map[string]any
	line := strings.TrimSpace(readLogFile(t, path))
	if err := json.Unmarshal([]byte(line), &logged); err != nil {
		t.Fatalf("decode log line %q: %v", line, err)
	}

	request, ok := logged["request"].(map[string]any)
	if !ok {
		t.Fatalf("logged request = %#v, want object", logged["request"])
	}
	if request["body"] != `{"order":2}` {
		t.Fatalf("logged request body = %#v, want captured request body", request["body"])
	}

	response, ok := logged["response"].(map[string]any)
	if !ok {
		t.Fatalf("logged response = %#v, want object", logged["response"])
	}
	if response["body"] != `{"created":true}` {
		t.Fatalf("logged response body = %#v, want captured response body", response["body"])
	}
}

func TestHandlerSkipsBodiesWhenExpressionsDoNotMatch(t *testing.T) {
	path := t.TempDir() + "/access.log"
	p := newTestPlugin(t, Config{
		Path:                path,
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  []any{[]any{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: []any{[]any{"status", "==", "500"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
	})

	req := httptest.NewRequest(http.MethodPost, "/orders", bytes.NewBufferString(`{"order":3}`))
	req = apisixctx.WithRequestVars(req)
	req.Header.Set("X-Log-Body", "no")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream request body: %v", err)
		}
		if string(body) != `{"order":3}` {
			t.Fatalf("upstream body = %q, want original request body", body)
		}

		apisixctx.RegisterRequestVar(r, "$status", http.StatusCreated)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":false}`))
	})).ServeHTTP(rr, req)
	_ = p.logger.Sync()

	var logged map[string]any
	line := strings.TrimSpace(readLogFile(t, path))
	if err := json.Unmarshal([]byte(line), &logged); err != nil {
		t.Fatalf("decode log line %q: %v", line, err)
	}
	if _, ok := logged["request"]; ok {
		t.Fatalf("logged request = %#v, want no request body", logged["request"])
	}
	if _, ok := logged["response"]; ok {
		t.Fatalf("logged response = %#v, want no response body", logged["response"])
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
