package route

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strconv"
	"sync"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/resource"
)

// Benchmark corpus for the immutable route dispatch and full proxy control
// path through a loopback upstream. Payload scaling remains owned by the
// proxy package benchmark corpus.

var (
	proxyLoopbackEnvMu    sync.Mutex
	proxyLoopbackEnvCache = map[string]*proxyBenchmarkEnvironment{}
)

type proxyBenchmarkEnvironment struct {
	client     *http.Client
	server     *httptest.Server
	upstreams  []*httptest.Server
	targetPath string
}

func (environment *proxyBenchmarkEnvironment) Close() {
	environment.server.Close()
	for _, upstream := range environment.upstreams {
		upstream.Close()
	}
}

func newProxyBenchmarkEnvironment(
	b *testing.B,
	routes, nodes, payloadSize int,
	plugins string,
) *proxyBenchmarkEnvironment {
	payload := bytes.Repeat([]byte("x"), payloadSize)
	environment := &proxyBenchmarkEnvironment{}
	pathPrefix := fmt.Sprintf("/bench/%d-%s-%d", routes, plugins, nodes)
	environment.targetPath = pathPrefix + "/target"

	upstreamNodes := make([]resource.Node, 0, nodes)
	for range nodes {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", strconv.Itoa(payloadSize))
			_, _ = w.Write(payload)
		}))
		environment.upstreams = append(environment.upstreams, server)
		parsed, err := url.Parse(server.URL)
		if err != nil {
			b.Fatal(err)
		}
		port, err := strconv.Atoi(parsed.Port())
		if err != nil {
			b.Fatal(err)
		}
		upstreamNodes = append(upstreamNodes, resource.Node{
			Host: parsed.Hostname(), Port: port, Weight: 1,
		})
	}

	proxyRoute := resource.Route{
		ID:       "proxy-benchmark-runtime",
		Upstream: resource.Upstream{Scheme: "http", Nodes: upstreamNodes},
	}
	var bindings []plugin.Binding
	if plugins == "request-id" {
		bindings = append(bindings, testPluginBinding(
			b,
			"request-id",
			map[string]any{},
			proxyRoute,
		))
	}
	proxyHandler := testPreparedProxyHandler(
		b,
		proxyRoute,
		resource.Service{},
		testEffectiveConfig(),
		bindings...,
	)
	preparedRoutes := make([]PreparedRoute, 0, routes)
	for index := range routes {
		uri := environment.targetPath
		if index > 0 {
			uri = fmt.Sprintf("%s/filler/%06d", pathPrefix, index)
		}
		preparedRoutes = append(preparedRoutes, PreparedRoute{
			Route:   resource.Route{ID: fmt.Sprintf("bench-%d-%s-%d", routes, plugins, index), Uri: uri},
			Handler: proxyHandler,
		})
	}
	snapshot, err := CompileHTTP(context.Background(), CompileInput{Revision: 1, Routes: preparedRoutes})
	if err != nil {
		b.Fatal(err)
	}
	environment.server = httptest.NewServer(snapshot.Handler())

	concurrency := runtime.GOMAXPROCS(0) * 4
	environment.client = &http.Client{Transport: &http.Transport{
		MaxIdleConns: concurrency, MaxIdleConnsPerHost: concurrency, MaxConnsPerHost: concurrency,
	}}
	return environment
}

func proxyLoopbackBenchmarkEnvironment(
	b *testing.B,
	routes, nodes, payloadSize int,
	plugins string,
) *proxyBenchmarkEnvironment {
	key := fmt.Sprintf("routes=%d/plugins=%s/nodes=%d/payload=%d", routes, plugins, nodes, payloadSize)
	proxyLoopbackEnvMu.Lock()
	defer proxyLoopbackEnvMu.Unlock()
	if environment := proxyLoopbackEnvCache[key]; environment != nil {
		return environment
	}
	environment := newProxyBenchmarkEnvironment(b, routes, nodes, payloadSize, plugins)
	proxyLoopbackEnvCache[key] = environment
	b.Cleanup(environment.Close)
	return environment
}

func BenchmarkRouteProxyLoopback(b *testing.B) {
	for _, routes := range []int{1, 100, 1000} {
		for _, plugins := range []string{"none", "request-id"} {
			for _, nodes := range []int{1, 10} {
				name := fmt.Sprintf("routes=%d/plugins=%s/nodes=%d", routes, plugins, nodes)
				b.Run(name, func(b *testing.B) {
					environment := proxyLoopbackBenchmarkEnvironment(b, routes, nodes, 1024, plugins)
					errors := make(chan error, 1)
					reportError := func(err error) {
						select {
						case errors <- err:
						default:
						}
					}
					b.ReportAllocs()
					b.SetBytes(1024)
					b.ResetTimer()
					b.RunParallel(func(pb *testing.PB) {
						for pb.Next() {
							response, err := environment.client.Get(environment.server.URL + environment.targetPath)
							if err != nil {
								reportError(err)
								return
							}
							if _, err := io.Copy(io.Discard, response.Body); err != nil {
								_ = response.Body.Close()
								reportError(err)
								return
							}
							if err := response.Body.Close(); err != nil {
								reportError(err)
								return
							}
							if response.StatusCode != http.StatusOK {
								reportError(fmt.Errorf("status = %d", response.StatusCode))
								return
							}
						}
					})
					b.StopTimer()
					select {
					case err := <-errors:
						b.Fatal(err)
					default:
					}
				})
			}
		}
	}
}
