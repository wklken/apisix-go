package gzip_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/gzip"
)

func TestGzipStreamingPreservesUpstreamWithoutAcceptedCoding(t *testing.T) {
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
	binding := bindPluginForTest("gzip", p, plugin.ScopeRoute, plugin.ResourceProvenance{
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
		_, _ = w.Write([]byte("upstream response"))
	})).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if response.Body.String() != "upstream response" {
		t.Fatalf("body = %q, want upstream response", response.Body.String())
	}
}

func TestGzipStreamingExecutorIgnoresDeflateOffer(t *testing.T) {
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
	binding := bindPluginForTest("gzip", p, plugin.ScopeRoute, plugin.ResourceProvenance{
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
	if got := response.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want no deflate offer", got)
	}
	if got := response.Body.String(); got != "deflated response" {
		t.Fatalf("response body = %q, want identity body", got)
	}
}
