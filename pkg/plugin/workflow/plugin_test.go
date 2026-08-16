package workflow

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

type recordingWorkflowStopper struct {
	name  string
	order *[]string
}

func (s recordingWorkflowStopper) Stop() {
	*s.order = append(*s.order, s.name)
}

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := base.MaterializePluginSecrets(p); err != nil {
		t.Fatalf("MaterializePluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestParseOfficialReturnActionArray(t *testing.T) {
	var cfg Config
	err := util.Parse(map[string]any{
		"rules": []any{
			map[string]any{
				"case": []any{[]any{"uri", "==", "/anything/rejected"}},
				"actions": []any{
					[]any{"return", map[string]any{"code": 403}},
				},
			},
		},
	}, &cfg)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	if len(cfg.Rules) != 1 || len(cfg.Rules[0].Actions) != 1 {
		t.Fatalf("rules = %#v, want one rule with one action", cfg.Rules)
	}
	action := cfg.Rules[0].Actions[0]
	if action.Name != "return" {
		t.Fatalf("action name = %q, want return", action.Name)
	}
	if action.Return.Code != http.StatusForbidden {
		t.Fatalf("return code = %d, want 403", action.Return.Code)
	}
}

func TestWorkflowRejectsDisabledNestedPluginBeforeConstruction(t *testing.T) {
	for _, actionName := range []string{"limit-req", "limit-conn", "limit-count"} {
		t.Run(actionName, func(t *testing.T) {
			p := &Plugin{config: Config{Rules: []Rule{{Actions: []Action{{
				Name:   actionName,
				Config: map[string]any{"key": "$ENV://WORKFLOW_DISABLED_CHILD_SECRET"},
			}}}}}}
			if err := p.Init(); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			p.SetPluginEnabledChecker(func(name string) bool { return name != actionName })

			err := p.ValidatePreMaterialization()
			if err == nil || !strings.Contains(err.Error(), actionName) || !strings.Contains(err.Error(), "disabled") {
				t.Fatalf(
					"ValidatePreMaterialization() error = %v, want disabled action rejection before construction",
					err,
				)
			}
			if p.children != nil || p.childStoppers != nil {
				t.Fatalf("disabled action retained child state: children=%v stoppers=%v", p.children, p.childStoppers)
			}
			if got := p.config.Rules[0].Actions[0].Config["key"]; got != "$ENV://WORKFLOW_DISABLED_CHILD_SECRET" {
				t.Fatalf("disabled action key = %v, want untouched secret reference", got)
			}
		})
	}

	var enabled Config
	if err := util.Parse(map[string]any{
		"rules": []any{
			map[string]any{"actions": []any{
				[]any{
					"limit-req",
					map[string]any{
						"rate":          1,
						"burst":         0,
						"key":           "remote_addr",
						"rejected_code": http.StatusTooManyRequests,
						"nodelay":       true,
					},
				},
				[]any{
					"limit-conn",
					map[string]any{
						"conn":               1,
						"burst":              0,
						"default_conn_delay": 0.1,
						"key":                "remote_addr",
						"rejected_code":      http.StatusTooManyRequests,
					},
				},
				[]any{
					"limit-count",
					map[string]any{
						"count":         1,
						"time_window":   60,
						"key":           "remote_addr",
						"rejected_code": http.StatusTooManyRequests,
					},
				},
			}},
		},
	}, &enabled); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	withEnabled := &Plugin{config: enabled}
	if err := withEnabled.Init(); err != nil {
		t.Fatalf("enabled Init() error = %v", err)
	}
	withEnabled.SetPluginEnabledChecker(func(string) bool { return true })
	if err := base.MaterializePluginSecrets(withEnabled); err != nil {
		t.Fatalf("enabled MaterializePluginSecrets() error = %v", err)
	}
	if err := withEnabled.PostInit(); err != nil {
		t.Fatalf("enabled PostInit() error = %v", err)
	}

	returns := &Plugin{config: Config{Rules: []Rule{{Actions: []Action{{
		Name:   "return",
		Return: ReturnAction{Code: http.StatusForbidden},
	}}}}}}
	if err := returns.Init(); err != nil {
		t.Fatalf("return Init() error = %v", err)
	}
	returns.SetPluginEnabledChecker(func(string) bool { return false })
	if err := base.MaterializePluginSecrets(returns); err != nil {
		t.Fatalf("return MaterializePluginSecrets() error = %v", err)
	}
	if err := returns.PostInit(); err != nil {
		t.Fatalf("return PostInit() error = %v, want return action independent of plugin membership", err)
	}
}

func TestHandlerReturnsConfiguredStatusForMatchingCase(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				Case: []any{[]any{"uri", "==", "/anything/rejected"}},
				Actions: []Action{
					{Name: "return", Return: ReturnAction{Code: http.StatusForbidden}},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/anything/rejected", nil)
	rr := httptest.NewRecorder()
	nextCalled := false

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if nextCalled {
		t.Fatal("next handler was called, want workflow return to stop request")
	}
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "rejected by workflow") {
		t.Fatalf("body = %q, want workflow rejection message", rr.Body.String())
	}
}

func TestHandlerFallsThroughWhenNoCaseMatches(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				Case: []any{[]any{"arg_name", "==", "blocked"}},
				Actions: []Action{
					{Name: "return", Return: ReturnAction{Code: http.StatusForbidden}},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/anything?name=allowed", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Next-Called", "yes")
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
	if rr.Header().Get("X-Next-Called") != "yes" {
		t.Fatal("next handler was not called")
	}
}

func TestHandlerUsesFirstMatchingRule(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				Case: []any{[]any{"arg_name", "==", "blocked"}},
				Actions: []Action{
					{Name: "return", Return: ReturnAction{Code: http.StatusForbidden}},
				},
			},
			{
				Actions: []Action{
					{Name: "return", Return: ReturnAction{Code: http.StatusTeapot}},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/anything?name=blocked", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want first matching rule status 403", rr.Code)
	}
}

func TestHandlerSupportsRestyExpressionOperators(t *testing.T) {
	p := newTestPlugin(t, Config{
		Rules: []Rule{
			{
				Case: []any{
					"AND",
					[]any{"method", "in", []any{"GET", "HEAD"}},
					[]any{"http_x_stage", "~*", "^prod$"},
					[]any{"remote_addr", "ipmatch", []any{"192.0.2.0/24"}},
				},
				Actions: []Action{
					{Name: "return", Return: ReturnAction{Code: http.StatusForbidden}},
				},
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("X-Stage", "PrOd")
	req.RemoteAddr = "192.0.2.40:1234"
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

func TestPostInitRejectsUnsupportedAction(t *testing.T) {
	p := &Plugin{config: Config{
		Rules: []Rule{
			{Actions: []Action{{Name: "unsupported-action"}}},
		},
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := base.MaterializePluginSecrets(p); err != nil {
		t.Fatalf("MaterializePluginSecrets() error = %v", err)
	}

	err := p.PostInit()
	if err == nil || !strings.Contains(err.Error(), "unsupported workflow action") {
		t.Fatalf("PostInit() error = %v, want unsupported workflow action error", err)
	}
}

func TestPostInitFailureRollsBackChildrenInReverseOrder(t *testing.T) {
	var order []string
	p := &Plugin{
		config: Config{Rules: []Rule{{Actions: []Action{{Name: "unsupported-action"}}}}},
		childStoppers: []workflowChildStopper{
			recordingWorkflowStopper{name: "first", order: &order},
			recordingWorkflowStopper{name: "second", order: &order},
		},
	}

	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want later initialization failure")
	}
	if got := strings.Join(order, ","); got != "second,first" {
		t.Fatalf("rollback order = %q, want second,first", got)
	}
	if p.childStoppers != nil || p.children != nil {
		t.Fatalf("rollback retained child state: stoppers=%v children=%v", p.childStoppers, p.children)
	}
}

func TestMaterializeFailureRollsBackEarlierChildOwner(t *testing.T) {
	p := &Plugin{config: Config{Rules: []Rule{{Actions: []Action{
		{
			Name: "limit-count",
			Config: map[string]any{
				"count":       1,
				"time_window": 60,
				"key":         "remote_addr",
			},
		},
		{
			Name: "limit-req",
			Config: map[string]any{
				"rate":  1,
				"burst": 0,
				"key":   "$ENV://WORKFLOW_UNOWNED_KEY",
			},
		},
	}}}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeSecrets(); err == nil {
		t.Fatal("MaterializeSecrets() error = nil, want second child materialization failure")
	}
	if p.childStoppers != nil || p.children != nil {
		t.Fatalf("materialization rollback retained child state: stoppers=%v children=%v", p.childStoppers, p.children)
	}
}

func TestStopRetiresChildrenInReverseOrderAndIsIdempotent(t *testing.T) {
	var order []string
	p := &Plugin{
		children: map[actionPosition]workflowChild{},
		childStoppers: []workflowChildStopper{
			recordingWorkflowStopper{name: "first", order: &order},
			recordingWorkflowStopper{name: "second", order: &order},
		},
	}

	p.Stop()
	p.Stop()
	if got := strings.Join(order, ","); got != "second,first" {
		t.Fatalf("Stop order = %q, want second,first exactly once", got)
	}
	if p.childStoppers != nil || p.children != nil {
		t.Fatalf("Stop retained child state: stoppers=%v children=%v", p.childStoppers, p.children)
	}
}

func TestPostInitRejectsInvalidReturnCode(t *testing.T) {
	p := &Plugin{config: Config{
		Rules: []Rule{
			{Actions: []Action{{Name: "return", Return: ReturnAction{Code: http.StatusContinue - 1}}}},
		},
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := base.MaterializePluginSecrets(p); err != nil {
		t.Fatalf("MaterializePluginSecrets() error = %v", err)
	}

	err := p.PostInit()
	if err == nil || !strings.Contains(err.Error(), "return action code") {
		t.Fatalf("PostInit() error = %v, want return action code error", err)
	}
}

func TestValidatePreMaterializationRejectsInvalidLimitCountActionBeforeSecretResolution(t *testing.T) {
	p := &Plugin{config: Config{
		Rules: []Rule{
			{Actions: []Action{{
				Name: "limit-count",
				Config: map[string]any{
					"count": 2,
					"key":   "$ENV://WORKFLOW_INVALID_LIMIT_COUNT_KEY",
				},
			}}},
		},
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	err := p.ValidatePreMaterialization()
	if err == nil || !strings.Contains(err.Error(), "time_window") {
		t.Fatalf("ValidatePreMaterialization() error = %v, want missing time_window before secret resolution", err)
	}
	if p.children != nil || p.childStoppers != nil {
		t.Fatalf("invalid action retained child state: children=%v stoppers=%v", p.children, p.childStoppers)
	}
	action := p.config.Rules[0].Actions[0]
	if action.limitReq != nil || action.limitConn != nil || action.limitCount != nil {
		t.Fatalf("invalid action retained runtime child: %#v", action)
	}
}

func TestPostInitRejectsLimitCountGroup(t *testing.T) {
	p := &Plugin{config: Config{
		Rules: []Rule{
			{Actions: []Action{{
				Name: "limit-count",
				Config: map[string]any{
					"count":       2,
					"time_window": 60,
					"group":       "services_1",
				},
			}}},
		},
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := base.MaterializePluginSecrets(p); err != nil {
		t.Fatalf("MaterializePluginSecrets() error = %v", err)
	}

	err := p.PostInit()
	if err == nil || !strings.Contains(err.Error(), "group is not supported") {
		t.Fatalf("PostInit() error = %v, want unsupported group validation error", err)
	}
}

func TestHandlerRunsLimitCountAction(t *testing.T) {
	var cfg Config
	err := util.Parse(map[string]any{
		"rules": []any{
			map[string]any{
				"actions": []any{
					[]any{
						"limit-count",
						map[string]any{
							"count":         1,
							"time_window":   60,
							"key":           "remote_addr",
							"rejected_code": http.StatusTooManyRequests,
						},
					},
				},
			},
		},
	}, &cfg)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	p := newTestPlugin(t, cfg)

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	first := httptest.NewRequest(http.MethodGet, "/", nil)
	first.RemoteAddr = "192.0.2.1:1234"
	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, first)
	if firstRecorder.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want %d", firstRecorder.Code, http.StatusNoContent)
	}

	second := httptest.NewRequest(http.MethodGet, "/", nil)
	second.RemoteAddr = "192.0.2.1:1234"
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, second)
	if secondRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want %d", secondRecorder.Code, http.StatusTooManyRequests)
	}
}

func TestMaterializeSecretsOwnsNestedLimitCountReferenceBeforePostInit(t *testing.T) {
	t.Setenv("WORKFLOW_LIMIT_COUNT_KEY", "remote_addr")
	var cfg Config
	if err := util.Parse(map[string]any{
		"rules": []any{map[string]any{
			"actions": []any{[]any{
				"limit-count",
				map[string]any{
					"count":         1,
					"time_window":   60,
					"key":           "$ENV://WORKFLOW_LIMIT_COUNT_KEY",
					"rejected_code": http.StatusTooManyRequests,
				},
			}},
		}},
	}, &cfg); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := base.MaterializePluginSecrets(p); err != nil {
		t.Fatalf("MaterializePluginSecrets() error = %v", err)
	}
	key, _ := p.config.Rules[0].Actions[0].Config["key"].(string)
	if !strings.Contains(key, "$ENV://WORKFLOW_LIMIT_COUNT_KEY#sha256:") || strings.Contains(key, "remote_addr") {
		t.Fatalf("nested limit-count key = %q, want safe environment descriptor", key)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := func() int {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "192.0.2.1:1234"
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder.Code
	}
	if got := request(); got != http.StatusNoContent {
		t.Fatalf("first status = %d, want %d", got, http.StatusNoContent)
	}
	if got := request(); got != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want nested materialized key quota rejection", got)
	}
	p.Stop()
	if p.config.Rules[0].Actions[0].limitCount != nil {
		t.Fatal("Stop() retained nested limit-count runtime child")
	}
}

func TestNestedLimitCountValidatesRedisClusterReferenceBeforeDescriptorRewrite(t *testing.T) {
	t.Setenv("WORKFLOW_REDIS_CLUSTER_NODE", "127.0.0.1:6379")
	var cfg Config
	if err := util.Parse(
		map[string]any{
			"rules": []any{map[string]any{
				"actions": []any{[]any{
					"limit-count",
					map[string]any{
						"count":               1,
						"time_window":         60,
						"key":                 "remote_addr",
						"policy":              "redis-cluster",
						"redis_cluster_nodes": []any{"$ENV://WORKFLOW_REDIS_CLUSTER_NODE"},
						"redis_cluster_name":  "workflow-cluster",
					},
				}},
			}},
		},
		&cfg,
	); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := base.MaterializePluginSecrets(p); err != nil {
		t.Fatalf("MaterializePluginSecrets() error = %v", err)
	}
	nodes, ok := p.config.Rules[0].Actions[0].Config["redis_cluster_nodes"].([]any)
	if !ok || len(nodes) != 1 {
		t.Fatalf("redis_cluster_nodes = %#v, want one safe descriptor", nodes)
	}
	descriptor, _ := nodes[0].(string)
	if !strings.Contains(descriptor, "$ENV://WORKFLOW_REDIS_CLUSTER_NODE#sha256:") ||
		strings.Contains(descriptor, "127.0.0.1:6379") {
		t.Fatalf("redis cluster descriptor = %q, want safe environment descriptor", descriptor)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v, want original valid node reference accepted", err)
	}
	p.Stop()
}

func TestConsumerWorkflowLimitCountOverridesRouteLimitCount(t *testing.T) {
	var cfg Config
	err := util.Parse(map[string]any{
		"rules": []any{
			map[string]any{
				"actions": []any{
					[]any{
						"limit-count",
						map[string]any{
							"count":         5,
							"time_window":   60,
							"key":           "remote_addr",
							"rejected_code": http.StatusTooManyRequests,
						},
					},
				},
			},
		},
	}, &cfg)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	p := newTestPlugin(t, cfg)

	nextCalled := false
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextCalled = true
		if !apisixctx.ConsumerPluginOverrides(r, "limit-count") {
			t.Fatal("consumer workflow action did not override route limit-count")
		}
		if !apisixctx.ConsumerPluginOverrides(r, "consumer-restriction") {
			t.Fatal("consumer workflow action discarded another consumer plugin override")
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.20:1234"
	req = apisixctx.WithApisixVars(req, nil)
	apisixctx.AttachConsumer(req, resource.Consumer{
		Username: "jack",
		Plugins: map[string]resource.PluginConfig{
			"consumer-restriction": map[string]any{},
			"workflow":             map[string]any{},
		},
	})
	req = apisixctx.WithConsumerPluginOverrides(req, map[string]struct{}{
		"consumer-restriction": {},
		"workflow":             {},
	})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	if !nextCalled {
		t.Fatal("next handler was not called")
	}
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
}

func TestHandlerRunsLimitReqAction(t *testing.T) {
	var cfg Config
	err := util.Parse(map[string]any{
		"rules": []any{
			map[string]any{
				"actions": []any{
					[]any{
						"limit-req",
						map[string]any{
							"rate":          1,
							"burst":         0,
							"key":           "remote_addr",
							"rejected_code": http.StatusTooManyRequests,
							"nodelay":       true,
						},
					},
				},
			},
		},
	}, &cfg)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	p := newTestPlugin(t, cfg)

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	first := httptest.NewRequest(http.MethodGet, "/", nil)
	first.RemoteAddr = "192.0.2.10:1234"
	firstRecorder := httptest.NewRecorder()
	handler.ServeHTTP(firstRecorder, first)
	if firstRecorder.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want %d", firstRecorder.Code, http.StatusNoContent)
	}

	second := httptest.NewRequest(http.MethodGet, "/", nil)
	second.RemoteAddr = "192.0.2.10:5678"
	secondRecorder := httptest.NewRecorder()
	handler.ServeHTTP(secondRecorder, second)
	if secondRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want %d", secondRecorder.Code, http.StatusTooManyRequests)
	}
}

func TestHandlerRunsLimitConnAction(t *testing.T) {
	var cfg Config
	err := util.Parse(map[string]any{
		"rules": []any{
			map[string]any{
				"actions": []any{
					[]any{
						"limit-conn",
						map[string]any{
							"conn":               1,
							"burst":              0,
							"default_conn_delay": 0.1,
							"key":                "remote_addr",
							"rejected_code":      http.StatusTooManyRequests,
						},
					},
				},
			},
		},
	}, &cfg)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	p := newTestPlugin(t, cfg)

	block := make(chan struct{})
	started := make(chan struct{})
	var startedOnce sync.Once
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() {
			close(started)
		})
		<-block
		w.WriteHeader(http.StatusNoContent)
	}))

	first := httptest.NewRequest(http.MethodGet, "/", nil)
	first.RemoteAddr = "192.0.2.1:1234"
	firstRecorder := httptest.NewRecorder()
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		handler.ServeHTTP(firstRecorder, first)
	}()
	<-started

	second := httptest.NewRequest(http.MethodGet, "/", nil)
	second.RemoteAddr = "192.0.2.1:5678"
	secondRecorder := httptest.NewRecorder()
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		handler.ServeHTTP(secondRecorder, second)
	}()
	select {
	case <-secondDone:
	case <-time.After(200 * time.Millisecond):
		close(block)
		<-firstDone
		<-secondDone
		t.Fatal(
			"second request reached downstream, want workflow limit-conn action to reject while first request is active",
		)
	}
	if secondRecorder.Code != http.StatusTooManyRequests {
		close(block)
		<-firstDone
		t.Fatalf("second status = %d, want %d", secondRecorder.Code, http.StatusTooManyRequests)
	}

	close(block)
	<-firstDone
	if firstRecorder.Code != http.StatusNoContent {
		t.Fatalf("first status = %d, want %d", firstRecorder.Code, http.StatusNoContent)
	}
}

func TestUnownedSecretReferenceRejectsNestedWorkflowPlugin(t *testing.T) {
	p := &Plugin{config: Config{Rules: []Rule{{Actions: []Action{{
		Name: "limit-req",
		Config: map[string]any{
			"rate":  1,
			"burst": 0,
			"key":   "$ENV://WORKFLOW_KEY",
		},
	}}}}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	err := base.MaterializePluginSecrets(p)
	if err == nil || err.Error() != "materialize plugin secrets: credential unavailable" {
		t.Fatalf("MaterializePluginSecrets() error = %v, want redacted nested secret rejection", err)
	}
}
