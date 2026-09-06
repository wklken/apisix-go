package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/zipkin"
	"github.com/wklken/apisix-go/pkg/runtime"
)

func TestZipkinResponsePlanEmitsSpanTree(t *testing.T) {
	for _, version := range []int{1, 2} {
		for _, bounded := range []bool{false, true} {
			t.Run(fmt.Sprintf("version=%d/bounded=%t", version, bounded), func(t *testing.T) {
				reported := make(chan []map[string]any, 1)
				collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var spans []map[string]any
					if err := json.NewDecoder(r.Body).Decode(&spans); err != nil {
						t.Error(err)
						return
					}
					reported <- spans
					w.WriteHeader(http.StatusAccepted)
				}))
				defer collector.Close()
				tasks := runtime.NewTaskRegistry(context.Background(), nil)
				owner, err := runtime.NewTaskOwner(tasks, "zipkin-test", runtime.TaskPlugin)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					if _, err := tasks.Stop(ctx); err != nil {
						t.Error(err)
					}
				}()
				p := &zipkin.Plugin{}
				p.SetDependencies(base.Dependencies{Tasks: owner})
				if err := p.Init(); err != nil {
					t.Fatal(err)
				}
				*p.Config().(*zipkin.Config) = zipkin.Config{
					Endpoint:    collector.URL,
					SampleRatio: 1,
					SpanVersion: version,
				}
				if err := p.PostInit(); err != nil {
					t.Fatal(err)
				}
				defer p.Stop()
				binding := checkedResponseBinding(t, "zipkin", p, ScopeRoute, "trace")
				bindings := []Binding{binding}
				if bounded {
					transform := newDualModeResponseTestPlugin(base.RequestResponseModeBounded)
					bindings = append(
						bindings,
						checkedResponseBinding(t, "ai-rate-limiting", transform, ScopeRoute, "buffer"),
					)
				}
				plan, err := BuildResponsePlan(ResponsePlanInput{StaticBindings: bindings})
				if err != nil {
					t.Fatal(err)
				}
				var upstreamSpan string
				handler := plan.Install(
					NewRequestPipeline(bindings, nil),
					http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if err := apisixctx.RunBeforeProxyHooks(r); err != nil {
							t.Error(err)
						}
						upstreamSpan = r.Header.Get("X-B3-Spanid")
						apisixctx.SetRequestResponseSource(r, apisixctx.ResponseSourceUpstream)
						_, _ = w.Write([]byte("response"))
					}),
				)
				request, lifecycle := apisixctx.EnsureRequestLifecycle(
					httptest.NewRequest(http.MethodGet, "http://gateway.test/trace", nil),
					time.Now(),
				)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != 200 || response.Body.String() != "response" {
					t.Fatalf("response=%d/%q", response.Code, response.Body.String())
				}
				lifecycle.Complete(
					apisixctx.ResponseOutcome{Kind: apisixctx.RequestOutcomeCompleted, Status: 200},
					time.Now(),
				)
				if failures := lifecycle.Finalize(); len(failures) != 0 {
					t.Fatal(failures)
				}
				p.Stop()
				select {
				case spans := <-reported:
					want := 3
					if version == 1 {
						want = 5
					}
					if len(spans) != want {
						t.Fatalf("spans=%#v, want %d", spans, want)
					}
					byName := map[string]map[string]any{}
					for _, span := range spans {
						byName[span["name"].(string)] = span
					}
					root, proxy := byName["apisix.request"], byName["apisix.proxy"]
					if proxy["parentId"] != root["id"] || proxy["id"] != upstreamSpan {
						t.Fatalf("root=%#v proxy=%#v upstream=%s", root, proxy, upstreamSpan)
					}
					for name, span := range byName {
						if name == "apisix.request" {
							continue
						}
						parent := root["id"]
						if name == "apisix.body_filter" {
							parent = proxy["id"]
						}
						if span["parentId"] != parent || span["traceId"] != root["traceId"] {
							t.Fatalf("child=%#v root=%#v", span, root)
						}
					}
				case <-time.After(time.Second):
					t.Fatal("no spans exported")
				}
			})
		}
	}
}
