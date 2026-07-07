package gzip

import (
	"bytes"
	cgzip "compress/gzip"
	"io"
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

func TestPostInitAcceptsWildcardTypesString(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Parse(map[string]any{"types": "*", "min_length": 1}, p.Config()); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/json", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})).ServeHTTP(res, req)

	if got := res.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if decoded := decodeGzip(t, res.Body.Bytes()); decoded != `{"ok":true}` {
		t.Fatalf("decoded body = %q, want JSON", decoded)
	}
}

func TestHandlerSkipsSmallContentLength(t *testing.T) {
	p := newTestPlugin(t, Config{Types: []string{"text/plain"}, MinLength: intPtr(10)})
	req := httptest.NewRequest(http.MethodGet, "/text", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "5")
		_, _ = w.Write([]byte("hello"))
	})).ServeHTTP(res, req)

	if got := res.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty", got)
	}
	if got := res.Body.String(); got != "hello" {
		t.Fatalf("body = %q, want hello", got)
	}
}

func TestHandlerWildcardTypesCompressesAnyContentType(t *testing.T) {
	p := newTestPlugin(t, Config{Types: []string{"*"}, MinLength: intPtr(1)})
	req := httptest.NewRequest(http.MethodGet, "/json", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})).ServeHTTP(res, req)

	if got := res.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if decoded := decodeGzip(t, res.Body.Bytes()); decoded != `{"ok":true}` {
		t.Fatalf("decoded body = %q, want JSON", decoded)
	}
}

func decodeGzip(t *testing.T, body []byte) string {
	t.Helper()

	reader, err := cgzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create gzip reader: %v", err)
	}
	defer reader.Close()
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("decode gzip body: %v", err)
	}
	return string(decoded)
}

func intPtr(v int) *int {
	return &v
}
