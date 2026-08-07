package json_test

import (
	"bytes"
	"runtime"
	"strings"
	"testing"

	apisixjson "github.com/wklken/apisix-go/pkg/json"
)

// benchmarkRoute mirrors the shape of an APISIX route resource so the typed
// benchmark exercises the same nested structures the data plane decodes from
// etcd. The body field dominates the payload size.
type benchmarkRoute struct {
	ID         string            `json:"id"`
	UpstreamID string            `json:"upstream_id"`
	Method     string            `json:"method"`
	URI        string            `json:"uri"`
	Headers    map[string]string `json:"headers"`
	Query      map[string]string `json:"query"`
	Plugins    map[string]any    `json:"plugins"`
	Nodes      []benchmarkNode   `json:"nodes"`
	Enabled    bool              `json:"enabled"`
	Priority   int64             `json:"priority"`
	Body       string            `json:"body"`
}

type benchmarkNode struct {
	Host   string            `json:"host"`
	Port   int               `json:"port"`
	Weight int               `json:"weight"`
	Labels map[string]string `json:"labels"`
}

type jsonBenchmarkFixture struct {
	name        string
	bodySize    int
	typed       benchmarkRoute
	dynamic     map[string]any
	typedJSON   []byte
	dynamicJSON []byte
}

// TestJSONBenchmarkFixtures locks the benchmark corpus contract: three fixed
// sizes whose typed and dynamic payloads carry the full body size plus a
// bounded margin of metadata.
func TestJSONBenchmarkFixtures(t *testing.T) {
	fixtures := newJSONBenchmarkFixtures(t)
	if len(fixtures) != 3 {
		t.Fatalf("fixture count = %d, want 3", len(fixtures))
	}
	for _, f := range fixtures {
		if len(f.typedJSON) < f.bodySize || len(f.typedJSON) > f.bodySize+4<<10 {
			t.Fatalf("%s typed payload size = %d, body size = %d",
				f.name, len(f.typedJSON), f.bodySize)
		}
		if len(f.dynamicJSON) < f.bodySize || len(f.dynamicJSON) > f.bodySize+4<<10 {
			t.Fatalf("%s dynamic payload size = %d, body size = %d",
				f.name, len(f.dynamicJSON), f.bodySize)
		}
	}
}

// newJSONBenchmarkFixtures builds the shared benchmark corpus for tests.
func newJSONBenchmarkFixtures(t *testing.T) []jsonBenchmarkFixture {
	t.Helper()
	fixtures, err := buildJSONBenchmarkFixtures()
	if err != nil {
		t.Fatalf("build benchmark fixtures: %v", err)
	}
	return fixtures
}

func benchmarkJSONFixtures(b *testing.B) []jsonBenchmarkFixture {
	b.Helper()
	fixtures, err := buildJSONBenchmarkFixtures()
	if err != nil {
		b.Fatalf("build benchmark fixtures: %v", err)
	}
	return fixtures
}

// buildJSONBenchmarkFixtures constructs typed and dynamic payloads from the
// same explicit field values; neither payload is derived from the other by a
// JSON round-trip.
func buildJSONBenchmarkFixtures() ([]jsonBenchmarkFixture, error) {
	sizes := []struct {
		name string
		size int
	}{
		{name: "1KiB", size: 1 << 10},
		{name: "32KiB", size: 32 << 10},
		{name: "256KiB", size: 256 << 10},
	}
	fixtures := make([]jsonBenchmarkFixture, 0, len(sizes))
	for _, spec := range sizes {
		body := strings.Repeat("x", spec.size)
		typed := newBenchmarkRoute(body)
		typedJSON, err := apisixjson.Marshal(typed)
		if err != nil {
			return nil, err
		}
		dynamic := newBenchmarkDynamicRoute(body)
		dynamicJSON, err := apisixjson.Marshal(dynamic)
		if err != nil {
			return nil, err
		}
		fixtures = append(fixtures, jsonBenchmarkFixture{
			name:        spec.name,
			bodySize:    spec.size,
			typed:       typed,
			dynamic:     dynamic,
			typedJSON:   typedJSON,
			dynamicJSON: dynamicJSON,
		})
	}
	return fixtures, nil
}

func newBenchmarkRoute(body string) benchmarkRoute {
	return benchmarkRoute{
		ID:         "route-benchmark-01",
		UpstreamID: "upstream-benchmark-01",
		Method:     "POST",
		URI:        "/benchmark/paths/:id/actions",
		Headers: map[string]string{
			"Content-Type": "application/json",
			"X-Benchmark":  "apisix-go",
		},
		Query: map[string]string{
			"verbose": "true",
			"page":    "1",
		},
		Plugins: map[string]any{
			"limit-req": map[string]any{
				"rate": 1000, "burst": 2000, "key": "remote_addr", "rejected_code": 429,
			},
			"key-auth": map[string]any{
				"header": "X-API-KEY", "hide_credentials": true,
			},
		},
		Nodes: []benchmarkNode{
			{Host: "192.168.1.1", Port: 8080, Weight: 1, Labels: map[string]string{"zone": "a"}},
			{Host: "192.168.1.2", Port: 8080, Weight: 2, Labels: map[string]string{"zone": "b"}},
		},
		Enabled:  true,
		Priority: 100,
		Body:     body,
	}
}

// newBenchmarkDynamicRoute mirrors newBenchmarkRoute with the same field
// values, written out as an explicitly built map.
func newBenchmarkDynamicRoute(body string) map[string]any {
	return map[string]any{
		"id":          "route-benchmark-01",
		"upstream_id": "upstream-benchmark-01",
		"method":      "POST",
		"uri":         "/benchmark/paths/:id/actions",
		"headers": map[string]string{
			"Content-Type": "application/json",
			"X-Benchmark":  "apisix-go",
		},
		"query": map[string]string{
			"verbose": "true",
			"page":    "1",
		},
		"plugins": map[string]any{
			"limit-req": map[string]any{
				"rate": 1000, "burst": 2000, "key": "remote_addr", "rejected_code": 429,
			},
			"key-auth": map[string]any{
				"header": "X-API-KEY", "hide_credentials": true,
			},
		},
		"nodes": []any{
			map[string]any{"host": "192.168.1.1", "port": 8080, "weight": 1, "labels": map[string]string{"zone": "a"}},
			map[string]any{"host": "192.168.1.2", "port": 8080, "weight": 2, "labels": map[string]string{"zone": "b"}},
		},
		"enabled":  true,
		"priority": 100,
		"body":     body,
	}
}

func BenchmarkJSONMarshalTyped(b *testing.B) {
	for _, fixture := range benchmarkJSONFixtures(b) {
		b.Run("size="+fixture.name, func(b *testing.B) {
			benchmarkJSONMarshalTyped(b, fixture)
		})
	}
}

func benchmarkJSONMarshalTyped(b *testing.B, f jsonBenchmarkFixture) {
	b.ReportAllocs()
	b.SetBytes(int64(len(f.typedJSON)))
	var sink []byte
	for b.Loop() {
		encoded, err := apisixjson.Marshal(f.typed)
		if err != nil {
			b.Fatal(err)
		}
		sink = encoded
	}
	runtime.KeepAlive(sink)
}

func BenchmarkJSONUnmarshalTyped(b *testing.B) {
	for _, fixture := range benchmarkJSONFixtures(b) {
		b.Run("size="+fixture.name, func(b *testing.B) {
			benchmarkJSONUnmarshalTyped(b, fixture)
		})
	}
}

func benchmarkJSONUnmarshalTyped(b *testing.B, f jsonBenchmarkFixture) {
	b.ReportAllocs()
	b.SetBytes(int64(len(f.typedJSON)))
	var sink benchmarkRoute
	for b.Loop() {
		var dst benchmarkRoute
		if err := apisixjson.Unmarshal(f.typedJSON, &dst); err != nil {
			b.Fatal(err)
		}
		sink = dst
	}
	runtime.KeepAlive(sink)
}

func BenchmarkJSONMarshalDynamic(b *testing.B) {
	for _, fixture := range benchmarkJSONFixtures(b) {
		b.Run("size="+fixture.name, func(b *testing.B) {
			benchmarkJSONMarshalDynamic(b, fixture)
		})
	}
}

func benchmarkJSONMarshalDynamic(b *testing.B, f jsonBenchmarkFixture) {
	b.ReportAllocs()
	b.SetBytes(int64(len(f.dynamicJSON)))
	var sink []byte
	for b.Loop() {
		encoded, err := apisixjson.Marshal(f.dynamic)
		if err != nil {
			b.Fatal(err)
		}
		sink = encoded
	}
	runtime.KeepAlive(sink)
}

func BenchmarkJSONUnmarshalDynamic(b *testing.B) {
	for _, fixture := range benchmarkJSONFixtures(b) {
		b.Run("size="+fixture.name, func(b *testing.B) {
			benchmarkJSONUnmarshalDynamic(b, fixture)
		})
	}
}

func benchmarkJSONUnmarshalDynamic(b *testing.B, f jsonBenchmarkFixture) {
	b.ReportAllocs()
	b.SetBytes(int64(len(f.dynamicJSON)))
	var sink map[string]any
	for b.Loop() {
		var dst map[string]any
		if err := apisixjson.Unmarshal(f.dynamicJSON, &dst); err != nil {
			b.Fatal(err)
		}
		sink = dst
	}
	runtime.KeepAlive(sink)
}

func BenchmarkJSONEncoder(b *testing.B) {
	for _, fixture := range benchmarkJSONFixtures(b) {
		b.Run("size="+fixture.name, func(b *testing.B) {
			benchmarkJSONEncoder(b, fixture)
		})
	}
}

func benchmarkJSONEncoder(b *testing.B, f jsonBenchmarkFixture) {
	b.ReportAllocs()
	b.SetBytes(int64(len(f.typedJSON)))
	buffer := &bytes.Buffer{}
	var sink int
	for b.Loop() {
		buffer.Reset()
		encoder := apisixjson.NewEncoder(buffer)
		if err := encoder.Encode(f.typed); err != nil {
			b.Fatal(err)
		}
		sink = buffer.Len()
	}
	runtime.KeepAlive(sink)
}

func BenchmarkJSONDecoder(b *testing.B) {
	for _, fixture := range benchmarkJSONFixtures(b) {
		b.Run("size="+fixture.name, func(b *testing.B) {
			benchmarkJSONDecoder(b, fixture)
		})
	}
}

func benchmarkJSONDecoder(b *testing.B, f jsonBenchmarkFixture) {
	b.ReportAllocs()
	b.SetBytes(int64(len(f.dynamicJSON)))
	reader := &bytes.Reader{}
	var sink map[string]any
	for b.Loop() {
		reader.Reset(f.dynamicJSON)
		decoder := apisixjson.NewDecoder(reader)
		decoder.UseNumber()
		var dst map[string]any
		if err := decoder.Decode(&dst); err != nil {
			b.Fatal(err)
		}
		sink = dst
	}
	runtime.KeepAlive(sink)
}
