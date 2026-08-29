package gzip

import (
	"bufio"
	"bytes"
	cgzip "compress/gzip"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/compression"
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

func TestSchemaRejectsInvalidAPISIXOptions(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	tests := []struct {
		name   string
		config map[string]any
	}{
		{name: "empty types", config: map[string]any{"types": []any{}}},
		{name: "zero minimum length", config: map[string]any{"min_length": 0}},
		{name: "compression level above maximum", config: map[string]any{"comp_level": 10}},
		{name: "unsupported HTTP version", config: map[string]any{"http_version": 2}},
		{name: "zero buffer number", config: map[string]any{"buffers": map[string]any{"number": 0}}},
		{name: "zero buffer size", config: map[string]any{"buffers": map[string]any{"size": 0}}},
		{name: "numeric vary", config: map[string]any{"vary": 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := util.Validate(tt.config, p.GetSchema()); err == nil {
				t.Fatal("Validate() error = nil, want invalid configuration rejected")
			}
		})
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
	p := newTestPlugin(t, Config{Types: []string{"text/plain"}, MinLength: new(1)})
	req := httptest.NewRequest(http.MethodGet, "/text", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello "))
		_, _ = w.Write([]byte("world"))
	})).ServeHTTP(res, req)

	if decoded := decodeGzip(t, res.Body.Bytes()); decoded != "hello world" {
		t.Fatalf("decoded body = %q, want hello world", decoded)
	}
}

func TestHandlerDoesNotCompressDeflate(t *testing.T) {
	p := newTestPlugin(t, Config{Types: []string{"text/plain"}, MinLength: new(1)})
	req := httptest.NewRequest(http.MethodGet, "/text", nil)
	req.Header.Set("Accept-Encoding", "deflate")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("identity response"))
	})).ServeHTTP(res, req)

	if got := res.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want no deflate offer", got)
	}
	if got := res.Body.String(); got != "identity response" {
		t.Fatalf("body = %q, want identity response", got)
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
			wantStatus:     http.StatusOK,
		},
		{
			name:           "disabled gzip uses identity when deflate is requested",
			acceptEncoding: "gzip;q=0, deflate;q=1",
			wantEncoding:   "",
			wantStatus:     http.StatusOK,
		},
		{
			name:           "wildcard disabled",
			acceptEncoding: "*;q=0",
			wantEncoding:   "",
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

func TestNegotiationWithoutVaryIdentityAndNotAcceptable(t *testing.T) {
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
			if got := res.Header().Get("Vary"); got != "" {
				t.Fatalf("Vary = %q, want absent", got)
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
	p := newTestPlugin(t, Config{Types: []string{"text/plain"}, MinLength: new(1)})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
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

	if got := res.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	for _, field := range []string{
		"Content-Length", "Content-Range", "Content-MD5", "Digest",
		"Content-Digest", "Repr-Digest", "ETag", "Last-Modified",
	} {
		if values := res.Header().Values(field); len(values) != 0 {
			t.Errorf("%s = %v, want removed after compression", field, values)
		}
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

func TestGzipNotModifiedWithoutContentTypePreservesEncodingWithoutVary(t *testing.T) {
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
	if got := res.Header().Get("Vary"); got != "" {
		t.Fatalf("Vary = %q, want absent", got)
	}
	if res.Body.Len() != 0 {
		t.Fatalf("body length = %d, want empty", res.Body.Len())
	}
}

func TestGzipStructuralCompressionOfferWrapsOnlySelectedCoding(t *testing.T) {
	p := newTestPlugin(t, Config{Types: []string{"text/plain"}})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	registered, state := compression.Register(req)
	offers := p.RegisterCompressionOffers(registered, state)
	offer := offers[0]
	if offer.Coding != compression.Gzip || offer.Eligible == nil {
		t.Fatalf("offer = %#v, want gzip with eligibility", offer)
	}
	_, state = compression.Register(registered, offer)
	decision := state.Decide(compression.ResponseMeta{
		Method: http.MethodGet,
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"text/plain"}},
	})
	if decision.Coding != compression.Gzip {
		t.Fatalf("decision coding = %q, want gzip", decision.Coding)
	}
	underlying := httptest.NewRecorder()
	wrapped, err := p.WrapCompression(underlying, registered, state, decision)
	if err != nil {
		t.Fatalf("WrapCompression() error = %v", err)
	}
	if wrapped == nil {
		t.Fatal("WrapCompression() returned nil writer")
	}
	if _, ok := wrapped.(base.StreamingResponseFinalizer); !ok {
		t.Fatal("compression wrapper does not own FinishStreamingResponse")
	}
	wrapped.Header().Set("Content-Type", "text/plain")
	wrapped.WriteHeader(http.StatusOK)
	_, _ = wrapped.Write([]byte("streamed"))
	if err := wrapped.(base.StreamingResponseFinalizer).FinishStreamingResponse(nil); err != nil {
		t.Fatalf("FinishStreamingResponse() error = %v", err)
	}
	if got := underlying.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
}

func TestGzipStructuralOfferOnlyIncludesGzipAndHTTPVersionGate(t *testing.T) {
	p := newTestPlugin(t, Config{Types: []string{"text/plain"}})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.ProtoMajor, req.ProtoMinor = 1, 1
	req.Header.Set("Accept-Encoding", "gzip;q=0, deflate;q=1")
	registered, state := compression.Register(req)
	offers := p.RegisterCompressionOffers(registered, state)
	if len(offers) != 1 {
		t.Fatalf("offers = %#v, want only gzip", offers)
	}
	offer := offers[0]
	if offer.Coding != compression.Gzip {
		t.Fatalf("primary offer coding = %q, want gzip", offer.Coding)
	}
	_, state = compression.Register(registered, offers...)
	decision := state.Decide(compression.ResponseMeta{
		Method: http.MethodGet,
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"text/plain"}},
	})
	if decision.Coding != compression.Identity {
		t.Fatalf("decision coding = %q, want identity when only deflate is accepted", decision.Coding)
	}

	legacy := httptest.NewRequest(http.MethodGet, "/", nil)
	legacy.ProtoMajor, legacy.ProtoMinor = 1, 0
	legacy.Header.Set("Accept-Encoding", "gzip, deflate")
	legacy, legacyState := compression.Register(legacy)
	legacyOffers := p.RegisterCompressionOffers(legacy, legacyState)
	legacyOffer := legacyOffers[0]
	_, legacyState = compression.Register(legacy, legacyOffers...)
	legacyDecision := legacyState.Decide(compression.ResponseMeta{
		Method: http.MethodGet,
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"text/plain"}},
	})
	if legacyOffer.Eligible == nil || legacyDecision.Coding != compression.Identity {
		t.Fatalf("HTTP/1.0 decision = %#v, want identity", legacyDecision)
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
