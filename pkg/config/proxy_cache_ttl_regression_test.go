package config

import (
	"testing"
	"time"
)

func TestStaticProxyCacheTTLRetainsSecondsAndZero(t *testing.T) {
	for _, test := range []struct {
		value string
		want  time.Duration
	}{{"7s", 7 * time.Second}, {"7", 7 * time.Second}, {"0s", 0}} {
		effective, err := LoadEffective(loadRequestFixture(t, "apisix: {proxy_cache: {cache_ttl: "+test.value+"}}"))
		if err != nil {
			t.Fatal(err)
		}
		ttl := effective.Config.Apisix.ProxyCache.CacheTTL
		if ttl == nil || *ttl != test.want {
			t.Fatalf("cache_ttl %s = %v, want %s", test.value, ttl, test.want)
		}
	}
}
