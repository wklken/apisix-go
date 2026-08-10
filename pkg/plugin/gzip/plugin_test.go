package gzip

import (
	"bufio"
	"bytes"
	cgzip "compress/gzip"
	"compress/zlib"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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
				reader, err := zlib.NewReader(bytes.NewReader(res.Body.Bytes()))
				if err != nil {
					t.Fatalf("create zlib reader: %v", err)
				}
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
		wantStatus     int
	}{
		{
			name:           "explicit gzip disabled",
			acceptEncoding: "gzip;q=0",
			wantEncoding:   "",
			wantVary:       true,
			wantStatus:     http.StatusOK,
		},
		{
			name:           "disabled gzip defers to deflate",
			acceptEncoding: "gzip;q=0, deflate;q=1",
			wantEncoding:   "deflate",
			wantVary:       true,
			wantStatus:     http.StatusOK,
		},
		{
			name:           "wildcard disabled",
			acceptEncoding: "*;q=0",
			wantEncoding:   "",
			wantVary:       true,
			wantStatus:     http.StatusNotAcceptable,
		},
		{
			name:           "wildcard applies without explicit coding",
			acceptEncoding: "*;q=0.5",
			wantEncoding:   "gzip",
			wantVary:       true,
			wantStatus:     http.StatusOK,
		},
		{
			name:           "higher quality coding wins",
			acceptEncoding: "deflate;q=0.3, gzip;q=0.8",
			wantEncoding:   "gzip",
			wantVary:       true,
			wantStatus:     http.StatusOK,
		},
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

			if res.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", res.Code, tt.wantStatus)
			}
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
				reader, err := zlib.NewReader(bytes.NewReader(res.Body.Bytes()))
				if err != nil {
					t.Fatalf("create zlib reader: %v", err)
				}
				defer func() { _ = reader.Close() }()
				decoded, err := io.ReadAll(reader)
				if err != nil {
					t.Fatalf("decode deflate body: %v", err)
				}
				if string(decoded) != "compress me please" {
					t.Fatalf("decoded body = %q, want deflate-compressed body", decoded)
				}
			default:
				wantBody := "compress me please"
				if tt.wantStatus == http.StatusNotAcceptable {
					wantBody = ""
				}
				if got := res.Body.String(); got != wantBody {
					t.Fatalf("body = %q, want %q", got, wantBody)
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

func TestNegotiationVaryIdentityAndNotAcceptable(t *testing.T) {
	tests := []struct {
		name           string
		acceptEncoding string
		status         int
		body           string
	}{
		{name: "missing", status: http.StatusOK, body: "identity"},
		{name: "disabled", acceptEncoding: "gzip;q=0", status: http.StatusOK, body: "identity"},
		{name: "not acceptable", acceptEncoding: "*;q=0", status: http.StatusNotAcceptable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{Types: []string{"text/plain"}, MinLength: new(1)})
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.acceptEncoding != "" {
				req.Header.Set("Accept-Encoding", tt.acceptEncoding)
			}
			res := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte("identity"))
			})).ServeHTTP(res, req)
			if res.Code != tt.status {
				t.Fatalf("status = %d, want %d", res.Code, tt.status)
			}
			if got := res.Header().Get("Vary"); got != "Accept-Encoding" {
				t.Fatalf("Vary = %q, want Accept-Encoding", got)
			}
			if got := res.Body.String(); got != tt.body {
				t.Fatalf("body = %q, want %q", got, tt.body)
			}
		})
	}
}

func TestNotAcceptableInvalidatesBodyDerivedHeaders(t *testing.T) {
	p := newTestPlugin(t, Config{Types: []string{"text/plain"}, MinLength: new(1)})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "*;q=0")
	res := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, field := range []string{
			"Content-Length", "Content-Range", "Content-MD5",
			"Digest", "Content-Digest", "Repr-Digest", "ETag", "Last-Modified",
		} {
			w.Header()[field] = []string{"stale"}
			w.Header()[strings.ToLower(field)] = []string{"lowercase stale"}
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("upstream"))
	})).ServeHTTP(res, req)
	if res.Code != http.StatusNotAcceptable || res.Body.Len() != 0 {
		t.Fatalf("response = %d/%d bytes, want empty 406", res.Code, res.Body.Len())
	}
	for actual := range res.Header() {
		for _, field := range []string{
			"Content-Length", "Content-Range", "Content-MD5",
			"Digest", "Content-Digest", "Repr-Digest", "ETag", "Last-Modified",
		} {
			if strings.EqualFold(actual, field) {
				t.Errorf("body-derived header %q remains on 406", actual)
			}
		}
	}
}

func TestCompressionInvalidatesBodyDerivedHeaders(t *testing.T) {
	for _, acceptEncoding := range []string{"gzip", "deflate"} {
		t.Run(acceptEncoding, func(t *testing.T) {
			p := newTestPlugin(t, Config{Types: []string{"text/plain"}, MinLength: new(1)})
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Accept-Encoding", acceptEncoding)
			res := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for _, field := range []string{
					"Content-Range", "Content-MD5", "Digest", "Content-Digest",
					"Repr-Digest", "ETag", "Last-Modified",
				} {
					w.Header().Set(field, "stale")
				}
				w.Header().Set("Content-Type", "text/plain")
				w.Header().Set("Content-Length", "11")
				_, _ = w.Write([]byte("hello world"))
			})).ServeHTTP(res, req)

			if got := res.Header().Get("Content-Encoding"); got != acceptEncoding {
				t.Fatalf("Content-Encoding = %q, want %q", got, acceptEncoding)
			}
			for _, field := range []string{
				"Content-Length", "Content-Range", "Content-MD5", "Digest",
				"Content-Digest", "Repr-Digest", "ETag", "Last-Modified",
			} {
				if values := res.Header().Values(field); len(values) != 0 {
					t.Errorf("%s = %v, want removed after compression", field, values)
				}
			}
		})
	}
}

func TestGzipStatusAndHeadSemantics(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		status     int
		contentEnc string
		wantEnc    string
		wantBody   string
	}{
		{name: "head advertises", method: http.MethodHead, status: http.StatusOK, wantEnc: "gzip"},
		{name: "switching protocols", method: http.MethodGet, status: http.StatusSwitchingProtocols},
		{name: "no content", method: http.MethodGet, status: http.StatusNoContent},
		{
			name:       "not modified preserves encoding",
			method:     http.MethodGet,
			status:     http.StatusNotModified,
			contentEnc: "br",
			wantEnc:    "br",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{Types: []string{"text/plain"}, MinLength: new(1)})
			req := httptest.NewRequest(tt.method, "/", nil)
			req.Header.Set("Accept-Encoding", "gzip")
			res := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				if tt.contentEnc != "" {
					w.Header().Set("Content-Encoding", tt.contentEnc)
				}
				w.Header().Set("Content-Length", "11")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte("hello world"))
			})).ServeHTTP(res, req)
			if got := res.Header().Get("Content-Encoding"); got != tt.wantEnc {
				t.Fatalf("Content-Encoding = %q, want %q", got, tt.wantEnc)
			}
			if got := res.Body.String(); got != tt.wantBody {
				t.Fatalf("body = %q, want %q", got, tt.wantBody)
			}
			if tt.method == http.MethodHead && res.Header().Get("Content-Length") != "" {
				t.Fatalf("HEAD Content-Length = %q, want removed", res.Header().Get("Content-Length"))
			}
		})
	}
}

func TestGzipForwardsInformationalBeforeFinal(t *testing.T) {
	p := newTestPlugin(t, Config{Types: []string{"text/plain"}, MinLength: new(1)})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	res := &statusCaptureWriter{header: make(http.Header)}
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("final"))
	})).ServeHTTP(res, req)
	if got, want := res.statuses, []int{http.StatusEarlyHints, http.StatusOK}; !slicesEqual(got, want) {
		t.Fatalf("statuses = %#v, want %#v", got, want)
	}
	if got := decodeGzip(t, res.body.Bytes()); got != "final" {
		t.Fatalf("decoded body = %q, want final", got)
	}
}

func TestGzipBodylessNegotiationPreservesStatusAndMetadata(t *testing.T) {
	for _, status := range []int{http.StatusSwitchingProtocols, http.StatusNoContent} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			p := newTestPlugin(t, Config{Types: []string{"text/plain"}, MinLength: new(1)})
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Accept-Encoding", "*;q=0")
			res := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(status)
				_, _ = w.Write([]byte("must not pass"))
			})).ServeHTTP(res, req)
			if res.Code != status {
				t.Fatalf("status = %d, want %d", res.Code, status)
			}
			if res.Body.Len() != 0 {
				t.Fatalf("body length = %d, want empty", res.Body.Len())
			}
			if got := res.Header().Get("Vary"); got != "" {
				t.Fatalf("Vary = %q, want empty", got)
			}
		})
	}
}

func TestGzipNotModifiedWithoutContentTypePreservesEncodingAndVary(t *testing.T) {
	p := newTestPlugin(t, Config{Types: []string{"text/plain"}, MinLength: new(1)})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "*;q=0")
	res := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusNotModified)
		_, _ = w.Write([]byte("must not pass"))
	})).ServeHTTP(res, req)
	if res.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", res.Code)
	}
	if got := res.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := res.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Fatalf("Vary = %q, want exactly Accept-Encoding", got)
	}
	if res.Body.Len() != 0 {
		t.Fatalf("body length = %d, want empty", res.Body.Len())
	}
}

type statusCaptureWriter struct {
	header   http.Header
	statuses []int
	body     bytes.Buffer
}

func (w *statusCaptureWriter) Header() http.Header { return w.header }
func (w *statusCaptureWriter) WriteHeader(status int) {
	w.statuses = append(w.statuses, status)
}
func (w *statusCaptureWriter) Write(body []byte) (int, error) { return w.body.Write(body) }

func slicesEqual(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
