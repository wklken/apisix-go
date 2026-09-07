package serverless

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/base"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
)

func TestOfficialHTTPSRedirectUsesRequestVariables(t *testing.T) {
	p := newTestPlugin(t, NewPreFunction(), Config{Functions: []string{`return function()
 if ngx.var.scheme == "http" and ngx.var.host == "foo.com" then
 ngx.header["Location"] = "https://foo.com" .. ngx.var.request_uri
 ngx.exit(ngx.HTTP_MOVED_PERMANENTLY)
 end end`}})
	req := httptest.NewRequest(http.MethodGet, "http://foo.com/hello?q=1", nil)
	response := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(response, req)
	if response.Code != 301 || response.Header().Get("Location") != "https://foo.com/hello?q=1" {
		t.Fatalf(
			"status=%d location=%q body=%q",
			response.Code,
			response.Header().Get("Location"),
			response.Body.String(),
		)
	}
}

func TestContextUpstreamURIReachesProxyWithoutLosingEscapes(t *testing.T) {
	p := newTestPlugin(
		t,
		NewPostFunction(),
		Config{Functions: []string{`return function(conf,ctx) ctx.var.upstream_uri = "/server%2Fport?new=1" end`}},
	)
	for _, prior := range []string{"", "/prior?old=1"} {
		req := httptest.NewRequest(http.MethodGet, "http://foo.com/hello?client=1", nil)
		if prior != "" {
			req = req.WithContext(
				context.WithValue(
					req.Context(),
					apisixctx.ProxyRewriteKey,
					map[string]any{"uri": prior, "host": "preserved"},
				),
			)
		}
		response := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rewrite := apisixctx.FinalizeProxyRewrite(r)
			if got := r.URL.RequestURI(); got != "/server%2Fport?new=1" {
				t.Errorf("prior=%q target=%q", prior, got)
			}
			if prior != "" && rewrite.Host != "preserved" {
				t.Errorf("lost existing host override: %+v", rewrite)
			}
			w.WriteHeader(204)
		})).ServeHTTP(response, req)
		if response.Code != 204 {
			t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
		}
	}
}

func TestPreAndPostKeepClientRequestURIAfterUpstreamRewrite(t *testing.T) {
	pre := newTestPlugin(
		t,
		NewPreFunction(),
		Config{Functions: []string{`return function(conf,ctx) ctx.var.upstream_uri="/server?new=1" end`}},
	)
	post := newTestPlugin(
		t,
		NewPostFunction(),
		Config{
			Functions: []string{
				`return function(conf,ctx) ngx.header["X-Client-URI"]=ctx.var.request_uri; ngx.header["X-Upstream-URI"]=ctx.var.upstream_uri; ngx.exit(204) end`,
			},
		},
	)
	request := httptest.NewRequest(http.MethodGet, "http://foo.com/./hello?client=1", nil)
	request.URL.Path = "/hello" // Production normalization precedes plugin execution.
	response := httptest.NewRecorder()
	pre.Handler(post.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))).
		ServeHTTP(response, request)
	if response.Code != 204 || response.Header().Get("X-Client-URI") != "/hello?client=1" ||
		response.Header().Get("X-Upstream-URI") != "/server?new=1" {
		t.Fatalf("status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
	}
}

func TestPreFunctionClientURISurvivesDetachedLogPhase(t *testing.T) {
	for _, sensitive := range []bool{false, true} {
		pre := newTestPlugin(
			t,
			NewPreFunction(),
			Config{Functions: []string{`return function(conf,ctx) ctx.var.upstream_uri="/server?new=1" end`}},
		)
		request := httptest.NewRequest(http.MethodGet, "http://foo.com/hello?client=1&token=secret", nil)
		expected := "/hello?client=1&token=secret"
		if sensitive {
			apisixctx.RegisterSensitiveQueryName(request, "token")
			expected = "/hello?client=1&token=***"
		}
		result := pre.RunRequestPhase(httptest.NewRecorder(), request)
		snapshot := base.BuildLogSnapshotFromOwnedInputs(
			result.Request,
			base.ResponseCaptureSnapshot{},
			nil,
			false,
			apisixctx.ResponseOutcome{Status: 204},
			apisixctx.ResponseSourceUpstream,
			time.Time{},
			time.Time{},
		)
		post := newTestPlugin(
			t,
			NewPostFunction(),
			Config{
				Phase: "log",
				Functions: []string{
					`return function(conf,ctx) if ctx.var.request_uri ~= "` + expected + `" then error("incorrect client request URI") end end`,
				},
			},
		)
		if err := post.RunLogPhase(snapshot); err != nil {
			t.Fatalf("sensitive=%t log callback: %v", sensitive, err)
		}
	}
}
