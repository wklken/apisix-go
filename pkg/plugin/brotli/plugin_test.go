package brotli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	brotlidec "github.com/andybalholm/brotli"
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

func TestHandlerCompressesMatchingResponse(t *testing.T) {
	vary := true
	p := newTestPlugin(t, Config{
		Types:     []string{"text/plain"},
		MinLength: new(5),
		Vary:      &vary,
	})
	req := httptest.NewRequest(http.MethodGet, "/text", nil)
	req.Header.Set("Accept-Encoding", "gzip, br")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Length", "11")
		w.Header().Set("Etag", `"strong"`)
		_, _ = w.Write([]byte("hello world"))
	})).ServeHTTP(res, req)

	if got := res.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("Content-Encoding = %q, want br", got)
	}
	if got := res.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want removed", got)
	}
	if got := res.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Fatalf("Vary = %q, want Accept-Encoding", got)
	}
	if got := res.Header().Get("Etag"); got != `W/"strong"` {
		t.Fatalf("Etag = %q, want weak ETag", got)
	}
	if decoded := decodeBrotli(t, res.Body.Bytes()); decoded != "hello world" {
		t.Fatalf("decoded body = %q, want hello world", decoded)
	}
}

func TestHandlerAppendsVaryToExistingResponseValue(t *testing.T) {
	vary := true
	p := newTestPlugin(t, Config{Types: []string{"text/plain"}, MinLength: new(1), Vary: &vary})
	req := httptest.NewRequest(http.MethodGet, "/text", nil)
	req.Header.Set("Accept-Encoding", "br")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Vary", "upstream")
		_, _ = w.Write([]byte("hello world"))
	})).ServeHTTP(res, req)

	if got := res.Header().Get("Vary"); got != "upstream, Accept-Encoding" {
		t.Fatalf("Vary = %q, want upstream, Accept-Encoding", got)
	}
}

func TestHandlerClearsEmbeddedQuoteETag(t *testing.T) {
	p := newTestPlugin(t, Config{Types: []string{"text/plain"}, MinLength: new(1)})
	req := httptest.NewRequest(http.MethodGet, "/text", nil)
	req.Header.Set("Accept-Encoding", "br")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Etag", `"12"34"`)
		_, _ = w.Write([]byte("hello world"))
	})).ServeHTTP(res, req)

	if got := res.Header().Get("Etag"); got != "" {
		t.Fatalf("Etag = %q, want embedded-quote ETag cleared", got)
	}
}

func TestHandlerSkipsWhenClientDoesNotAcceptBrotli(t *testing.T) {
	p := newTestPlugin(t, Config{Types: []string{"text/plain"}})
	req := httptest.NewRequest(http.MethodGet, "/text", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello world"))
	})).ServeHTTP(res, req)

	if got := res.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty", got)
	}
	if got := res.Body.String(); got != "hello world" {
		t.Fatalf("body = %q, want uncompressed", got)
	}
}

func TestHandlerSkipsSmallOrAlreadyEncodedResponses(t *testing.T) {
	tests := []struct {
		name       string
		headers    map[string]string
		body       string
		minLength  int
		wantHeader string
	}{
		{
			name:      "small response",
			headers:   map[string]string{"Content-Type": "text/plain", "Content-Length": "4"},
			body:      "tiny",
			minLength: 5,
		},
		{
			name:       "already encoded response",
			headers:    map[string]string{"Content-Type": "text/plain", "Content-Encoding": "gzip"},
			body:       "hello world",
			minLength:  5,
			wantHeader: "gzip",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				Types:     []string{"text/plain"},
				MinLength: new(tt.minLength),
			})
			req := httptest.NewRequest(http.MethodGet, "/text", nil)
			req.Header.Set("Accept-Encoding", "br")
			res := httptest.NewRecorder()

			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for key, value := range tt.headers {
					w.Header().Set(key, value)
				}
				_, _ = w.Write([]byte(tt.body))
			})).ServeHTTP(res, req)

			if got := res.Header().Get("Content-Encoding"); got != tt.wantHeader {
				t.Fatalf("Content-Encoding = %q, want %q", got, tt.wantHeader)
			}
			if got := res.Body.String(); got != tt.body {
				t.Fatalf("body = %q, want %q", got, tt.body)
			}
		})
	}
}

func TestHandlerSupportsWildcardAcceptEncodingAndType(t *testing.T) {
	p := newTestPlugin(t, Config{Types: []string{"*"}, MinLength: new(1)})
	req := httptest.NewRequest(http.MethodGet, "/json", nil)
	req.Header.Set("Accept-Encoding", "*")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})).ServeHTTP(res, req)

	if got := res.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("Content-Encoding = %q, want br", got)
	}
	if decoded := decodeBrotli(t, res.Body.Bytes()); decoded != `{"ok":true}` {
		t.Fatalf("decoded body = %q, want JSON", decoded)
	}
}

func TestConfigDecodesWildcardTypes(t *testing.T) {
	var config Config
	if err := json.Unmarshal([]byte(`{"types":"*"}`), &config); err != nil {
		t.Fatalf("json.Unmarshal() error = %v, want wildcard type accepted", err)
	}
	p := newTestPlugin(t, config)
	if !p.config.wildcardType {
		t.Fatal("wildcard type was not enabled")
	}
}

func TestHandlerAcceptsPositiveBrotliQuality(t *testing.T) {
	p := newTestPlugin(t, Config{Types: []string{"text/plain"}, MinLength: new(1)})
	req := httptest.NewRequest(http.MethodGet, "/text", nil)
	req.Header.Set("Accept-Encoding", "gzip, br;q=0.5")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello world"))
	})).ServeHTTP(res, req)

	if got := res.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("Content-Encoding = %q, want br", got)
	}
}

func TestWriterOptionsApplyCompressionLevelAndWindow(t *testing.T) {
	p := newTestPlugin(t, Config{
		CompLevel: new(9),
		LGWin:     new(22),
	})

	options := p.writerOptions()
	if options.Quality != 9 || options.LGWin != 22 {
		t.Fatalf("writer options = %#v, want quality=9 lgwin=22", options)
	}
}

func decodeBrotli(t *testing.T, body []byte) string {
	t.Helper()

	decoded, err := io.ReadAll(brotlidec.NewReader(bytes.NewReader(body)))
	if err != nil {
		t.Fatalf("decode brotli body: %v", err)
	}
	return string(decoded)
}
