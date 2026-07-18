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

func TestPostInitMatchesAPISIXDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{})

	if len(p.config.Types) != 1 || p.config.Types[0] != "text/html" {
		t.Fatalf("Types = %#v, want [text/html]", p.config.Types)
	}
	if *p.config.MinLength != 20 || *p.config.Mode != 0 || *p.config.CompLevel != 6 {
		t.Fatalf("minimum/mode/level = %d/%d/%d, want 20/0/6", *p.config.MinLength, *p.config.Mode, *p.config.CompLevel)
	}
	if *p.config.LGWin != 19 || *p.config.LGBlock != 0 {
		t.Fatalf("window/block = %d/%d, want 19/0", *p.config.LGWin, *p.config.LGBlock)
	}
	if *p.config.HTTPVersion != 1.1 {
		t.Fatalf("HTTPVersion = %g, want 1.1", *p.config.HTTPVersion)
	}
	if p.config.Vary != nil {
		t.Fatalf("Vary = %v, want unset", *p.config.Vary)
	}
}

func TestPostInitPreservesAPISIXConfigMatrix(t *testing.T) {
	trueValue := true
	tests := []struct {
		name     string
		config   Config
		mode     int
		level    int
		window   int
		block    int
		wantVary *bool
	}{
		{name: "defaults", config: Config{}, mode: 0, level: 6, window: 19, block: 0},
		{name: "mode one", config: Config{Mode: new(1)}, mode: 1, level: 6, window: 19, block: 0},
		{name: "level five", config: Config{CompLevel: new(5)}, mode: 0, level: 5, window: 19, block: 0},
		{name: "window twelve", config: Config{CompLevel: new(5), LGWin: new(12)}, mode: 0, level: 5, window: 12, block: 0},
		{name: "vary", config: Config{CompLevel: new(5), LGWin: new(12), Vary: &trueValue}, mode: 0, level: 5, window: 12, block: 0, wantVary: &trueValue},
		{name: "block sixteen", config: Config{CompLevel: new(5), LGWin: new(12), LGBlock: new(16), Vary: &trueValue}, mode: 0, level: 5, window: 12, block: 16, wantVary: &trueValue},
		{name: "mode two", config: Config{Mode: new(2), CompLevel: new(5), LGWin: new(12), LGBlock: new(16), Vary: &trueValue}, mode: 2, level: 5, window: 12, block: 16, wantVary: &trueValue},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, tt.config)
			if len(p.config.Types) != 1 || p.config.Types[0] != "text/html" || *p.config.MinLength != 20 || *p.config.HTTPVersion != 1.1 {
				t.Fatalf("shared defaults = types %#v, minimum %d, HTTP %g", p.config.Types, *p.config.MinLength, *p.config.HTTPVersion)
			}
			if *p.config.Mode != tt.mode || *p.config.CompLevel != tt.level || *p.config.LGWin != tt.window || *p.config.LGBlock != tt.block {
				t.Fatalf("mode/level/window/block = %d/%d/%d/%d, want %d/%d/%d/%d", *p.config.Mode, *p.config.CompLevel, *p.config.LGWin, *p.config.LGBlock, tt.mode, tt.level, tt.window, tt.block)
			}
			if tt.wantVary == nil {
				if p.config.Vary != nil {
					t.Fatalf("Vary = %v, want unset", *p.config.Vary)
				}
			} else if p.config.Vary == nil || *p.config.Vary != *tt.wantVary {
				t.Fatalf("Vary = %v, want %v", p.config.Vary, *tt.wantVary)
			}
		})
	}
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

func TestHandlerDoesNotSynthesizeContentLengthAfterCompression(t *testing.T) {
	p := newTestPlugin(t, Config{Types: []string{"text/plain"}, MinLength: new(1)})
	server := httptest.NewServer(p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "11")
		_, _ = w.Write([]byte("hello world"))
	})))
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Accept-Encoding", "br")
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("request server: %v", err)
	}
	defer res.Body.Close()

	if got := res.Header.Get("Content-Length"); got != "" || res.ContentLength != -1 {
		t.Fatalf("Content-Length = %q (%d), want absent", got, res.ContentLength)
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
