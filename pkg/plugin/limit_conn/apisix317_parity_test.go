package limit_conn

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/limitbase"
	"github.com/wklken/apisix-go/pkg/util"
)

func TestAPISIX317SchemaAcceptsEmptyStringLimitsKeyAndRules(t *testing.T) {
	plugin := &Plugin{}
	if err := plugin.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	tests := []struct {
		name   string
		config map[string]any
	}{
		{
			name: "root string limits and key",
			config: map[string]any{
				"conn": "", "burst": "", "default_conn_delay": 0.1, "key": "",
			},
		},
		{
			name: "rule string limits",
			config: map[string]any{
				"default_conn_delay": 0.1,
				"rules": []any{map[string]any{
					"conn": "", "burst": "", "key": "$http_x_user",
				}},
			},
		},
		{
			name: "empty rules",
			config: map[string]any{
				"default_conn_delay": 0.1, "rules": []any{},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := util.Validate(test.config, plugin.GetSchema()); err != nil {
				t.Fatalf("schema rejected APISIX-valid config %#v: %v", test.config, err)
			}
		})
	}
}

func TestAPISIX317LimitConnUsesConfigAndConsumerIdentity(t *testing.T) {
	plugin := newTestPlugin(t, Config{
		Conn: 1, Burst: 0, DefaultConnDelay: 0.1, Key: "remote_addr",
	})
	if err := plugin.SetAPISIXPluginContext(base.APISIXPluginContext{
		ConfigType: "route&service", ConfigVersion: "11&17",
	}); err != nil {
		t.Fatal(err)
	}
	request := apisixctx.WithAPISIXConfigIdentitySuffix(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		"&consumer",
		"&5",
	)

	if got := plugin.applyLimitKey(request, "client-a"); got != "client-aroute&service&consumer11&17&5" {
		t.Fatalf("limit key = %q", got)
	}
}

func TestAPISIX317LimitConnConsumerBindingDoesNotDuplicateIdentity(t *testing.T) {
	plugin := newTestPlugin(t, Config{
		Conn: 1, Burst: 0, DefaultConnDelay: 0.1, Key: "remote_addr",
	})
	if err := plugin.SetAPISIXPluginContext(base.APISIXPluginContext{
		ConfigType: "route&consumer", ConfigVersion: "11&5", ConsumerOverride: true,
	}); err != nil {
		t.Fatal(err)
	}
	request := apisixctx.WithAPISIXConfigIdentitySuffix(
		httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil),
		"&consumer",
		"&5",
	)

	if got := plugin.applyLimitKey(request, "client-a"); got != "client-aroute&consumer11&5" {
		t.Fatalf("consumer limit key = %q", got)
	}
}

func TestAPISIX317LimitConnSharesLocalReservationsAcrossInstances(t *testing.T) {
	state := limitbase.NewState()
	newPlugin := func() *Plugin {
		plugin := &Plugin{config: Config{
			Conn: 1, Burst: 0, DefaultConnDelay: 0.1, Key: "remote_addr",
		}}
		plugin.SetRateLimitState(state)
		if err := plugin.Init(); err != nil {
			t.Fatal(err)
		}
		if err := plugin.PostInit(); err != nil {
			t.Fatal(err)
		}
		if err := plugin.SetAPISIXPluginContext(base.APISIXPluginContext{
			ConfigType: "route", ConfigVersion: "11",
		}); err != nil {
			t.Fatal(err)
		}
		return plugin
	}
	first := newPlugin()
	second := newPlugin()
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil)

	_, allowed, release, err := first.increase(request, "client-a", 1, 0)
	if err != nil || !allowed {
		t.Fatalf("first admission = allowed %t, error %v", allowed, err)
	}
	if _, allowed, _, err := second.increase(request, "client-a", 1, 0); err != nil || allowed {
		t.Fatalf("second admission = allowed %t, error %v", allowed, err)
	}
	release(nil)
	if _, allowed, release, err = second.increase(request, "client-a", 1, 0); err != nil || !allowed {
		t.Fatalf("post-release admission = allowed %t, error %v", allowed, err)
	}
	release(nil)
}

func TestAPISIX317LimitConnRuleKeyHasNoLocalRuleIndex(t *testing.T) {
	plugin := newTestPlugin(t, Config{
		DefaultConnDelay: 0.1,
		Rules:            []Rule{{Conn: 1, Burst: 0, Key: "${http_x_tenant}"}},
	})
	request := httptest.NewRequest(http.MethodGet, "http://gateway.test/", nil)
	request.Header.Set("X-Tenant", "tenant-a")

	key, ok := plugin.resolveRuleKey(request, 7, plugin.config.Rules[0])
	if !ok || key != "tenant-a" {
		t.Fatalf("rule key = %q, resolved %t", key, ok)
	}
}

func TestAPISIX317RedisLimitConnWireContract(t *testing.T) {
	upperIncoming := strings.ToUpper(redisLimitConnIncomingScript)
	if strings.Contains(upperIncoming, "ZADD', KEYS[1], 'NX") ||
		strings.Contains(upperIncoming, "ZADD\", KEYS[1], \"NX") {
		t.Fatal("incoming script adds an APISIX-incompatible NX reservation policy")
	}
	if !strings.Contains(upperIncoming, "EXPIRE") || strings.Contains(upperIncoming, "PEXPIRE") {
		t.Fatal("incoming script must use APISIX key_ttl seconds")
	}
	if strings.Contains(strings.ToUpper(redisLimitConnLeavingScript), "DEL") {
		t.Fatal("release script must not delete the Redis key")
	}

	now := time.Unix(1_700_000_000, 0)
	client := &scriptedConnRedisClient{result: []any{int64(1), int64(2)}}
	limiter := &redisConnLimiter{
		client: client, unitDelay: 0.5, keyTTL: 90 * time.Second,
		now: func() time.Time { return now }, newMemberID: func() (string, error) { return "request-id", nil },
	}
	delay, _, allowed, err := limiter.incoming("client-a", 1, 2)
	if err != nil || !allowed || delay != 500*time.Millisecond {
		t.Fatalf("incoming = delay %s, allowed %t, error %v", delay, allowed, err)
	}
	if len(client.keys) != 1 || client.keys[0] != "limit_conn:client-a" {
		t.Fatalf("Redis keys = %#v", client.keys)
	}
	wantArgs := []any{3, int64(90), now.Unix(), "request-id"}
	if len(client.args) != len(wantArgs) {
		t.Fatalf("Redis args = %#v, want %#v", client.args, wantArgs)
	}
	for index := range wantArgs {
		if client.args[index] != wantArgs[index] {
			t.Fatalf("Redis arg %d = %#v, want %#v", index, client.args[index], wantArgs[index])
		}
	}
}
