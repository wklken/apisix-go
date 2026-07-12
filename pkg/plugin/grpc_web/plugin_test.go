package grpc_web

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
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

func TestHandlerRespondsToCORSPreflight(t *testing.T) {
	p := newTestPlugin(t, Config{CorsAllowHeaders: "content-type,x-grpc-web,authorization"})

	req := httptest.NewRequest(http.MethodOptions, "/hello.Service/Method", nil)
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", res.Code)
	}
	if got := res.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want *", got)
	}
	if got := res.Header().Get("Access-Control-Allow-Methods"); got != http.MethodPost {
		t.Fatalf("Access-Control-Allow-Methods = %q, want POST", got)
	}
	if got := res.Header().Get("Access-Control-Allow-Headers"); got != "content-type,x-grpc-web,authorization" {
		t.Fatalf("Access-Control-Allow-Headers = %q, want configured headers", got)
	}
	if got := res.Header().Get("Access-Control-Expose-Headers"); got != "grpc-message,grpc-status" {
		t.Fatalf("Access-Control-Expose-Headers = %q, want grpc-message,grpc-status", got)
	}
}

func TestHandlerRejectsInvalidRequest(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		contentType string
		body        string
		wantStatus  int
	}{
		{
			name:        "non-post",
			method:      http.MethodGet,
			contentType: "application/grpc-web",
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "unsupported content type",
			method:      http.MethodPost,
			contentType: "application/json",
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "invalid base64 text body",
			method:      http.MethodPost,
			contentType: "application/grpc-web-text",
			body:        "not-base64!",
			wantStatus:  http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{})
			req := httptest.NewRequest(tt.method, "/hello.Service/Method", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", tt.contentType)
			res := httptest.NewRecorder()

			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatal("next handler should not be called")
			})).ServeHTTP(res, req)

			if res.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", res.Code, tt.wantStatus)
			}
		})
	}
}

func TestHandlerTransformsTextRequestAndResponse(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(
		http.MethodPost,
		"/hello.Service/Method",
		strings.NewReader(base64.StdEncoding.EncodeToString([]byte("hello"))),
	)
	req.Header.Set("Content-Type", "application/grpc-web-text")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if string(body) != "hello" {
			t.Fatalf("upstream body = %q, want decoded hello", body)
		}
		if got := r.Header.Get("Content-Type"); got != "application/grpc" {
			t.Fatalf("upstream Content-Type = %q, want application/grpc", got)
		}
		if got := r.Header.Get("TE"); got != "trailers" {
			t.Fatalf("upstream TE = %q, want trailers", got)
		}

		w.Header().Set("Content-Type", "application/grpc")
		_, _ = w.Write([]byte("reply"))
		w.Header().Set("Grpc-Status", "0")
		w.Header().Set("Grpc-Message", "ok")
	})).ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if got := res.Header().Get("Content-Type"); got != "application/grpc-web-text" {
		t.Fatalf("response Content-Type = %q, want application/grpc-web-text", got)
	}
	wantBody := base64.StdEncoding.EncodeToString([]byte("reply")) +
		base64.StdEncoding.EncodeToString(buildTrailerForTest("0", "ok"))
	if got := res.Body.String(); got != wantBody {
		t.Fatalf("response body = %q, want %q", got, wantBody)
	}
	if got := res.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want removed", got)
	}
}

func TestHandlerTransformsBinaryRequestAndResponse(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodPost, "/hello.Service/Method", strings.NewReader("hello"))
	req.Header.Set("Content-Type", "application/grpc-web+proto")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if string(body) != "hello" {
			t.Fatalf("upstream body = %q, want unchanged hello", body)
		}
		if got := r.Header.Get("Content-Type"); got != "application/grpc" {
			t.Fatalf("upstream Content-Type = %q, want application/grpc", got)
		}

		w.Header().Set("Content-Type", "application/grpc")
		_, _ = w.Write([]byte("reply"))
		w.Header().Set("Grpc-Status", "7")
		w.Header().Set("Grpc-Message", "denied")
	})).ServeHTTP(res, req)

	if got := res.Header().Get("Content-Type"); got != "application/grpc-web+proto" {
		t.Fatalf("response Content-Type = %q, want application/grpc-web+proto", got)
	}
	wantBody := append([]byte("reply"), buildTrailerForTest("7", "denied")...)
	if got := res.Body.Bytes(); string(got) != string(wantBody) {
		t.Fatalf("response body = %q, want %q", got, wantBody)
	}
}

func TestHandlerStreamsBinaryResponseBeforeUpstreamReturns(t *testing.T) {
	p := newTestPlugin(t, Config{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseUpstream := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseUpstream)

	server := httptest.NewServer(p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Grpc-Status", "0")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("grpc-web response writer does not expose http.Flusher")
			return
		}
		_, _ = w.Write([]byte("first"))
		flusher.Flush()
		<-release
		_, _ = w.Write([]byte("second"))
	})))
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/hello.Service/Method", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/grpc-web")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	defer resp.Body.Close()

	first := make([]byte, len("first"))
	readFirst := make(chan error, 1)
	go func() {
		_, readErr := io.ReadFull(resp.Body, first)
		readFirst <- readErr
	}()
	select {
	case readErr := <-readFirst:
		if readErr != nil {
			t.Fatalf("read first streamed chunk: %v", readErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first streamed chunk")
	}
	if string(first) != "first" {
		t.Fatalf("first streamed chunk = %q, want first", first)
	}

	releaseUpstream()
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read remaining streamed response: %v", err)
	}
	wantRest := append([]byte("second"), buildTrailerForTest("0", "")...)
	if string(rest) != string(wantRest) {
		t.Fatalf("remaining response = %q, want %q", rest, wantRest)
	}
}

func TestHandlerStreamingResponseObservesClientCancellation(t *testing.T) {
	p := newTestPlugin(t, Config{})
	started := make(chan struct{})
	canceled := make(chan struct{})
	server := httptest.NewServer(p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("grpc-web response writer does not expose http.Flusher")
			return
		}
		_, _ = w.Write([]byte("first"))
		flusher.Flush()
		close(started)
		<-r.Context().Done()
		close(canceled)
	})))
	defer server.Close()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/hello.Service/Method", strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/grpc-web")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("client.Do() error = %v", err)
	}
	first := make([]byte, len("first"))
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		_ = resp.Body.Close()
		t.Fatalf("read first streamed chunk: %v", err)
	}
	if string(first) != "first" {
		_ = resp.Body.Close()
		t.Fatalf("first streamed chunk = %q, want first", first)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		_ = resp.Body.Close()
		t.Fatal("timed out waiting for upstream stream")
	}
	_ = resp.Body.Close()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request context was not canceled after client disconnect")
	}
}

func TestHandlerPreservesTrailersOnlyResponseMetadata(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodPost, "/hello.Service/Method", strings.NewReader("hello"))
	req.Header.Set("Content-Type", "application/grpc-web")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Grpc-Status", "7")
		w.Header().Set("Grpc-Message", "denied")
	})).ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if res.Body.Len() != 0 {
		t.Fatalf("trailers-only response body = %q, want empty", res.Body.String())
	}
	if got := res.Header().Get("Grpc-Status"); got != "7" {
		t.Fatalf("Grpc-Status header = %q, want 7", got)
	}
	if got := res.Header().Get("Grpc-Message"); got != "denied" {
		t.Fatalf("Grpc-Message header = %q, want denied", got)
	}
}

func TestHandlerConvertsUpstreamTrailerPrefixMetadata(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodPost, "/hello.Service/Method", strings.NewReader("hello"))
	req.Header.Set("Content-Type", "application/grpc-web")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("reply"))
		w.Header().Set(http.TrailerPrefix+"Grpc-Status", "7")
		w.Header().Set(http.TrailerPrefix+"Grpc-Message", "denied")
	})).ServeHTTP(res, req)

	wantBody := append([]byte("reply"), buildTrailerForTest("7", "denied")...)
	if got := res.Body.Bytes(); string(got) != string(wantBody) {
		t.Fatalf("response body = %q, want trailer metadata from upstream trailers: %q", got, wantBody)
	}
	if got := res.Header().Get("Grpc-Status"); got != "7" {
		t.Fatalf("Grpc-Status = %q, want 7", got)
	}
	if got := res.Header().Get("Grpc-Message"); got != "denied" {
		t.Fatalf("Grpc-Message = %q, want denied", got)
	}
	if got := res.Header().Get("Trailer"); got != "" {
		t.Fatalf("Trailer header = %q, want grpc trailer declaration consumed", got)
	}
}

func TestHandlerPreservesExistingCorsOrigin(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodPost, "/hello.Service/Method", strings.NewReader("hello"))
	req.Header.Set("Content-Type", "application/grpc-web")
	res := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "https://client.example")
		w.Header().Set("Grpc-Status", "0")
	})).ServeHTTP(res, req)

	if got := res.Header().Get("Access-Control-Allow-Origin"); got != "https://client.example" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want existing origin", got)
	}
}

func TestHandlerRewritesWildcardRouteToGRPCPath(t *testing.T) {
	p := newTestPlugin(t, Config{})
	router := chi.NewRouter()
	router.Method(http.MethodPost, "/grpc/*", p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/helloworld.Greeter/SayHello" {
			t.Fatalf("upstream path = %q, want gRPC wildcard path", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/grpc" {
			t.Fatalf("upstream Content-Type = %q, want application/grpc", got)
		}
		w.Header().Set("Grpc-Status", "0")
	})))

	req := httptest.NewRequest(http.MethodPost, "/grpc/helloworld.Greeter/SayHello", strings.NewReader("hello"))
	req.Header.Set("Content-Type", "application/grpc-web")
	res := httptest.NewRecorder()

	router.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", res.Code, res.Body.String())
	}
}

func TestHandlerRejectsRoutedRequestWithoutWildcard(t *testing.T) {
	p := newTestPlugin(t, Config{})
	router := chi.NewRouter()
	router.Method(http.MethodPost, "/grpc", p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})))

	req := httptest.NewRequest(http.MethodPost, "/grpc", strings.NewReader("hello"))
	req.Header.Set("Content-Type", "application/grpc-web")
	res := httptest.NewRecorder()

	router.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.Code)
	}
}

func buildTrailerForTest(status string, message string) []byte {
	trailer := []byte("grpc-status:" + status + "\r\n" + "grpc-message:" + message + "\r\n")
	return append([]byte{0x80, 0x00, 0x00, 0x00, byte(len(trailer))}, trailer...)
}
