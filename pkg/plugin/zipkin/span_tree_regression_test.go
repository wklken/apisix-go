package zipkin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

// (request/proxy/response) and 5 spans for v1. Production RunRequestPhase
// must emit that tree, not a single SERVER span.
func TestZipkinSpanTree(t *testing.T) {
	for _, test := range []struct {
		version int
		want    int
		names   []string
	}{
		{version: 2, want: 3, names: []string{"apisix.request", "apisix.proxy", "apisix.response_span"}},
		{version: 1, want: 5, names: []string{"apisix.request", "apisix.rewrite", "apisix.access", "apisix.proxy", "apisix.body_filter"}},
	} {
		t.Run(fmt.Sprintf("span_version_%d", test.version), func(t *testing.T) {
			reported := make(chan []map[string]any, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var spans []map[string]any
				if err := json.NewDecoder(r.Body).Decode(&spans); err != nil {
					t.Fatalf("decode spans: %v", err)
				}
				reported <- spans
				w.WriteHeader(http.StatusAccepted)
			}))
			t.Cleanup(server.Close)

			p := newTestPlugin(t, Config{
				Endpoint: server.URL, SampleRatio: 1, SpanVersion: test.version,
			})
			request, lifecycle := apisixctx.EnsureRequestLifecycle(
				httptest.NewRequest(http.MethodGet, "http://gateway.test/opentracing", nil), time.Now(),
			)
			result := p.RunRequestPhase(httptest.NewRecorder(), request)
			if result.Decision != base.RequestContinue {
				t.Fatalf("decision = %d, want continue", result.Decision)
			}
			if err := apisixctx.RunBeforeProxyHooks(result.Request); err != nil {
				t.Fatal(err)
			}
			if filter, ok := any(p).(base.StreamingHeaderFilterPlugin); ok {
				if err := filter.RunStreamingHeaderFilter(
					result.Request,
					&base.StreamingResponseState{Status: 200},
				); err != nil {
					t.Fatal(err)
				}
			}
			lifecycle.Complete(
				apisixctx.ResponseOutcome{Kind: apisixctx.RequestOutcomeCompleted, Status: http.StatusOK},
				time.Now(),
			)
			if failures := lifecycle.Finalize(); len(failures) != 0 {
				t.Fatalf("finalize failures = %#v", failures)
			}
			p.processor.Flush()
			select {
			case spans := <-reported:
				if len(spans) != test.want {
					t.Fatalf(
						"span count = %d names=%v, want APISIX 3.17 count %d",
						len(spans),
						spanNames(spans),
						test.want,
					)
				}
				got := map[string]bool{}
				for _, span := range spans {
					name, _ := span["name"].(string)
					got[name] = true
				}
				for _, name := range test.names {
					if !got[name] {
						t.Fatalf("missing span %q in %#v", name, spanNames(spans))
					}
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for zipkin report")
			}
		})
	}
}

func spanNames(spans []map[string]any) []string {
	names := make([]string, 0, len(spans))
	for _, span := range spans {
		name, _ := span["name"].(string)
		names = append(names, name)
	}
	return names
}
