package proxy_cache

import (
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"

	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestDefaultHostKeySharesCacheAcrossClientPorts(t *testing.T) {
	p := newTestPlugin(t, Config{CacheStrategy: "memory"})
	calls := 0
	handler := p.Handler(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; _, _ = w.Write([]byte("cached")) }),
	)
	for _, host := range []string{"example.test:80", "example.test:8080"} {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "http://"+host+"/same", nil))
	}
	if calls != 1 {
		t.Fatalf("origin calls = %d, want cache shared by host without port", calls)
	}
}

func TestSharedCacheHeadersFollowServingPlugin(t *testing.T) {
	for _, phasePath := range []bool{false, true} {
		for _, hideFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("phase=%t/hide-first=%t", phasePath, hideFirst), func(t *testing.T) {
				zones := []appconfig.Zone{{Name: "header-policy", MemorySize: "1M"}}
				cfg := Config{
					CacheStrategy:    "memory",
					CacheZone:        "header-policy",
					CacheKey:         []string{"same"},
					HideCacheHeaders: hideFirst,
				}
				first := newTestPlugin(t, cfg, zones)
				cfg.HideCacheHeaders = !hideFirst
				second := newTestPlugin(t, cfg, zones)
				state := base.ResponseState{
					Status: 200,
					Header: http.Header{"Cache-Control": {"max-age=60"}, "Expires": {"Wed, 21 Oct 2030 07:28:00 GMT"}},
					Body:   []byte("cached"),
				}
				request := httptest.NewRequest("GET", "/same", nil)
				var header http.Header
				if phasePath {
					miss := first.RunRequestPhase(httptest.NewRecorder(), request)
					if err := first.RunHeaderFilter(miss.Request, &state); err != nil {
						t.Fatal(err)
					}
					if err := first.RunFinalResponseStore(miss.Request, state); err != nil {
						t.Fatal(err)
					}
					hit := second.RunRequestPhase(httptest.NewRecorder(), request)
					holder := base.CacheHitResponseHolderFromRequest(hit.Request)
					if holder == nil {
						t.Fatal("missing cached response")
					}
					cached, err := holder.Consume()
					if err != nil {
						t.Fatal(err)
					}
					header = cached.Header
				} else {
					first.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						maps.Copy(w.Header(), state.Header)
						_, _ = w.Write(state.Body)
					})).ServeHTTP(httptest.NewRecorder(), request)
					response := httptest.NewRecorder()
					second.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("expected cache hit") })).
						ServeHTTP(response, request)
					header = response.Header()
				}
				if header.Get(cacheStatusHeader) != "HIT" {
					t.Fatalf("not a cache hit: %v", header)
				}
				for _, key := range []string{"Cache-Control", "Expires"} {
					if got := header.Get(key); (got == "") != cfg.HideCacheHeaders {
						t.Errorf("serving hide=%t, %s=%q", cfg.HideCacheHeaders, key, got)
					}
				}
			})
		}
	}
}
