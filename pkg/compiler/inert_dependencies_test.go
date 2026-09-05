package compiler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
)

func TestInertDependenciesDoNotQuarantine(t *testing.T) {
	cases := map[string]string{
		"route-name":        `{"id":"r","uri":"/r","name":"$secret://vault/absent/key"}`,
		"route-label":       `{"id":"r","uri":"/r","labels":{"team":"$secret://vault/absent/key"}}`,
		"route-description": `{"id":"r","uri":"/r","desc":"$secret://vault/absent/key"}`,
		"disabled-proto":    `{"id":"r","uri":"/r","plugins":{"grpc-transcode":{"_meta":{"disable":true},"proto_id":"removed","service":"demo.Echo","method":"Call"}}}`,
		"disabled-plugin":   `{"id":"r","uri":"/r","plugins":{"proxy-rewrite":{"_meta":{"disable":true},"headers":{"set":{"X-Demo":"$secret://vault/absent/key"}}}}}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			desired := mustGenerationSnapshot(t, 501, []generation.Resource{resourceValue("routes", "r", raw)}, nil)
			c, err := New()
			if err != nil {
				t.Fatal(err)
			}
			set, err := c.PreparePublication(
				context.Background(),
				ticketForSnapshot(desired, generation.DomainHTTP),
				desired,
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			candidate := set.Domains[generation.DomainHTTP]
			t.Logf("decisions = %+v", candidate.Decisions)
			if _, found := candidate.Snapshot.Lookup(generation.ResourceKey{Kind: "routes", ID: "r"}); !found {
				t.Fatal("inert string incorrectly removes route from publication")
			}
		})
	}
}

func TestDisabledPluginRouteStillServes(t *testing.T) {
	upstream := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }),
	)
	defer upstream.Close()
	factory, _ := newWorkerTestFactory(t)
	factory.effective.Config.Plugins = []string{"proxy-rewrite"}
	factory.effective.Config.Apisix.ID = "round5-publication-runtime"
	defer func() {
		if err := factory.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	node := strings.TrimPrefix(upstream.URL, "http://")
	desired := mustGenerationSnapshot(t, 502, []generation.Resource{
		resourceValue(
			"routes",
			"r",
			fmt.Sprintf(
				`{"id":"r","uri":"/r","upstream":{"nodes":{%q:1}},"plugins":{"proxy-rewrite":{"_meta":{"disable":true},"headers":{"set":{"X-Demo":"$secret://vault/absent/key"}}}}}`,
				node,
			),
		),
		resourceValue("routes", "ok", fmt.Sprintf(`{"id":"ok","uri":"/ok","upstream":{"nodes":{%q:1}}}`, node)),
	}, nil)
	prepared, err := factory.PrepareGeneration(
		context.Background(),
		ticketForSnapshot(desired, generation.DomainHTTP),
		desired,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := prepared.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	for _, path := range []string{"/ok", "/r"} {
		rec := httptest.NewRecorder()
		prepared.HTTP().Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		t.Logf("%s status=%d", path, rec.Code)
		if rec.Code != 200 {
			t.Errorf("disabled plugin removed otherwise valid route: %s=%d", path, rec.Code)
		}
	}
}

func TestEnabledPluginDependenciesStillQuarantine(t *testing.T) {
	for _, raw := range []string{
		`{"id":"r","uri":"/r","plugins":{"grpc-transcode":{"proto_id":"removed","service":"demo.Echo","method":"Call"}}}`,
		`{"id":"r","uri":"/r","plugins":{"proxy-rewrite":{"headers":{"set":{"X-Demo":"$secret://vault/absent/key"}}}}}`,
	} {
		desired := mustGenerationSnapshot(t, 503, []generation.Resource{resourceValue("routes", "r", raw)}, nil)
		compiler, err := New()
		if err != nil {
			t.Fatal(err)
		}
		set, err := compiler.PreparePublication(
			context.Background(),
			ticketForSnapshot(desired, generation.DomainHTTP),
			desired,
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, found := set.Domains[generation.DomainHTTP].Snapshot.Lookup(
			generation.ResourceKey{Kind: "routes", ID: "r"},
		); found {
			t.Fatal("enabled plugin missing dependency was ignored")
		}
	}
}
