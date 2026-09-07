package proxy_cache

import (
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestDiskTTLUsesStaticDefaultAndOriginFreshness(t *testing.T) {
	for _, phasePath := range []bool{false, true} {
		for _, test := range []struct {
			name, cacheControl, expires string
			staticTTL                   *time.Duration
			want                        time.Duration
		}{
			{name: "default", want: 10 * time.Second},
			{name: "static", staticTTL: new(7 * time.Second), want: 7 * time.Second},
			{name: "disabled", staticTTL: new(time.Duration(0)), want: 0},
			{name: "origin", cacheControl: "max-age=2", want: 2 * time.Second},
			{name: "shared-origin", cacheControl: "s-maxage=3, max-age=2", want: 3 * time.Second},
			{name: "expires", expires: time.Now().Add(5 * time.Second).UTC().Format(http.TimeFormat), want: 5 * time.Second},
			{name: "stale", cacheControl: "max-age=0", want: 0},
		} {
			t.Run(fmt.Sprintf("phase=%t/%s", phasePath, test.name), func(t *testing.T) {
				effective := &appconfig.EffectiveConfig{}
				effective.Config.Apisix.ProxyCache.CacheTTL = test.staticTTL
				p := newTestPlugin(
					t,
					Config{CacheStrategy: "disk", CacheZone: "disk-ttl", CacheTTL: 99},
					[]appconfig.Zone{{Name: "disk-ttl", DiskPath: t.TempDir()}},
				)
				p.SetDependencies(base.Dependencies{Config: effective})
				request := httptest.NewRequest("GET", "http://example.test/ttl", nil)
				header := make(http.Header)
				if test.cacheControl != "" {
					header.Set("Cache-Control", test.cacheControl)
				}
				if test.expires != "" {
					header.Set("Expires", test.expires)
				}
				if phasePath {
					miss := p.RunRequestPhase(httptest.NewRecorder(), request)
					if err := p.RunFinalResponseStore(
						miss.Request,
						base.ResponseState{Status: 200, Header: header, Body: []byte("data")},
					); err != nil {
						t.Fatal(err)
					}
				} else {
					p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						maps.Copy(w.Header(), header)
						_, _ = w.Write([]byte("data"))
					})).ServeHTTP(httptest.NewRecorder(), request)
				}
				entry, status := p.lookup(request, p.cacheKey(request))
				if test.want == 0 {
					if status == "HIT" {
						t.Fatalf("zero TTL still HIT: %s", entry.ttl)
					}
					return
				}
				if status != "HIT" {
					t.Fatalf("cache status = %s", status)
				}
				if test.expires != "" {
					if entry.ttl > test.want || entry.ttl < test.want-time.Second {
						t.Fatalf("expires TTL=%s", entry.ttl)
					}
				} else if entry.ttl != test.want {
					t.Fatalf("TTL=%s, want %s", entry.ttl, test.want)
				}
			})
		}
	}
}
