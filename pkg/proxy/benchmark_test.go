package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
)

// Benchmark corpus for the proxy buffer pool and the full ReverseProxy
// ServeHTTP handler path. The transport and writer are deterministic; nothing
// binds a socket or uses the default transport.

var benchmarkProxyPayloads = map[string][]byte{
	"1KiB":  bytes.Repeat([]byte("x"), 1<<10),
	"64KiB": bytes.Repeat([]byte("x"), 64<<10),
	"1MiB":  bytes.Repeat([]byte("x"), 1<<20),
}

var benchmarkProxySizes = []struct {
	name string
	size int
}{
	{name: "1KiB", size: 1 << 10},
	{name: "64KiB", size: 64 << 10},
	{name: "1MiB", size: 1 << 20},
}

// benchmarkRoundTripper returns a fresh response with a fresh read-closer on
// every call; constructing it is part of the measured ServeHTTP row.
type benchmarkRoundTripper struct {
	payload []byte
}

func (rt *benchmarkRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader(rt.payload)),
		Request:    req,
	}, nil
}

// benchmarkProxyWriter records only status and byte count.
type benchmarkProxyWriter struct {
	status int
	bytes  int
}

var benchmarkProxyHeader = http.Header{}

func (w *benchmarkProxyWriter) WriteHeader(status int) {
	w.status = status
}

func (w *benchmarkProxyWriter) Write(p []byte) (int, error) {
	w.bytes += len(p)
	return len(p), nil
}

func (*benchmarkProxyWriter) Header() http.Header {
	return benchmarkProxyHeader
}

func BenchmarkProxyBufferPool(b *testing.B) {
	pool := newProxyBufferPool()
	b.Run("serial", func(b *testing.B) {
		b.ReportAllocs()
		var sink []byte
		for b.Loop() {
			buffer := pool.Get()
			pool.Put(buffer)
			sink = buffer
		}
		runtime.KeepAlive(sink)
	})
	b.Run("parallel", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			var sink []byte
			for pb.Next() {
				buffer := pool.Get()
				pool.Put(buffer)
				sink = buffer
			}
			runtime.KeepAlive(sink)
		})
	})
}

func BenchmarkReverseProxyServeHTTP(b *testing.B) {
	for _, spec := range benchmarkProxySizes {
		b.Run("size="+spec.name, func(b *testing.B) {
			benchmarkReverseProxyServeHTTP(b, benchmarkProxyPayloads[spec.name])
		})
	}
}

func benchmarkReverseProxyServeHTTP(b *testing.B, payload []byte) {
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	handler := NewProxyHandler(
		&benchmarkRoundTripper{payload: payload},
		func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = "upstream.benchmark"
		},
		nil,
		nil,
	)
	request := httptest.NewRequest(http.MethodGet, "http://apisix.benchmark/benchmark", nil)
	writer := &benchmarkProxyWriter{}
	var sink int
	for b.Loop() {
		writer.bytes = 0
		writer.status = 0
		handler.ServeHTTP(writer, request)
		sink += writer.status
	}
	runtime.KeepAlive(sink)
	if writer.status != http.StatusOK {
		b.Fatalf("writer status = %d, want %d", writer.status, http.StatusOK)
	}
	if writer.bytes != len(payload) {
		b.Fatalf("writer bytes = %d, want %d", writer.bytes, len(payload))
	}
}

// BenchmarkVerifiedSmallPath measures passive health-config parsing at
// upstream publication for a checks map with both healthy and unhealthy
// sections.
func BenchmarkVerifiedSmallPath(b *testing.B) {
	checks := map[string]any{
		"passive": map[string]any{
			"type": "https",
			"healthy": map[string]any{
				"http_statuses": []any{float64(200), float64(204)},
			},
			"unhealthy": map[string]any{
				"http_statuses": []any{float64(429), float64(500)},
				"http_failures": float64(5),
				"tcp_failures":  float64(2),
				"timeouts":      float64(7),
			},
		},
	}

	b.Run("health-config-parse", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			config, err := parsePassiveHealthConfig(checks)
			if err != nil {
				b.Fatal(err)
			}
			if config.HTTPFailures != 5 {
				b.Fatal("parsed failure counts mismatch")
			}
		}
	})
}
