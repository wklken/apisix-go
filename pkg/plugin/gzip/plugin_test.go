package gzip

import (
	"bufio"
	"bytes"
	"compress/flate"
	cgzip "compress/gzip"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
)

type fakeCapabilityWriter struct {
	flushes int
	hijacks int
}

func (w *fakeCapabilityWriter) Header() http.Header { return make(http.Header) }
func (w *fakeCapabilityWriter) WriteHeader(int)     {}
func (w *fakeCapabilityWriter) Write(p []byte) (int, error) {
	return len(p), nil
}
func (w *fakeCapabilityWriter) Flush() { w.flushes++ }
func (w *fakeCapabilityWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacks++
	return nil, nil, http.ErrNotSupported
}

func TestMaybeCompressResponseWriterPreservesCapabilities(t *testing.T) {
	fake := &fakeCapabilityWriter{}
	wrapper := &maybeCompressResponseWriter{
		ResponseWriter: fake,
		w:              fake,
		encoding:       encodingNone,
		minLength:      1,
	}

	_ = http.NewResponseController(wrapper).Flush()
	if fake.flushes != 1 {
		t.Fatalf("flushes = %d, want 1 reached through the wrapper", fake.flushes)
	}

	if _, _, err := wrapper.Hijack(); err != http.ErrNotSupported {
		t.Fatalf("Hijack() error = %v, want ErrNotSupported from fake", err)
	}
	if fake.hijacks != 1 {
		t.Fatalf("hijacks = %d, want exactly 1 delegated call", fake.hijacks)
	}
}

func TestMaybeCompressResponseWriterHijackWithoutUnderlyingSupport(t *testing.T) {
	wrapper := &maybeCompressResponseWriter{
		ResponseWriter: httptest.NewRecorder(),
		w:              httptest.NewRecorder(),
		encoding:       encodingNone,
		minLength:      1,
	}
	if _, _, err := wrapper.Hijack(); err != http.ErrNotSupported {
		t.Fatalf("Hijack() error = %v, want typed ErrNotSupported", err)
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
	p := newTestPlugin(t, Config{Types: []string{"text/plain"}, MinLength: new(10)})
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
	p := newTestPlugin(t, Config{Types: []string{"*"}, MinLength: new(1)})
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

func TestHandlerCompressesMultipleWritesOnce(t *testing.T) {
	for _, acceptEncoding := range []string{"gzip", "deflate"} {
		t.Run(acceptEncoding, func(t *testing.T) {
			p := newTestPlugin(t, Config{Types: []string{"text/plain"}, MinLength: new(1)})
			req := httptest.NewRequest(http.MethodGet, "/text", nil)
			req.Header.Set("Accept-Encoding", acceptEncoding)
			res := httptest.NewRecorder()

			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("hello "))
				_, _ = w.Write([]byte("world"))
			})).ServeHTTP(res, req)

			var decoded string
			if acceptEncoding == "gzip" {
				decoded = decodeGzip(t, res.Body.Bytes())
			} else {
				reader := flate.NewReader(bytes.NewReader(res.Body.Bytes()))
				decompressed, err := io.ReadAll(reader)
				if err != nil {
					t.Fatalf("decode deflate: %v", err)
				}
				_ = reader.Close()
				decoded = string(decompressed)
			}
			if decoded != "hello world" {
				t.Fatalf("decoded body = %q, want hello world", decoded)
			}
		})
	}
}

func TestHandlerHonorsAcceptEncodingQuality(t *testing.T) {
	tests := []struct {
		name           string
		acceptEncoding string
		wantEncoding   string
		wantVary       bool
	}{
		{name: "explicit gzip disabled", acceptEncoding: "gzip;q=0", wantEncoding: ""},
		{name: "disabled gzip defers to deflate", acceptEncoding: "gzip;q=0, deflate;q=1", wantEncoding: "deflate", wantVary: true},
		{name: "wildcard disabled", acceptEncoding: "*;q=0", wantEncoding: ""},
		{name: "wildcard applies without explicit coding", acceptEncoding: "*;q=0.5", wantEncoding: "gzip", wantVary: true},
		{name: "higher quality coding wins", acceptEncoding: "deflate;q=0.3, gzip;q=0.8", wantEncoding: "gzip", wantVary: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{Types: []string{"text/plain"}, MinLength: new(1), Vary: new(true)})
			req := httptest.NewRequest(http.MethodGet, "/text", nil)
			req.Header.Set("Accept-Encoding", tt.acceptEncoding)
			res := httptest.NewRecorder()

			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte("compress me please"))
			})).ServeHTTP(res, req)

			if got := res.Header().Get("Content-Encoding"); got != tt.wantEncoding {
				t.Fatalf("Content-Encoding = %q, want %q", got, tt.wantEncoding)
			}
			if tt.wantVary {
				if got := res.Header().Get("Vary"); got != "Accept-Encoding" {
					t.Fatalf("Vary = %q, want Accept-Encoding", got)
				}
			} else if got := res.Header().Get("Vary"); got != "" {
				t.Fatalf("Vary = %q, want empty when not compressed", got)
			}

			switch tt.wantEncoding {
			case "gzip":
				if decoded := decodeGzip(t, res.Body.Bytes()); decoded != "compress me please" {
					t.Fatalf("decoded body = %q, want gzip-compressed body", decoded)
				}
			case "deflate":
				reader := flate.NewReader(bytes.NewReader(res.Body.Bytes()))
				defer func() { _ = reader.Close() }()
				decoded, err := io.ReadAll(reader)
				if err != nil {
					t.Fatalf("decode deflate body: %v", err)
				}
				if string(decoded) != "compress me please" {
					t.Fatalf("decoded body = %q, want deflate-compressed body", decoded)
				}
			default:
				if got := res.Body.String(); got != "compress me please" {
					t.Fatalf("body = %q, want uncompressed body", got)
				}
			}
		})
	}
}

func decodeGzip(t *testing.T, body []byte) string {
	t.Helper()

	reader, err := cgzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create gzip reader: %v", err)
	}
	defer func() { _ = reader.Close() }()
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("decode gzip body: %v", err)
	}
	return string(decoded)
}
