package error_page

import (
	"net/http"
	"net/http/httptest"
	"testing"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestErrorPageRunsOneAtomicBufferedBodyCallback(t *testing.T) {
	plugin := newTestPlugin(t, Metadata{
		Enable: true,
		Error404: ErrorPage{
			Body:        "custom error",
			ContentType: "text/plain",
		},
	})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/missing", nil)
	req = apisixctx.WithRequestVars(req)
	state := base.ResponseState{
		Status: http.StatusNotFound,
		Header: http.Header{"Content-Length": {"8"}, "X-Unchanged": {"yes"}},
		Body:   []byte("original"),
	}
	apisixctx.SetRequestResponseSource(req, apisixctx.ResponseSourceAPISIX)
	if err := plugin.RunBufferedBodyFilter(req, &state); err != nil {
		t.Fatalf("RunBufferedBodyFilter() error = %v", err)
	}
	if got := string(state.Body); got != "custom error" {
		t.Fatalf("body = %q, want custom error", got)
	}
	if got := state.Header.Get("Content-Type"); got != "text/plain" {
		t.Fatalf("Content-Type = %q, want text/plain", got)
	}
	if got := state.Header.Get("Content-Length"); got != "12" {
		t.Fatalf("Content-Length = %q, want 12", got)
	}
	if got := state.Header.Get("X-Unchanged"); got != "yes" {
		t.Fatalf("X-Unchanged = %q, want preserved", got)
	}
}

func newTestPlugin(t *testing.T, metadata Metadata) *Plugin {
	t.Helper()

	p := &Plugin{metadata: metadata}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerRewritesConfiguredErrorPage(t *testing.T) {
	p := newTestPlugin(t, Metadata{
		Enable: true,
		Error404: ErrorPage{
			Body:        `{"code":404,"message":"missing"}`,
			ContentType: "application/json",
		},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("original"))
	})

	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.Code)
	}
	if got := res.Body.String(); got != `{"code":404,"message":"missing"}` {
		t.Fatalf("body = %q, want custom error page", got)
	}
	if got := res.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	if got := res.Header().Get("Content-Length"); got != "32" {
		t.Fatalf("content-length = %q, want custom body length", got)
	}
}

func TestHandlerKeepsUnconfiguredOrDisabledResponses(t *testing.T) {
	disabled := newTestPlugin(t, Metadata{Enable: false, Error404: ErrorPage{Body: "custom"}})
	res := performRequest(disabled, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("original"))
	})
	if got := res.Body.String(); got != "original" {
		t.Fatalf("disabled body = %q, want original", got)
	}

	enabled := newTestPlugin(t, Metadata{Enable: true})
	res = performRequest(enabled, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad request"))
	})
	if got := res.Body.String(); got != "bad request" {
		t.Fatalf("unconfigured status body = %q, want original", got)
	}
}

func TestHandlerDoesNotRewriteUpstreamErrorWhenSourceIsKnown(t *testing.T) {
	p := newTestPlugin(t, Metadata{
		Enable: true,
		Error404: ErrorPage{
			Body:        "custom",
			ContentType: "text/plain",
		},
	})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/missing", nil)
	req = apisixctx.WithRequestVars(req)
	apisixctx.RegisterRequestVar(req, "$response_source", "upstream")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("upstream error"))
	})).ServeHTTP(rr, req)

	if got := rr.Body.String(); got != "upstream error" {
		t.Fatalf("body = %q, want upstream error unchanged", got)
	}
}

func TestHandlerKeepsSuccessfulResponses(t *testing.T) {
	p := newTestPlugin(t, Metadata{Enable: true, Error404: ErrorPage{Body: "custom"}})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if got := res.Body.String(); got != "ok" {
		t.Fatalf("body = %q, want ok", got)
	}
}

func TestDefaultErrorPageBody(t *testing.T) {
	p := newTestPlugin(t, Metadata{Enable: true, Error500: ErrorPage{}})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("original"))
	})

	if res.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.Code)
	}
	if got := res.Header().Get("Content-Type"); got != "text/html" {
		t.Fatalf("content-type = %q, want text/html", got)
	}
	if got := res.Body.String(); got == "" || got == "original" {
		t.Fatalf("body = %q, want default error page", got)
	}
}

func performRequest(p *Plugin, handler func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/missing", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(handler)).ServeHTTP(rr, req)
	return rr
}
