package route

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
)

type matchedRouteLogCapture struct {
	snapshots chan base.LogSnapshot
}

func newMatchedRouteLogCapture() *matchedRouteLogCapture {
	return &matchedRouteLogCapture{
		snapshots: make(chan base.LogSnapshot, 8),
	}
}

func (*matchedRouteLogCapture) Init() error                            { return nil }
func (*matchedRouteLogCapture) PostInit() error                        { return nil }
func (*matchedRouteLogCapture) Config() any                            { return &struct{}{} }
func (*matchedRouteLogCapture) GetSchema() string                      { return "" }
func (*matchedRouteLogCapture) GetMetadataSchema() string              { return "" }
func (*matchedRouteLogCapture) GetPriority() int                       { return 0 }
func (*matchedRouteLogCapture) GetName() string                        { return "matched-route-log-capture" }
func (*matchedRouteLogCapture) Handler(next http.Handler) http.Handler { return next }

func (capture *matchedRouteLogCapture) RunLogPhase(snapshot base.LogSnapshot) error {
	capture.snapshots <- snapshot
	return nil
}

func (*matchedRouteLogCapture) LogCapturePolicy() base.LogCapturePolicy {
	return base.LogCapturePolicy{}
}

func TestCompileHTTPPublishesActualMatchedHostAndURIForEachRequest(t *testing.T) {
	t.Parallel()

	routeResource := resource.Route{
		ID:    "multi-match",
		Hosts: []string{"foo.com", "bar.com"},
		Uris:  []string{"/foo*", "/bar*"},
	}
	handler := ensureRouteLifecycle(initializeAPISIXVars(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(
				w,
				"%s|%s",
				apisixctx.GetApisixVar(r, "$matched_host"),
				apisixctx.GetApisixVar(r, "$matched_uri"),
			)
		}),
		"",
		routeResource,
		resource.Service{},
	))
	snapshot, err := CompileHTTP(context.Background(), CompileInput{
		Revision: 1,
		Routes: []PreparedRoute{{
			Route:   routeResource,
			Hosts:   routeResource.Hosts,
			Handler: handler,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	serve := func(host string, path string) string {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Host = host
		request, lifecycle := apisixctx.EnsureRequestLifecycle(request, time.Now())
		response := httptest.NewRecorder()
		snapshot.Handler().ServeHTTP(response, request)
		lifecycle.Complete(apisixctx.ResponseOutcome{
			Kind: apisixctx.RequestOutcomeCompleted, Status: response.Code, Committed: true,
		}, time.Now())
		if failures := lifecycle.Finalize(); len(failures) != 0 {
			return fmt.Sprintf("finalize failures=%v", failures)
		}
		apisixctx.RecycleVars(lifecycle.FinalRequest())
		if response.Code != http.StatusOK {
			return fmt.Sprintf("status=%d body=%q", response.Code, response.Body.String())
		}
		return response.Body.String()
	}
	for _, test := range []struct {
		host string
		path string
		want string
	}{
		{host: "foo.com", path: "/foo1", want: "foo.com|/foo*"},
		{host: "bar.com", path: "/bar1", want: "bar.com|/bar*"},
	} {
		if got := serve(test.host, test.path); got != test.want {
			t.Fatalf("request %s%s matched labels = %q, want %q", test.host, test.path, got, test.want)
		}
	}

	const requestCount = 64
	errors := make(chan error, requestCount)
	var requests sync.WaitGroup
	for index := range requestCount {
		requests.Go(func() {
			host, path, want := "foo.com", "/foo1", "foo.com|/foo*"
			if index%2 == 1 {
				host, path, want = "bar.com", "/bar1", "bar.com|/bar*"
			}
			if got := serve(host, path); got != want {
				errors <- fmt.Errorf("request %s%s matched labels = %q, want %q", host, path, got, want)
			}
		})
	}
	requests.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
}

func TestCompileHTTPPublishesActualMatchedRouteToDetachedLogSnapshot(t *testing.T) {
	t.Parallel()

	staticRoute := resource.Route{
		ID:    "multi-match",
		Hosts: []string{"foo.com", "bar.com"},
		Uris:  []string{"/foo*", "/bar*"},
	}
	capture := newMatchedRouteLogCapture()
	routeBindings := matchedRouteLogBindings(t, staticRoute, capture, plugin.ScopeRoute)
	routeHandler := matchedRouteLogHandler(t, routeBindings, staticRoute)
	notFoundBindings := matchedRouteLogBindings(t, resource.Route{}, capture, plugin.ScopeGlobal)
	notFoundHandler, err := BuildPreparedNotFoundHandler("", notFoundBindings)
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := CompileHTTP(context.Background(), CompileInput{
		Revision: 1,
		Routes: []PreparedRoute{
			{Route: staticRoute, Hosts: staticRoute.Hosts, Handler: routeHandler},
			{Route: resource.Route{ID: "parameter", Uri: "/users/:id"}, Handler: routeHandler},
			{
				Route: resource.Route{ID: "wildcard-host", Uri: "/wild*", Hosts: []string{"*.example.com"}},
				Hosts: []string{"*.example.com"}, Handler: routeHandler,
			},
			{
				Route: resource.Route{
					ID: "host-rank", Uri: "/rank*", Hosts: []string{"*.example.com", "api.example.com"},
				},
				Hosts: []string{"*.example.com", "api.example.com"}, Handler: routeHandler,
			},
		},
		NotFound: notFoundHandler,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name       string
		host       string
		path       string
		wantStatus int
		wantHost   string
		wantURI    string
	}{
		{
			name: "multi host and URI", host: "bar.com", path: "/bar1",
			wantStatus: http.StatusNoContent, wantHost: "bar.com", wantURI: "/bar*",
		},
		{
			name: "direct parameter route", host: "gateway.test", path: "/users/42",
			wantStatus: http.StatusNoContent, wantURI: "/users/:id",
		},
		{
			name: "wildcard host", host: "tenant.example.com", path: "/wild1",
			wantStatus: http.StatusNoContent, wantHost: "*.example.com", wantURI: "/wild*",
		},
		{
			name: "exact host wins over wildcard", host: "api.example.com", path: "/rank1",
			wantStatus: http.StatusNoContent, wantHost: "api.example.com", wantURI: "/rank*",
		},
		{
			name: "not found", host: "gateway.test", path: "/missing",
			wantStatus: http.StatusNotFound,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			request.Host = test.host
			request, lifecycle := apisixctx.EnsureRequestLifecycle(request, time.Now())
			response := httptest.NewRecorder()
			snapshot.Handler().ServeHTTP(response, request)
			lifecycle.Complete(apisixctx.ResponseOutcome{
				Kind: apisixctx.RequestOutcomeCompleted, Status: response.Code, Committed: true,
			}, time.Now())
			if failures := lifecycle.Finalize(); len(failures) != 0 {
				t.Fatalf("Finalize() failures = %v", failures)
			}
			finalRequest := lifecycle.FinalRequest()
			apisixctx.RecycleVars(finalRequest)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			select {
			case logged := <-capture.snapshots:
				if got := logged.Request.APISIXVars["$matched_host"]; got != test.wantHost {
					t.Fatalf("detached $matched_host = %#v, want %q", got, test.wantHost)
				}
				if got := logged.Request.APISIXVars["$matched_uri"]; got != test.wantURI {
					t.Fatalf("detached $matched_uri = %#v, want %q", got, test.wantURI)
				}
			case <-time.After(time.Second):
				t.Fatal("detached log snapshot was not captured")
			}
		})
	}
}

func matchedRouteLogBindings(
	t *testing.T,
	routeResource resource.Route,
	capture *matchedRouteLogCapture,
	logScope plugin.Scope,
) []plugin.Binding {
	t.Helper()
	bindings := []plugin.Binding{
		bindPluginForTest(
			"http-logger",
			capture,
			logScope,
			plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: routeResource.ID},
		),
	}
	originalPlugin := bindings[0].Plugin
	if _, err := newRequestPipelineWithLog(bindings, nil); err != nil {
		t.Fatal(err)
	}
	if bindings[0].Plugin != originalPlugin {
		t.Fatal("log binding was mutated while building the pipeline")
	}
	return bindings
}

func matchedRouteLogHandler(
	t *testing.T,
	bindings []plugin.Binding,
	routeResource resource.Route,
) http.Handler {
	t.Helper()
	pipeline, err := newRequestPipelineWithLog(bindings, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ensureRouteLifecycle(initializeAPISIXVars(
		pipeline.Then(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})),
		"",
		routeResource,
		resource.Service{},
	))
}
