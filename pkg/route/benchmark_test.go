package route

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/wklken/apisix-go/pkg/resource"
)

// Benchmark corpus for APISIX URI conversion, route registration, and
// dispatch. Static routes exercise the project dispatcher behind exact chi
// patterns; embedded-wildcard routes share a dispatcher behind a chi prefix.

// benchmarkRouteWriter records only status and byte count.
type benchmarkRouteWriter struct {
	status int
	bytes  int
}

var benchmarkRouteHeader = http.Header{}

func (w *benchmarkRouteWriter) WriteHeader(status int) {
	w.status = status
}

func (w *benchmarkRouteWriter) Write(p []byte) (int, error) {
	w.bytes += len(p)
	return len(p), nil
}

func (*benchmarkRouteWriter) Header() http.Header {
	return benchmarkRouteHeader
}

func BenchmarkConvertURI(b *testing.B) {
	uris := []struct {
		name string
		uri  string
	}{
		{name: "param", uri: "/benchmark/paths/:id/actions"},
		{name: "wildcard", uri: "/benchmark/articles/*/comments"},
	}
	for _, spec := range uris {
		b.Run("uri="+spec.name, func(b *testing.B) {
			b.ReportAllocs()
			var sink string
			for b.Loop() {
				converted, err := convertURI(spec.uri)
				if err != nil {
					b.Fatal(err)
				}
				sink = converted
			}
			runtime.KeepAlive(sink)
		})
	}
}

func BenchmarkRegisterRoutes(b *testing.B) {
	for _, kind := range []string{"static", "embedded-wildcard"} {
		for _, routeCount := range []int{10, 100, 1000} {
			b.Run(fmt.Sprintf("kind=%s/routes=%d", kind, routeCount), func(b *testing.B) {
				benchmarkRegisterRoutes(b, kind, routeCount)
			})
		}
	}
}

func benchmarkRegisterRoutes(b *testing.B, kind string, routeCount int) {
	b.ReportAllocs()
	uris := make([]string, routeCount)
	for i := range uris {
		switch kind {
		case "static":
			uris[i] = fmt.Sprintf("/benchmark/routes/%04d", i)
		case "embedded-wildcard":
			uris[i] = fmt.Sprintf("/benchmark/articles/*/suffix-%04d", i)
		}
	}
	handler := http.NotFoundHandler()
	for b.Loop() {
		mux := chi.NewRouter()
		registrar := newRouteRegistrar(mux)
		for _, uri := range uris {
			if err := registrar.registerRouteWithHosts([]string{http.MethodGet}, uri, nil, handler); err != nil {
				b.Fatal(err)
			}
		}
		runtime.KeepAlive(mux)
	}
}

func BenchmarkRouteDispatch(b *testing.B) {
	for _, kind := range []string{"static", "embedded-wildcard", "same-uri-collision", "same-suffix-collision"} {
		for _, result := range []string{"match-first", "match-last", "miss"} {
			for _, routeCount := range []int{10, 100, 1000} {
				b.Run(fmt.Sprintf("kind=%s/result=%s/routes=%d", kind, result, routeCount), func(b *testing.B) {
					benchmarkRouteDispatch(b, kind, result, routeCount)
				})
			}
		}
	}
}

func benchmarkRouteDispatch(b *testing.B, kind, result string, routeCount int) {
	b.ReportAllocs()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux := chi.NewRouter()
	registrar := newRouteRegistrar(mux)
	for i := range routeCount {
		var uri string
		var hosts []string
		switch kind {
		case "static":
			uri = fmt.Sprintf("/routes/%04d", i)
		case "embedded-wildcard":
			uri = fmt.Sprintf("/articles/*/suffix-%04d", i)
		case "same-uri-collision":
			uri = "/collision/*"
			hosts = []string{fmt.Sprintf("host-%04d.example", i)}
		case "same-suffix-collision":
			uri = "/articles/*/comments"
			hosts = []string{fmt.Sprintf("host-%04d.example", i)}
		}
		if err := registrar.registerRouteWithHosts([]string{http.MethodGet}, uri, hosts, handler); err != nil {
			b.Fatal(err)
		}
	}

	matchIndex := 0
	if result == "match-last" {
		matchIndex = routeCount - 1
	}
	var request *http.Request
	switch {
	case kind == "static" && result != "miss":
		request = httptest.NewRequest(
			http.MethodGet,
			fmt.Sprintf("http://apisix.benchmark/routes/%04d", matchIndex),
			nil,
		)
	case kind == "static" && result == "miss":
		request = httptest.NewRequest(http.MethodGet, "http://apisix.benchmark/routes/9999", nil)
	case kind == "embedded-wildcard" && result != "miss":
		request = httptest.NewRequest(
			http.MethodGet,
			fmt.Sprintf("http://apisix.benchmark/articles/some-slug/suffix-%04d", matchIndex),
			nil,
		)
	case (kind == "same-uri-collision" || kind == "same-suffix-collision") && result != "miss":
		path := "/collision/item"
		if kind == "same-suffix-collision" {
			path = "/articles/some-slug/comments"
		}
		hostIndex := 0
		if result == "match-last" {
			hostIndex = routeCount - 1
		}
		request = httptest.NewRequest(
			http.MethodGet,
			"http://host-"+fmt.Sprintf("%04d", hostIndex)+".example"+path,
			nil,
		)
	case (kind == "same-uri-collision" || kind == "same-suffix-collision") && result == "miss":
		path := "/collision/item"
		if kind == "same-suffix-collision" {
			path = "/articles/some-slug/comments"
		}
		request = httptest.NewRequest(http.MethodGet, "http://missing.example"+path, nil)
	default:
		request = httptest.NewRequest(http.MethodGet, "http://apisix.benchmark/articles/some-slug/suffix-missing", nil)
	}

	writer := &benchmarkRouteWriter{}
	var sink int
	for b.Loop() {
		writer.status = 0
		writer.bytes = 0
		mux.ServeHTTP(writer, request)
		sink += writer.status
	}
	runtime.KeepAlive(sink)

	want := http.StatusNotFound
	if result != "miss" {
		want = http.StatusNoContent
	}
	if writer.status != want {
		b.Fatalf("dispatch status = %d, want %d", writer.status, want)
	}
}

// BenchmarkCompileHTTPRouteIndexes measures detached router assembly from
// already-prepared immutable route handlers. Store/plugin materialization is
// intentionally outside this authority-free benchmark boundary.
func BenchmarkCompileHTTPRouteIndexes(b *testing.B) {
	for _, routeCount := range []int{100, 1000} {
		b.Run(fmt.Sprintf("routes=%d", routeCount), func(b *testing.B) {
			routes := make([]PreparedRoute, routeCount)
			for index := range routeCount {
				id := fmt.Sprintf("bench-route-%d", index)
				routes[index] = PreparedRoute{
					Route: resource.Route{ID: id, Uri: "/bench/" + id},
					Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
						writer.WriteHeader(http.StatusNoContent)
					}),
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				snapshot, err := CompileHTTP(context.Background(), CompileInput{
					Revision: 1,
					Routes:   routes,
				})
				if err != nil || snapshot.Handler() == nil {
					b.Fatalf("CompileHTTP() = (%T, %v)", snapshot, err)
				}
			}
		})
	}
}
