package gzip_test

import (
	"compress/zlib"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/gzip"
)

func TestGzipStreamingNotAcceptableIsBodyless406(t *testing.T) {
	p := &gzip.Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	minLength := 1
	p.Config().(*gzip.Config).Types = []string{"text/plain"}
	p.Config().(*gzip.Config).MinLength = &minLength
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	binding := plugin.BindPlugin("gzip", p, plugin.ScopeRoute, plugin.ResourceProvenance{
		Kind: plugin.ResourceRoute,
		ID:   "gzip",
	})
	executor, err := plugin.NewStreamingResponseExecutor([]plugin.Binding{binding})
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept-Encoding", "*;q=0")
	request, _ = apisixctx.EnsureRequestLifecycle(request, time.Now())
	response := httptest.NewRecorder()
	executor.Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("must be suppressed"))
	})).ServeHTTP(response, request)
	if response.Code != http.StatusNotAcceptable {
		t.Fatalf("status = %d, want 406", response.Code)
	}
	if response.Body.Len() != 0 {
		t.Fatalf("body length = %d, want 0", response.Body.Len())
	}
}

func TestGzipStreamingExecutorSelectsDeflateOffer(t *testing.T) {
	p := &gzip.Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	minLength := 1
	p.Config().(*gzip.Config).Types = []string{"text/plain"}
	p.Config().(*gzip.Config).MinLength = &minLength
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	binding := plugin.BindPlugin("gzip", p, plugin.ScopeRoute, plugin.ResourceProvenance{
		Kind: plugin.ResourceRoute, ID: "deflate",
	})
	executor, err := plugin.NewStreamingResponseExecutor([]plugin.Binding{binding})
	if err != nil {
		t.Fatalf("NewStreamingResponseExecutor() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Accept-Encoding", "gzip;q=0, deflate;q=1")
	response := httptest.NewRecorder()
	executor.Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("deflated response"))
	})).ServeHTTP(response, request)
	if got := response.Header().Get("Content-Encoding"); got != "deflate" {
		t.Fatalf("Content-Encoding = %q, want deflate", got)
	}
	reader, err := zlib.NewReader(response.Body)
	if err != nil {
		t.Fatalf("zlib.NewReader() error = %v", err)
	}
	body, err := io.ReadAll(reader)
	if closeErr := reader.Close(); err == nil {
		err = closeErr
	}
	if err != nil || string(body) != "deflated response" {
		t.Fatalf("decoded response = %q/%v", body, err)
	}
}
