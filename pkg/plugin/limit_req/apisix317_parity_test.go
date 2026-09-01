package limit_req

import (
	"hash/crc32"
	"strconv"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/limitbase"
	"github.com/wklken/apisix-go/pkg/util"
)

func TestAPISIX317SchemaAcceptsEmptyKey(t *testing.T) {
	plugin := &Plugin{}
	if err := plugin.Init(); err != nil {
		t.Fatal(err)
	}
	if err := util.Validate(map[string]any{
		"rate": 1, "burst": 0, "key": "",
	}, plugin.GetSchema()); err != nil {
		t.Fatalf("schema rejected APISIX-valid empty key: %v", err)
	}
}

func TestAPISIX317LimitReqUsesParentAndEffectiveConfigVersion(t *testing.T) {
	context := base.APISIXPluginContext{
		SourceResourceKey: "/apisix/routes/route-1",
		SourceConfig: map[string]any{
			"rate": 1.0, "burst": 0.0, "key": "remote_addr",
		},
	}
	got, err := buildLimitReqKey(context, context.SourceConfig, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	canonical := `{"_meta":[],"allow_degradation":false,"burst":0,"key":"remote_addr","key_type":"var","nodelay":false,"policy":"local","rate":1,"rejected_code":503}`
	want := "/apisix/routes/route-1:" +
		strconv.FormatUint(uint64(crc32.ChecksumIEEE([]byte(canonical))), 10) +
		":192.0.2.1"
	if got != want {
		t.Fatalf("limit-req key = %q, want %q", got, want)
	}
}

func TestAPISIX317WorkflowActionsUseDistinctConfigVersions(t *testing.T) {
	context := base.APISIXPluginContext{
		SourceResourceKey: "/apisix/routes/route-1",
		SourceConfig: map[string]any{
			"rate": 1.0, "burst": 0.0, "key": "remote_addr",
		},
	}
	first := Plugin{apisixContext: context.WithWorkflowVID(1)}
	second := Plugin{apisixContext: context.WithWorkflowVID(2)}
	firstKey := first.scopedKey("192.0.2.1")
	secondKey := second.scopedKey("192.0.2.1")
	if firstKey == secondKey {
		t.Fatalf("workflow actions share limit-req key %q", firstKey)
	}
}

func TestAPISIX317LocalStateSurvivesPluginReplacement(t *testing.T) {
	state := limitbase.NewStateWithClock(nil)
	context := base.APISIXPluginContext{
		SourceResourceKey: "/routes/route-1",
		SourceConfig: map[string]any{
			"rate": 1.0, "burst": 0.0, "key": "remote_addr", "nodelay": true,
		},
	}
	newPlugin := func() *Plugin {
		plugin := newTestPlugin(t, Config{Rate: 1, Burst: 0, Key: "remote_addr", Nodelay: new(true)})
		plugin.SetRateLimitState(state)
		if err := plugin.SetAPISIXPluginContext(context); err != nil {
			t.Fatal(err)
		}
		return plugin
	}
	first := newPlugin()
	second := newPlugin()
	key := first.scopedKey("192.0.2.1")
	if _, allowed, err := first.incomingWithConsumer(key, ""); err != nil || !allowed {
		t.Fatalf("first admission = allowed %t, error %v", allowed, err)
	}
	if _, allowed, err := second.incomingWithConsumer(key, ""); err != nil || allowed {
		t.Fatalf("replacement admission = allowed %t, error %v; want shared rejection", allowed, err)
	}
}

func TestAPISIX317RedisStateKeyPrefix(t *testing.T) {
	if got := redisStateKey("/routes/route-1:123:client"); got != "limit_req:/routes/route-1:123:client" {
		t.Fatalf("Redis state key = %q", got)
	}
}

func TestAPISIX317RedisStateTTL(t *testing.T) {
	tests := []struct {
		name  string
		rate  float64
		burst float64
		want  time.Duration
	}{
		{name: "no burst", rate: 1, burst: 0, want: time.Second},
		{name: "fractional rate", rate: 0.1, burst: 0.1, want: 2 * time.Second},
		{name: "round up", rate: 2, burst: 3, want: 3 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := redisLimitReqTTL(test.rate, test.burst); got != test.want {
				t.Fatalf("Redis state TTL = %s, want %s", got, test.want)
			}
		})
	}
}
