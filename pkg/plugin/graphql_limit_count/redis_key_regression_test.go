package graphql_limit_count

import (
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestRedisCounterIdentityIsolatesPluginConfigAndParent(t *testing.T) {
	keys := make([]string, 0, 3)
	for _, test := range []struct {
		parent string
		count  int
	}{
		{"routes/graphql", 10}, {"routes/graphql", 20}, {"services/graphql", 10},
	} {
		p := newTestPlugin(t, Config{Count: test.count, TimeWindow: 60})
		p.config.Policy = "redis"
		client := &scriptedRedisClient{result: []any{int64(1), int64(9), int64(60)}}
		p.redisLimiter = &redisCountLimiter{client: client}
		if err := p.SetAPISIXPluginContext(base.APISIXPluginContext{
			SourceResourceKey: test.parent,
			SourceConfig: map[string]any{
				"count":       test.count,
				"time_window": 60,
				"policy":      "redis",
				"redis_host":  "localhost",
			},
		}); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := p.incoming(
			httptest.NewRequest("POST", "/graphql", nil),
			"client",
			1,
			int64(test.count),
			60,
		); err != nil {
			t.Fatal(err)
		}
		key := client.keys[0]
		if !regexp.MustCompile("^plugin-graphql-limit-count" + test.parent + `:[0-9]+:client$`).MatchString(key) {
			t.Fatalf("Redis key = %q; want parent and config version", key)
		}
		keys = append(keys, key)
	}
	if keys[0] == keys[1] || keys[0] == keys[2] {
		t.Fatalf("shared counters across config/parent changes: %v", keys)
	}
}

func TestRedisCounterGroupUsesOfficialSharedIdentity(t *testing.T) {
	p := newTestPlugin(t, Config{Count: 10, TimeWindow: 60})
	p.config.Policy = "redis-cluster"
	p.config.Group = "shared"
	client := &scriptedRedisClient{result: []any{int64(1), int64(9), int64(60)}}
	p.redisLimiter = &redisCountLimiter{client: client}
	if _, _, _, err := p.incoming(httptest.NewRequest("POST", "/graphql", nil), "client", 1, 10, 60); err != nil {
		t.Fatal(err)
	}
	if got := client.keys[0]; got != "plugin-graphql-limit-countshared:client" {
		t.Fatalf("group key = %q", got)
	}
}
