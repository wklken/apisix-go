package compression_test

import (
	"bytes"
	cgzip "compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	brotlidec "github.com/andybalholm/brotli"
	"github.com/wklken/apisix-go/pkg/plugin/brotli"
	"github.com/wklken/apisix-go/pkg/plugin/gzip"
)

func TestCombinedCompressionNegotiation(t *testing.T) {
	tests := []struct {
		name           string
		acceptEncoding string
		wantEncoding   string
		wrap           func(http.Handler) http.Handler
	}{
		{
			name:           "brotli outer",
			acceptEncoding: "br;q=0.8, gzip;q=0.5, deflate;q=0.2",
			wantEncoding:   "br",
			wrap: func(next http.Handler) http.Handler {
				return newBrotli(t).Handler(newGzip(t).Handler(next))
			},
		},
		{
			name:           "gzip outer",
			acceptEncoding: "br;q=0.2, gzip;q=0.8, deflate;q=0.5",
			wantEncoding:   "gzip",
			wrap: func(next http.Handler) http.Handler {
				return newGzip(t).Handler(newBrotli(t).Handler(next))
			},
		},
		{
			name:           "deflate preferred",
			acceptEncoding: "br;q=0.2, gzip;q=0.3, deflate;q=0.9",
			wantEncoding:   "deflate",
			wrap: func(next http.Handler) http.Handler {
				return newBrotli(t).Handler(newGzip(t).Handler(next))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Accept-Encoding", tt.acceptEncoding)
			res := httptest.NewRecorder()
			body := bytes.Repeat([]byte("representation "), 8)
			upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/plain")
				w.Header().Set("Content-Length", "128")
				_, _ = w.Write(body)
			})

			tt.wrap(upstream).ServeHTTP(res, req)
			if got := res.Header().Get("Content-Encoding"); got != tt.wantEncoding {
				t.Fatalf("Content-Encoding = %q, want %q", got, tt.wantEncoding)
			}
			if got := res.Header().Values("Content-Encoding"); len(got) != 1 {
				t.Fatalf("Content-Encoding values = %#v, want exactly one layer", got)
			}
			var decoded []byte
			switch tt.wantEncoding {
			case "br":
				reader := brotlidec.NewReader(bytes.NewReader(res.Body.Bytes()))
				decoded, _ = io.ReadAll(reader)
			case "gzip":
				decoded = decodeGzip(t, res.Body.Bytes())
			case "deflate":
				reader, err := zlib.NewReader(bytes.NewReader(res.Body.Bytes()))
				if err != nil {
					t.Fatalf("create zlib reader: %v", err)
				}
				decoded, _ = io.ReadAll(reader)
				_ = reader.Close()
			}
			if !bytes.Equal(decoded, body) {
				t.Fatalf("decoded body = %q, want %q", decoded, body)
			}
		})
	}
}

func TestCombinedCompressionPreservesAcceptedExistingCoding(t *testing.T) {
	wrappers := map[string]func(*testing.T, http.Handler) http.Handler{
		"brotli outer": func(t *testing.T, next http.Handler) http.Handler {
			return newBrotli(t).Handler(newGzip(t).Handler(next))
		},
		"gzip outer": func(t *testing.T, next http.Handler) http.Handler {
			return newGzip(t).Handler(newBrotli(t).Handler(next))
		},
	}
	for name, wrap := range wrappers {
		for _, coding := range []string{"gzip", "br"} {
			t.Run(name+"/"+coding, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				req.Header.Set("Accept-Encoding", coding+", identity;q=0")
				res := httptest.NewRecorder()
				upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/plain")
					w.Header().Set("Content-Encoding", coding)
					_, _ = w.Write([]byte("already encoded"))
				})
				wrap(t, upstream).ServeHTTP(res, req)
				if res.Code != http.StatusOK || res.Header().Get("Content-Encoding") != coding ||
					res.Body.String() != "already encoded" {
					t.Fatalf("response = %d/%q/%q, want 200/%s/already encoded", res.Code,
						res.Header().Get("Content-Encoding"), res.Body.String(), coding)
				}
			})
		}
	}
}

func TestCombinedCompressionRejectsBrotliCapFallbackWhenIdentityForbidden(t *testing.T) {
	for _, declared := range []bool{false, true} {
		for _, outer := range []string{"brotli", "gzip"} {
			t.Run(outer+"/declared="+strconv.FormatBool(declared), func(t *testing.T) {
				limit := int64(8)
				br := newBrotli(t)
				br.Config().(*brotli.Config).MaxResponseSize = &limit
				gz := newGzip(t)
				upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/plain")
					if declared {
						w.Header().Set("Content-Length", "32")
					}
					_, _ = w.Write(bytes.Repeat([]byte("x"), 32))
				})
				var handler http.Handler
				if outer == "brotli" {
					handler = br.Handler(gz.Handler(upstream))
				} else {
					handler = gz.Handler(br.Handler(upstream))
				}
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				req.Header.Set("Accept-Encoding", "br, identity;q=0")
				res := httptest.NewRecorder()
				handler.ServeHTTP(res, req)
				if res.Code != http.StatusNotAcceptable || res.Body.Len() != 0 {
					t.Fatalf("response = %d/%d bytes, want empty 406", res.Code, res.Body.Len())
				}
			})
		}
	}
}

func TestCombinedCompressionPreservesAcceptedPreEncodedBrotliAboveCap(t *testing.T) {
	for _, declared := range []bool{false, true} {
		for _, outer := range []string{"brotli", "gzip"} {
			t.Run(outer+"/declared="+strconv.FormatBool(declared), func(t *testing.T) {
				limit := int64(8)
				body := bytes.Repeat([]byte("encoded"), 8)
				br := newBrotli(t)
				br.Config().(*brotli.Config).MaxResponseSize = &limit
				gz := newGzip(t)
				upstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/plain")
					w.Header().Set("Content-Encoding", "br")
					if declared {
						w.Header().Set("Content-Length", strconv.Itoa(len(body)))
					}
					_, _ = w.Write(body)
				})
				var handler http.Handler
				if outer == "brotli" {
					handler = br.Handler(gz.Handler(upstream))
				} else {
					handler = gz.Handler(br.Handler(upstream))
				}
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				req.Header.Set("Accept-Encoding", "br, identity;q=0")
				res := httptest.NewRecorder()
				handler.ServeHTTP(res, req)
				if res.Code != http.StatusOK || res.Header().Get("Content-Encoding") != "br" ||
					!bytes.Equal(res.Body.Bytes(), body) {
					t.Fatalf("response = %d/%q/%d bytes, want preserved 200/br/%d bytes",
						res.Code, res.Header().Get("Content-Encoding"), res.Body.Len(), len(body))
				}
			})
		}
	}
}

func newGzip(t *testing.T) *gzip.Plugin {
	t.Helper()
	minLength := 1
	p := &gzip.Plugin{}
	p.Config().(*gzip.Config).Types = []string{"text/plain"}
	p.Config().(*gzip.Config).MinLength = &minLength
	if err := p.Init(); err != nil {
		t.Fatalf("gzip Init: %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("gzip PostInit: %v", err)
	}
	return p
}

func newBrotli(t *testing.T) *brotli.Plugin {
	t.Helper()
	minLength := 1
	p := &brotli.Plugin{}
	p.Config().(*brotli.Config).Types = []string{"text/plain"}
	p.Config().(*brotli.Config).MinLength = &minLength
	if err := p.Init(); err != nil {
		t.Fatalf("brotli Init: %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("brotli PostInit: %v", err)
	}
	return p
}

func decodeGzip(t *testing.T, body []byte) []byte {
	t.Helper()
	reader, err := cgzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create gzip reader: %v", err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("decode gzip body: %v", err)
	}
	_ = reader.Close()
	return decoded
}
