package brotli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	brotlidec "github.com/andybalholm/brotli"

	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/compression"
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

func TestPostInitMatchesAPISIXDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{})

	if len(p.config.Types) != 1 || p.config.Types[0] != "text/html" {
		t.Fatalf("Types = %#v, want [text/html]", p.config.Types)
	}
	if *p.config.MinLength != 20 || *p.config.Mode != 0 || *p.config.CompLevel != 6 {
		t.Fatalf(
			"minimum/mode/level = %d/%d/%d, want 20/0/6",
			*p.config.MinLength,
			*p.config.Mode,
			*p.config.CompLevel,
		)
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

func TestSchemaMatchesAPISIX317Rejections(t *testing.T) {
	p := newTestPlugin(t, Config{})
	if err := util.Validate(map[string]any{}, p.GetSchema()); err != nil {
		t.Fatalf("default config validation error = %v", err)
	}

	for _, tt := range []struct {
		name   string
		config map[string]any
		field  string
	}{
		{name: "empty types", config: map[string]any{"types": []any{}}, field: "types"},
		{name: "minimum length", config: map[string]any{"min_length": 0}, field: "min_length"},
		{name: "mode", config: map[string]any{"mode": 4}, field: "mode"},
		{name: "compression level", config: map[string]any{"comp_level": 12}, field: "comp_level"},
		{name: "http version", config: map[string]any{"http_version": 2}, field: "http_version"},
		{name: "window", config: map[string]any{"lgwin": 100}, field: "lgwin"},
		{name: "block", config: map[string]any{"lgblock": 8}, field: "lgblock"},
		{name: "vary type", config: map[string]any{"vary": 0}, field: "vary"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := util.Validate(tt.config, p.GetSchema())
			if err == nil || !strings.Contains(err.Error(), tt.field) {
				t.Fatalf("Validate(%v) error = %v, want %s rejection", tt.config, err, tt.field)
			}
		})
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
		{
			name:   "level five",
			config: Config{CompLevel: new(5)},
			mode:   0,
			level:  5,
			window: 19,
			block:  0,
		},
		{
			name:   "window twelve",
			config: Config{CompLevel: new(5), LGWin: new(12)},
			mode:   0,
			level:  5,
			window: 12,
			block:  0,
		},
		{
			name:     "vary",
			config:   Config{CompLevel: new(5), LGWin: new(12), Vary: &trueValue},
			mode:     0,
			level:    5,
			window:   12,
			block:    0,
			wantVary: &trueValue,
		},
		{
			name:     "block sixteen",
			config:   Config{CompLevel: new(5), LGWin: new(12), LGBlock: new(16), Vary: &trueValue},
			mode:     0,
			level:    5,
			window:   12,
			block:    16,
			wantVary: &trueValue,
		},
		{
			name: "mode two",
			config: Config{
				Mode:      new(2),
				CompLevel: new(5),
				LGWin:     new(12),
				LGBlock:   new(16),
				Vary:      &trueValue,
			},
			mode:     2,
			level:    5,
			window:   12,
			block:    16,
			wantVary: &trueValue,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, tt.config)
			if len(p.config.Types) != 1 || p.config.Types[0] != "text/html" ||
				*p.config.MinLength != 20 ||
				*p.config.HTTPVersion != 1.1 {
				t.Fatalf(
					"shared defaults = types %#v, minimum %d, HTTP %g",
					p.config.Types,
					*p.config.MinLength,
					*p.config.HTTPVersion,
				)
			}
			if *p.config.Mode != tt.mode || *p.config.CompLevel != tt.level ||
				*p.config.LGWin != tt.window ||
				*p.config.LGBlock != tt.block {
				t.Fatalf(
					"mode/level/window/block = %d/%d/%d/%d, want %d/%d/%d/%d",
					*p.config.Mode,
					*p.config.CompLevel,
					*p.config.LGWin,
					*p.config.LGBlock,
					tt.mode,
					tt.level,
					tt.window,
					tt.block,
				)
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
		w.Header().Set("Last-Modified", "Thu, 27 Nov 2025 00:32:33 GMT")
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
		t.Fatalf("Etag = %q, want downgraded weak ETag", got)
	}
	if got := res.Header().Get("Last-Modified"); got != "Thu, 27 Nov 2025 00:32:33 GMT" {
		t.Fatalf("Last-Modified = %q, want preserved", got)
	}
	if decoded := decodeBrotli(t, res.Body.Bytes()); decoded != "hello world" {
		t.Fatalf("decoded body = %q, want hello world", decoded)
	}
}

func TestHandlerDoesNotSynthesizeContentLengthAfterCompression(t *testing.T) {
	p := newTestPlugin(t, Config{Types: []string{"text/plain"}, MinLength: new(1)})
	server := httptest.NewServer(
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Length", "11")
			_, _ = w.Write([]byte("hello world"))
		})),
	)
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
	defer func() { _ = res.Body.Close() }()

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

func TestHandlerMatchesAPISIX317ETagRules(t *testing.T) {
	p := newTestPlugin(t, Config{Types: []string{"text/plain"}, MinLength: new(1)})
	for _, tt := range []struct {
		name string
		etag string
		want string
	}{
		{name: "invalid unquoted", etag: "123456789"},
		{name: "weak remains weak", etag: `W/"123456789"`, want: `W/"123456789"`},
		{name: "strong becomes weak", etag: `"123456789"`, want: `W/"123456789"`},
		{name: "embedded quote is accepted", etag: `"12"34"`, want: `W/"12"34"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/text", nil)
			req.Header.Set("Accept-Encoding", "br")
			res := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				w.Header().Set("Etag", tt.etag)
				_, _ = w.Write([]byte("hello world"))
			})).ServeHTTP(res, req)

			if got := res.Header().Get("Etag"); got != tt.want {
				t.Fatalf("Etag = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandlerDoesNotAddVaryByDefault(t *testing.T) {
	p := newTestPlugin(t, Config{Types: []string{"text/plain"}, MinLength: new(1)})
	req := httptest.NewRequest(http.MethodGet, "/text", nil)
	req.Header.Set("Accept-Encoding", "br")
	res := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello world"))
	})).ServeHTTP(res, req)

	if got := res.Header().Get("Vary"); got != "" {
		t.Fatalf("Vary = %q, want absent for APISIX 3.17 default", got)
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

func TestWriteCompressedResponseFlushesThroughWrapperChain(t *testing.T) {
	recorder := base.NewBufferedResponseWriter()
	recorder.Header().Set("Content-Type", "text/plain")
	recorder.Header().Set("Content-Encoding", "br")
	recorder.WriteHeader(http.StatusOK)
	recorder.SetBody([]byte("compressed"))

	fake := &fakeCapabilityWriter{}
	wrapper := &capabilityUnwrapper{ResponseWriter: fake}

	writeCompressedResponse(wrapper, recorder)

	if fake.flushes != 1 {
		t.Fatalf("flushes = %d, want 1 reached through the wrapper chain", fake.flushes)
	}
}

type hijackCaptureWriter struct {
	header    http.Header
	statuses  []int
	hijacks   int
	hijackErr error
	body      bytes.Buffer
}

func (w *hijackCaptureWriter) Header() http.Header { return w.header }
func (w *hijackCaptureWriter) WriteHeader(status int) {
	w.statuses = append(w.statuses, status)
}
func (w *hijackCaptureWriter) Write(body []byte) (int, error) { return w.body.Write(body) }
func (w *hijackCaptureWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacks++
	return nil, nil, w.hijackErr
}

func TestBrotliHijackDoesNotCommitDefaultResponse(t *testing.T) {
	for _, explicit101 := range []bool{false, true} {
		t.Run(strconv.FormatBool(explicit101), func(t *testing.T) {
			baseWriter := &hijackCaptureWriter{header: make(http.Header)}
			writer := newBoundedResponseWriter(baseWriter, 1024)
			if explicit101 {
				writer.WriteHeader(http.StatusSwitchingProtocols)
			}
			if _, _, err := writer.Hijack(); err != nil {
				t.Fatalf("Hijack() error = %v", err)
			}
			wantStatuses := []int(nil)
			if explicit101 {
				wantStatuses = []int{http.StatusSwitchingProtocols}
			}
			if !slices.Equal(baseWriter.statuses, wantStatuses) || baseWriter.hijacks != 1 {
				t.Fatalf("statuses/hijacks = %v/%d, want %v/1", baseWriter.statuses,
					baseWriter.hijacks, wantStatuses)
			}
		})
	}
}

func TestBrotliHandlerStopsAfterDirectHijack(t *testing.T) {
	p := newTestPlugin(t, Config{})
	baseWriter := &hijackCaptureWriter{header: make(http.Header)}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("brotli writer does not expose Hijacker")
		}
		_, _, _ = hijacker.Hijack()
	})).ServeHTTP(baseWriter, req)
	if len(baseWriter.statuses) != 0 || baseWriter.hijacks != 1 {
		t.Fatalf("statuses/hijacks = %v/%d, want no response and one hijack",
			baseWriter.statuses, baseWriter.hijacks)
	}
}

func TestBrotliHandlerCanRecoverAfterFailedHijack(t *testing.T) {
	t.Run("unsupported", func(t *testing.T) {
		p := newTestPlugin(t, Config{})
		res := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _, err := w.(http.Hijacker).Hijack()
			if err != http.ErrNotSupported {
				t.Fatalf("Hijack() error = %v, want ErrNotSupported", err)
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("fallback"))
		})).ServeHTTP(res, req)
		if res.Code != http.StatusServiceUnavailable || res.Body.String() != "fallback" {
			t.Fatalf("response = %d/%q, want 503/fallback", res.Code, res.Body.String())
		}
	})

	t.Run("delegated error", func(t *testing.T) {
		p := newTestPlugin(t, Config{})
		baseWriter := &hijackCaptureWriter{
			header:    make(http.Header),
			hijackErr: errors.New("hijack failed"),
		}
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				t.Fatal("Hijack() error = nil")
			}
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("fallback"))
		})).ServeHTTP(baseWriter, req)
		if !slices.Equal(baseWriter.statuses, []int{http.StatusBadGateway}) ||
			baseWriter.body.String() != "fallback" || baseWriter.hijacks != 1 {
			t.Fatalf("response = %v/%q hijacks=%d, want [502]/fallback/1",
				baseWriter.statuses, baseWriter.body.String(), baseWriter.hijacks)
		}
	})
}

type informationalCaptureWriter struct {
	header    http.Header
	statuses  []int
	snapshots []http.Header
	body      bytes.Buffer
}

func (w *informationalCaptureWriter) Header() http.Header { return w.header }
func (w *informationalCaptureWriter) WriteHeader(status int) {
	w.statuses = append(w.statuses, status)
	w.snapshots = append(w.snapshots, w.header.Clone())
}
func (w *informationalCaptureWriter) Write(body []byte) (int, error) { return w.body.Write(body) }

func TestBrotliInformationalHeadersDoNotLeakIntoFinal(t *testing.T) {
	p := newTestPlugin(t, Config{Types: []string{"text/plain"}, MinLength: new(1)})
	res := &informationalCaptureWriter{header: make(http.Header)}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "br")
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Link", "</early>")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Set("Link", "</final>")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("body"))
	})).ServeHTTP(res, req)
	if !slices.Equal(res.statuses, []int{http.StatusEarlyHints, http.StatusOK}) {
		t.Fatalf("statuses = %v, want [103 200]", res.statuses)
	}
	if got := res.snapshots[0].Values("Link"); !slices.Equal(got, []string{"</early>"}) {
		t.Fatalf("103 Link = %v, want early only", got)
	}
	if got := res.snapshots[1].Values("Link"); !slices.Equal(got, []string{"</final>"}) {
		t.Fatalf("final Link = %v, want final only", got)
	}
}

type fakeCapabilityWriter struct {
	flushes int
}

func (w *fakeCapabilityWriter) Header() http.Header { return make(http.Header) }
func (w *fakeCapabilityWriter) WriteHeader(int)     {}
func (w *fakeCapabilityWriter) Write(p []byte) (int, error) {
	return len(p), nil
}
func (w *fakeCapabilityWriter) Flush() { w.flushes++ }

type capabilityUnwrapper struct {
	http.ResponseWriter
}

func (w *capabilityUnwrapper) Unwrap() http.ResponseWriter { return w.ResponseWriter }

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

func TestBrotliResponseAboveLimitReturnsControlledError(t *testing.T) {
	limit := int64(1024)
	p := newTestPlugin(t, Config{maxResponseSize: limit})

	recorder := base.NewBufferedResponseWriter()
	recorder.Header().Set("Content-Type", "text/html")
	_, _ = recorder.Write(bytes.Repeat([]byte("a"), 2048))

	if err := p.compressResponse(recorder); err == nil {
		t.Fatal("compressResponse() error = nil for a response above the internal limit")
	}
	if recorder.Header().Get("Content-Encoding") == "br" {
		t.Fatal("oversized response was compressed")
	}
	if len(recorder.Body()) != 2048 {
		t.Fatalf("body length = %d, want the original 2048 bytes untouched", len(recorder.Body()))
	}
}

func TestBrotliHandlerPassesOversizedResponseThrough(t *testing.T) {
	limit := int64(1024)
	body := bytes.Repeat([]byte("payload"), 300)
	p := newTestPlugin(t, Config{maxResponseSize: limit})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))

	request := httptest.NewRequest(http.MethodGet, "http://example.test/download", nil)
	request.Header.Set("Accept-Encoding", "br")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if got := response.Header().Get("Content-Encoding"); got == "br" {
		t.Fatal("oversized response was compressed instead of passed through")
	}
	if got := response.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Fatalf("Content-Length = %q, want %d preserved on pass-through", got, len(body))
	}
	if !bytes.Equal(response.Body.Bytes(), body) {
		t.Fatal("oversized response body was modified")
	}
}

func TestBrotliHandlerPreservesContentLengthForUncompressedResponse(t *testing.T) {
	p := newTestPlugin(t, Config{Types: []string{"text/plain"}, MinLength: new(1)})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "4")
		_, _ = w.Write([]byte("body"))
	}))

	request := httptest.NewRequest(http.MethodGet, "http://example.test/plain", nil)
	request.Header.Set("Accept-Encoding", "br")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if got := response.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty", got)
	}
	if got := response.Header().Get("Content-Length"); got != "4" {
		t.Fatalf("Content-Length = %q, want preserved original length", got)
	}
	if got := response.Body.String(); got != "body" {
		t.Fatalf("body = %q, want body", got)
	}
}

func TestBrotliCompressesBelowLimit(t *testing.T) {
	limit := int64(1024)
	body := []byte("compressible payload")
	p := newTestPlugin(t, Config{maxResponseSize: limit})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(body)
	}))

	request := httptest.NewRequest(http.MethodGet, "http://example.test/page", nil)
	request.Header.Set("Accept-Encoding", "br")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if got := response.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("Content-Encoding = %q, want br below the cap", got)
	}
	if got := decodeBrotli(t, response.Body.Bytes()); got != string(body) {
		t.Fatalf("decoded body = %q, want %q", got, body)
	}
}

func TestBrotliPassThroughOversizedDeclaredRetainsContentLength(t *testing.T) {
	limit := int64(1024)
	body := bytes.Repeat([]byte("payload"), 512)
	p := newTestPlugin(t, Config{maxResponseSize: limit})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write(body)
	}))

	request := httptest.NewRequest(http.MethodGet, "http://example.test/download", nil)
	request.Header.Set("Accept-Encoding", "br")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if got := response.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty for pass-through", got)
	}
	if got := response.Header().Get("Content-Length"); got != "4096" {
		t.Fatalf("Content-Length = %q, want preserved 4096", got)
	}
	if !bytes.Equal(response.Body.Bytes(), body) {
		t.Fatal("pass-through body was modified")
	}
}

func TestBrotliPassThroughOversizedUnknownLength(t *testing.T) {
	limit := int64(1024)
	body := bytes.Repeat([]byte("payload"), 512)
	p := newTestPlugin(t, Config{maxResponseSize: limit})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(body)
	}))

	request := httptest.NewRequest(http.MethodGet, "http://example.test/download", nil)
	request.Header.Set("Accept-Encoding", "br")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if got := response.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty for pass-through", got)
	}
	if !bytes.Equal(response.Body.Bytes(), body) {
		t.Fatal("pass-through body was modified")
	}
}

func TestBrotliPassThroughRetainsSmallContentLength(t *testing.T) {
	p := newTestPlugin(t, Config{Types: []string{"text/html"}, MinLength: new(5)})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "4")
		_, _ = w.Write([]byte("tiny"))
	}))

	request := httptest.NewRequest(http.MethodGet, "http://example.test/data", nil)
	request.Header.Set("Accept-Encoding", "br")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if got := response.Header().Get("Content-Length"); got != "4" {
		t.Fatalf("Content-Length = %q, want preserved 4", got)
	}
	if got := response.Body.String(); got != "tiny" {
		t.Fatalf("body = %q, want tiny", got)
	}
}

func TestBoundedResponseWriterNeverBuffersBeyondCapPlusChunk(t *testing.T) {
	const cap = int64(1024)
	base := httptest.NewRecorder()
	writer := newBoundedResponseWriter(base, cap)

	chunk := bytes.Repeat([]byte("a"), 1024)
	for range 4 {
		_, _ = writer.Write(chunk)
	}

	if !writer.committed {
		t.Fatal("bounded writer did not switch to pass-through for an oversized body")
	}
	if writer.maxBuffered > cap+int64(len(chunk)) {
		t.Fatalf(
			"max buffered = %d, want at most cap+largestWrite %d",
			writer.maxBuffered,
			cap+int64(len(chunk)),
		)
	}
	if got := base.Body.Len(); got != 4096 {
		t.Fatalf("streamed body length = %d, want 4096", got)
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

func TestBrotliNegotiationVaryStatusAndHead(t *testing.T) {
	tests := []struct {
		name           string
		method         string
		acceptEncoding string
		status         int
		contentEnc     string
		wantEnc        string
		wantBody       string
		wantVary       bool
	}{
		{
			name:     "identity without accepted coding",
			method:   http.MethodGet,
			status:   http.StatusOK,
			wantBody: "body",
		},
		{
			name:           "not acceptable",
			method:         http.MethodGet,
			acceptEncoding: "*;q=0",
			status:         http.StatusNotAcceptable,
		},
		{
			name:           "head advertises",
			method:         http.MethodHead,
			acceptEncoding: "br",
			status:         http.StatusOK,
			wantEnc:        "br",
			wantVary:       true,
		},
		{
			name:           "no content",
			method:         http.MethodGet,
			acceptEncoding: "br",
			status:         http.StatusNoContent,
		},
		{
			name:           "not modified preserves encoding",
			method:         http.MethodGet,
			acceptEncoding: "br",
			status:         http.StatusNotModified,
			contentEnc:     "gzip",
			wantEnc:        "gzip",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(
				t,
				Config{Types: []string{"text/plain"}, MinLength: new(1), Vary: new(true)},
			)
			req := httptest.NewRequest(tt.method, "/", nil)
			if tt.acceptEncoding != "" {
				req.Header.Set("Accept-Encoding", tt.acceptEncoding)
			}
			res := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				if tt.contentEnc != "" {
					w.Header().Set("Content-Encoding", tt.contentEnc)
				}
				w.Header().Set("Content-Length", "4")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte("body"))
			})).ServeHTTP(res, req)
			if res.Code != tt.status {
				t.Fatalf("status = %d, want %d", res.Code, tt.status)
			}
			if got := res.Header().Get("Content-Encoding"); got != tt.wantEnc {
				t.Fatalf("Content-Encoding = %q, want %q", got, tt.wantEnc)
			}
			wantVary := ""
			if tt.wantVary {
				wantVary = "Accept-Encoding"
			}
			if got := res.Header().Get("Vary"); got != wantVary {
				t.Fatalf("Vary = %q, want %q", got, wantVary)
			}
			if tt.method == http.MethodHead && res.Header().Get("Content-Length") != "" {
				t.Fatalf(
					"HEAD Content-Length = %q, want removed",
					res.Header().Get("Content-Length"),
				)
			}
			if tt.status == http.StatusNotAcceptable && res.Body.Len() != 0 {
				t.Fatalf("406 body length = %d, want empty", res.Body.Len())
			}
			if tt.wantBody != "" && res.Body.String() != tt.wantBody &&
				tt.status == http.StatusOK && tt.method != http.MethodHead {
				if got := decodeBrotli(t, res.Body.Bytes()); got != tt.wantBody {
					t.Fatalf("decoded body = %q, want %q", got, tt.wantBody)
				}
			}
		})
	}
}

func TestBrotliNotAcceptableInvalidatesBodyDerivedHeaders(t *testing.T) {
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

func TestBrotliBodylessNegotiationPreservesStatusAndMetadata(t *testing.T) {
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

func TestBrotliNotModifiedWithoutContentTypePreservesEncodingWithoutVary(t *testing.T) {
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

func TestBrotliStructuralCompressionOfferOwnsStreamingFinish(t *testing.T) {
	p := newTestPlugin(t, Config{Types: []string{"text/plain"}})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "br")
	registered, state := compression.Register(req)
	offers := p.RegisterCompressionOffers(registered, state)
	offer := offers[0]
	if offer.Coding != compression.Brotli || offer.Eligible == nil {
		t.Fatalf("offer = %#v, want br with eligibility", offer)
	}
	_, state = compression.Register(registered, offer)
	decision := state.Decide(compression.ResponseMeta{
		Method: http.MethodGet,
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"text/plain"}},
	})
	underlying := httptest.NewRecorder()
	wrapped, err := p.WrapCompression(underlying, registered, state, decision)
	if err != nil {
		t.Fatalf("WrapCompression() error = %v", err)
	}
	wrapped.Header().Set("Content-Type", "text/plain")
	wrapped.WriteHeader(http.StatusOK)
	_, _ = wrapped.Write([]byte("streamed"))
	if finalizer, ok := wrapped.(base.StreamingResponseFinalizer); ok {
		if err := finalizer.FinishStreamingResponse(nil); err != nil {
			t.Fatalf("FinishStreamingResponse() error = %v", err)
		}
	} else {
		t.Fatal("compression wrapper does not own FinishStreamingResponse")
	}
	if got := underlying.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("Content-Encoding = %q, want br", got)
	}
}

func TestBrotliStreamingPassthroughWhenAlreadyEncoded(t *testing.T) {
	p := newTestPlugin(t, Config{Types: []string{"text/plain"}})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "br")
	registered, state := compression.Register(req)
	offers := p.RegisterCompressionOffers(registered, state)
	_, state = compression.Register(registered, offers[0])
	decision := state.Decide(compression.ResponseMeta{
		Method: http.MethodGet,
		Status: http.StatusOK,
		Header: http.Header{
			"Content-Type":     []string{"text/plain"},
			"Content-Encoding": []string{"br"},
		},
	})
	underlying := httptest.NewRecorder()
	wrapped, err := p.WrapCompression(underlying, registered, state, decision)
	if err != nil {
		t.Fatalf("WrapCompression() error = %v", err)
	}
	wrapped.Header().Set("Content-Type", "text/plain")
	wrapped.Header().Set("Content-Encoding", "br")
	wrapped.WriteHeader(http.StatusOK)
	if _, err := wrapped.Write([]byte("already-br")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if finalizer, ok := wrapped.(base.StreamingResponseFinalizer); ok {
		if err := finalizer.FinishStreamingResponse(nil); err != nil {
			t.Fatalf("FinishStreamingResponse() error = %v", err)
		}
	}
	if got := underlying.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("Content-Encoding = %q, want br", got)
	}
	if got := underlying.Body.String(); got != "already-br" {
		t.Fatalf("body = %q, want passthrough already-br", got)
	}
}

func TestBrotliStructuralOfferHonorsHTTPVersionGate(t *testing.T) {
	p := newTestPlugin(t, Config{Types: []string{"text/plain"}})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.ProtoMajor, req.ProtoMinor = 1, 0
	req.Header.Set("Accept-Encoding", "br")
	registered, state := compression.Register(req)
	offers := p.RegisterCompressionOffers(registered, state)
	offer := offers[0]
	if offer.Eligible == nil {
		t.Fatal("brotli offer has no eligibility callback")
	}
	_, state = compression.Register(registered, offer)
	decision := state.Decide(compression.ResponseMeta{
		Method: http.MethodGet,
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"text/plain"}},
	})
	if decision.Coding != compression.Identity {
		t.Fatalf("HTTP/1.0 decision = %q, want identity", decision.Coding)
	}
}
