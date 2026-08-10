package route

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/store"
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
	for _, kind := range []string{"static", "embedded-wildcard"} {
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
		switch kind {
		case "static":
			uri = fmt.Sprintf("/routes/%04d", i)
		case "embedded-wildcard":
			uri = fmt.Sprintf("/articles/*/suffix-%04d", i)
		}
		if err := registrar.registerRouteWithHosts([]string{http.MethodGet}, uri, nil, handler); err != nil {
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

// BenchmarkRouteBuildIndexes measures route-build store lookups: the
// route-bucket read, the per-route global-rule lookup, and per-route plugin
// metadata lookup. The shared store is reseeded per row through the event
// channel; the corpus uses only stable APIs.
func BenchmarkRouteBuildIndexes(b *testing.B) {
	if err := logger.ConfigureLevel("error"); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = logger.ConfigureLevel("info") })

	events := make(chan *store.Event, 64)
	storage, err := store.GetStore(b.TempDir()+"/route-build-index.db", events)
	if err != nil {
		b.Fatalf("get store: %v", err)
	}
	storage.Start()
	b.Cleanup(func() { _ = storage.Stop() })

	put := func(bucket, id string, value []byte) {
		event := store.NewEvent()
		event.Type = store.EventTypePut
		event.Key = []byte("/apisix/" + bucket + "/" + id)
		event.Value = value
		events <- event
	}
	del := func(bucket, id string) {
		event := store.NewEvent()
		event.Type = store.EventTypeDelete
		event.Key = []byte("/apisix/" + bucket + "/" + id)
		events <- event
	}
	clear := func(bucket string, ids []string) {
		for _, id := range ids {
			del(bucket, id)
		}
		if err := storage.Sync(); err != nil {
			b.Fatalf("Sync() error = %v", err)
		}
	}
	seedRoutes := func(count int, withCors bool) []string {
		ids := make([]string, count)
		for i := range count {
			id := fmt.Sprintf("bench-route-%d", i)
			ids[i] = id
			plugins := `{}`
			if withCors {
				plugins = `{"cors":{}}`
			}
			put("routes", id, []byte(`{"id":"`+id+`","uri":"/bench/`+id+`","plugins":`+plugins+`}`))
		}
		if err := storage.Sync(); err != nil {
			b.Fatalf("Sync() error = %v", err)
		}
		return ids
	}
	seedRules := func(count int) []string {
		ids := make([]string, count)
		for i := range count {
			id := fmt.Sprintf("bench-rule-%d", i)
			ids[i] = id
			put("global_rules", id, []byte(`{"id":"`+id+`","plugins":{}}`))
		}
		if err := storage.Sync(); err != nil {
			b.Fatalf("Sync() error = %v", err)
		}
		return ids
	}
	seedMetadata := func() {
		put("plugin_metadata", "cors", []byte(`{"id":"cors","allow_origins":{"key":"https://a.example.com"}}`))
		if err := storage.Sync(); err != nil {
			b.Fatalf("Sync() error = %v", err)
		}
	}

	build := func(b *testing.B) {
		builder := NewBuilderWithServerAddr(nil, "127.0.0.1:9080")
		for b.Loop() {
			mux := builder.Build()
			if mux == nil {
				b.Fatal("Build() returned nil")
			}
		}
	}

	var routes, rules []string

	routes = seedRoutes(100, false)
	b.Run("routes=100/global-rules=0", build)
	clear("routes", routes)

	routes = seedRoutes(100, false)
	rules = seedRules(100)
	b.Run("routes=100/global-rules=100", build)
	clear("routes", routes)
	clear("global_rules", rules)

	routes = seedRoutes(100, true)
	seedMetadata()
	b.Run("routes=100/metadata", build)
	clear("routes", routes)

	routes = seedRoutes(1000, true)
	b.Run("routes=1000/metadata", build)
	clear("routes", routes)
}
