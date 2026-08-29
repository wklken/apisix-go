package route

import (
	"context"
	"reflect"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
	pluginpkg "github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

func TestPlanHTTPPluginsPrecedenceAndExactSources(t *testing.T) {
	input := PlanningInput{
		Routes: []resource.Route{
			{
				ID:             "r1",
				ServiceID:      "s1",
				PluginConfigID: "pc1",
				Plugins: map[string]resource.PluginConfig{
					"proxy-rewrite": map[string]any{"host": "route.example"},
				},
			},
		},
		Services: map[string]resource.Service{
			"s1": {ID: "s1", Plugins: map[string]resource.PluginConfig{
				"proxy-rewrite": map[string]any{"host": "service.example"},
				"kafka-logger": map[string]any{
					"broker_list": map[string]any{"host": "127.0.0.1", "port": 9092},
				},
			}},
		},
		PluginConfigs: map[string]resource.PluginConfigRule{
			"pc1": {Plugins: map[string]resource.PluginConfig{
				"proxy-rewrite":    map[string]any{"host": "plugin-config.example"},
				"response-rewrite": map[string]any{"status_code": 201},
			}},
		},
		EnabledPlugins: []string{"proxy-rewrite", "response-rewrite", "kafka-logger"},
	}

	plan, err := PlanHTTPPlugins(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Routes) != 1 {
		t.Fatalf("routes = %d, want 1", len(plan.Routes))
	}
	route := plan.Routes[0]
	assertPluginPlanSource(
		t,
		route.Local,
		"proxy-rewrite",
		generation.ResourceKey{Kind: "routes", ID: "r1"},
	)
	assertPluginPlanSource(
		t,
		route.Local,
		"response-rewrite",
		generation.ResourceKey{Kind: "plugin_configs", ID: "pc1"},
	)
	assertPluginPlanSource(
		t,
		route.ServicePlans,
		"kafka-logger",
		generation.ResourceKey{Kind: "services", ID: "s1"},
	)
	assertPluginPlanSource(
		t,
		route.System,
		"request-context",
		generation.ResourceKey{Kind: "system", ID: "request-context"},
	)
	assertPluginPlanSource(
		t,
		plan.System,
		"request-context",
		generation.ResourceKey{Kind: "system", ID: "request-context"},
	)
	if got := pluginPlanNamed(route.Local, "proxy-rewrite").Config.(map[string]any)["host"]; got != "route.example" {
		t.Fatalf("proxy-rewrite host = %v, want route winner", got)
	}
}

func TestPlanHTTPPluginsUsesLegacyStablePriorityOrder(t *testing.T) {
	plan, err := PlanHTTPPlugins(context.Background(), PlanningInput{Routes: []resource.Route{
		{ID: "equal-first", Priority: 10},
		{ID: "lower", Priority: 1},
		{ID: "equal-second", Priority: 10},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := []string{plan.Routes[0].Route.ID, plan.Routes[1].Route.ID, plan.Routes[2].Route.ID}
	want := []string{"lower", "equal-first", "equal-second"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("route order = %v, want %v", got, want)
	}
}

func TestPlanHTTPPluginsInjectsEnabledLogRotateAsSystemPlugin(t *testing.T) {
	plan, err := PlanHTTPPlugins(context.Background(), PlanningInput{
		Routes:         []resource.Route{{ID: "log-rotate-route", Uri: "/logs"}},
		EnabledPlugins: []string{"log-rotate"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for scope, plans := range map[string][]PluginPlan{
		"not-found": plan.System,
		"route":     plan.Routes[0].System,
	} {
		logRotate := pluginPlanNamed(plans, "log-rotate")
		if logRotate == nil {
			t.Fatalf("%s system plans = %#v, want log-rotate", scope, plans)
		}
		if logRotate.Scope != pluginpkg.ScopeSystem ||
			logRotate.Source != (generation.ResourceKey{Kind: "system", ID: "log-rotate"}) {
			t.Fatalf("%s log-rotate plan = %#v, want system scope and source", scope, logRotate)
		}
	}
	disabled, err := PlanHTTPPlugins(context.Background(), PlanningInput{
		Routes: []resource.Route{{ID: "log-rotate-disabled-route", Uri: "/logs"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pluginPlanNamed(disabled.System, "log-rotate") != nil ||
		pluginPlanNamed(disabled.Routes[0].System, "log-rotate") != nil {
		t.Fatalf("disabled log-rotate remained in system plans: %#v / %#v", disabled.System, disabled.Routes[0].System)
	}
}

func TestPlanHTTPPluginsInjectsEnabledErrorLogLoggerAsSystemPlugin(t *testing.T) {
	plan, err := PlanHTTPPlugins(context.Background(), PlanningInput{
		Routes:         []resource.Route{{ID: "error-log-route", Uri: "/logs"}},
		EnabledPlugins: []string{"error-log-logger"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for scope, plans := range map[string][]PluginPlan{
		"not-found": plan.System,
		"route":     plan.Routes[0].System,
	} {
		errorLogLogger := pluginPlanNamed(plans, "error-log-logger")
		if errorLogLogger == nil {
			t.Fatalf("%s system plans = %#v, want error-log-logger", scope, plans)
		}
		if errorLogLogger.Scope != pluginpkg.ScopeSystem ||
			errorLogLogger.Source != (generation.ResourceKey{Kind: "system", ID: "error-log-logger"}) {
			t.Fatalf(
				"%s error-log-logger plan = %#v, want system scope and source",
				scope,
				errorLogLogger,
			)
		}
	}
}

func TestPlanHTTPPluginsIgnoresRouteLocalErrorLogLoggerConfig(t *testing.T) {
	plan, err := PlanHTTPPlugins(context.Background(), PlanningInput{
		Routes: []resource.Route{{
			ID:  "error-log-route",
			Uri: "/logs",
			Plugins: map[string]resource.PluginConfig{
				"error-log-logger": map[string]any{
					"tcp": map[string]any{"host": "127.0.0.1", "port": 1},
				},
			},
		}},
		EnabledPlugins: []string{"error-log-logger"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pluginPlanNamed(plan.Routes[0].Local, "error-log-logger") != nil {
		t.Fatalf("route-local error-log-logger remained in plans: %#v", plan.Routes[0].Local)
	}
	if pluginPlanNamed(plan.Routes[0].System, "error-log-logger") == nil {
		t.Fatalf("system error-log-logger missing from plans: %#v", plan.Routes[0].System)
	}
}

func TestPlanHTTPPluginsDisabledWinnerDoesNotRestoreLoser(t *testing.T) {
	plan, err := PlanHTTPPlugins(context.Background(), PlanningInput{
		Routes: []resource.Route{
			{ID: "r1", PluginConfigID: "pc1", Plugins: map[string]resource.PluginConfig{
				"proxy-rewrite": map[string]any{"_meta": map[string]any{"disable": true}},
			}},
		},
		PluginConfigs: map[string]resource.PluginConfigRule{
			"pc1": {Plugins: map[string]resource.PluginConfig{
				"proxy-rewrite": map[string]any{"host": "loser.example"},
			}},
		},
		EnabledPlugins: []string{"proxy-rewrite"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pluginPlanNamed(plan.Routes[0].Local, "proxy-rewrite") != nil {
		t.Fatal("disabled route winner restored lower-precedence plugin_config loser")
	}
}

func TestPlanHTTPPluginsConsumerGlobalMetadataAndInputIsolation(t *testing.T) {
	nested := map[string]any{"value": "before"}
	input := PlanningInput{
		Routes: []resource.Route{{ID: "r1", Plugins: map[string]resource.PluginConfig{
			"example-plugin": map[string]any{"nested": nested, "_meta": map[string]any{
				"priority": 99, "filter": []any{[]any{"uri", "==", "/allowed"}},
			}},
		}}},
		GlobalRules: []resource.GlobalRule{
			{ID: "g1", Plugins: map[string]resource.PluginConfig{"cors": map[string]any{}}},
			{
				ID: "g2",
				Plugins: map[string]resource.PluginConfig{
					"cors":             map[string]any{},
					"response-rewrite": map[string]any{"status_code": 202},
				},
			},
		},
		Consumers: map[string]resource.Consumer{
			"alice": {
				Username: "alice",
				GroupID:  "group-a",
				Plugins: map[string]resource.PluginConfig{
					"limit-count": map[string]any{
						"count": 20,
					}, "key-auth": map[string]any{"key": "secret"},
				},
			},
		},
		ConsumerGroups: map[string]resource.ConsumerGroup{
			"group-a": {Plugins: map[string]resource.PluginConfig{
				"limit-count": map[string]any{
					"count": 10,
				}, "proxy-rewrite": map[string]any{"host": "group.example"},
			}},
		},
		EnabledPlugins: []string{
			"example-plugin",
			"response-rewrite",
			"cors",
			"limit-count",
			"proxy-rewrite",
		},
	}
	plan, err := PlanHTTPPlugins(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if pluginPlanNamed(plan.Global, "cors") != nil {
		t.Fatal("duplicate global plugin was not removed from all rules")
	}
	assertPluginPlanSource(
		t,
		plan.Global,
		"response-rewrite",
		generation.ResourceKey{Kind: "global_rules", ID: "g2"},
	)
	consumer := plan.Consumers["alice"]
	assertPluginPlanSource(
		t,
		consumer,
		"limit-count",
		generation.ResourceKey{Kind: "consumers", ID: "alice"},
	)
	assertPluginPlanSource(
		t,
		consumer,
		"proxy-rewrite",
		generation.ResourceKey{Kind: "consumer_groups", ID: "group-a"},
	)
	if pluginPlanNamed(consumer, "key-auth") != nil {
		t.Fatal("credential-only auth plugin entered consumer execution plans")
	}

	local := pluginPlanNamed(plan.Routes[0].Local, "example-plugin")
	if _, exists := local.Config.(map[string]any)["_meta"]; exists {
		t.Fatal("planned config retained _meta")
	}
	raw := &recordingPlugin{name: "example-plugin", priority: 1, order: &[]string{}}
	binding := pluginpkg.Binding{
		Plugin: raw, Descriptor: pluginpkg.Descriptor{Factory: "example-plugin"}, Priority: 1,
		Scope: local.Scope, Provenance: local.Provenance,
	}
	applied, err := local.Apply(binding)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Priority != 99 || applied.Plugin == raw {
		t.Fatalf(
			"Apply() priority/plugin = (%d, %T), want priority 99 and metadata wrapper",
			applied.Priority,
			applied.Plugin,
		)
	}

	nested["value"] = "after"
	got := local.Config.(map[string]any)["nested"].(map[string]any)["value"]
	if !reflect.DeepEqual(got, "before") {
		t.Fatalf("planned nested value = %v after source mutation, want before", got)
	}
}

func TestPlanHTTPPluginsSkipsDisabledRoutesAndRejectsInvalidGlobalAuthority(t *testing.T) {
	var disabled resource.Route
	if err := util.Parse(map[string]any{
		"id": "disabled", "status": 0,
		"plugins": map[string]any{"example-plugin": map[string]any{}},
	}, &disabled); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanHTTPPlugins(context.Background(), PlanningInput{
		Routes: []resource.Route{
			disabled,
			{ID: "enabled", Status: 1},
		},
		EnabledPlugins: []string{"example-plugin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Routes) != 1 || plan.Routes[0].Route.ID != "enabled" || len(plan.Quarantined) != 0 {
		t.Fatalf("disabled route plan = %+v, want only enabled route and no quarantine", plan)
	}

	_, err = PlanHTTPPlugins(context.Background(), PlanningInput{
		GlobalRules: []resource.GlobalRule{{Plugins: map[string]resource.PluginConfig{
			"example-plugin": map[string]any{},
		}}},
		EnabledPlugins: []string{"example-plugin"},
	})
	if err == nil {
		t.Fatal("empty global rule authority error = nil")
	}
}

func TestPluginPlanApplyRejectsRelabeledBinding(t *testing.T) {
	plan := PluginPlan{
		Factory: "example-plugin", Scope: pluginpkg.ScopeRoute,
		Provenance: pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceRoute, ID: "r1"},
	}
	binding := pluginpkg.Binding{
		Plugin:     &recordingPlugin{name: "example-plugin", order: &[]string{}},
		Descriptor: pluginpkg.Descriptor{Factory: "example-plugin"},
		Scope:      pluginpkg.ScopeGlobal,
		Provenance: pluginpkg.ResourceProvenance{Kind: pluginpkg.ResourceGlobalRule, ID: "g1"},
	}
	if _, err := plan.Apply(binding); err == nil {
		t.Fatal("relabeled binding authority error = nil")
	}
}

func TestPlanHTTPPluginsQuarantinesRouteErrorsAndFailsGenerationWideSources(t *testing.T) {
	t.Run("route metadata is quarantined", func(t *testing.T) {
		plan, err := PlanHTTPPlugins(context.Background(), PlanningInput{
			Routes: []resource.Route{
				{
					ID: "bad",
					Plugins: map[string]resource.PluginConfig{
						"example-plugin": map[string]any{"_meta": "invalid"},
					},
				},
				{ID: "good"},
			},
			EnabledPlugins: []string{"example-plugin"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Routes) != 1 || plan.Routes[0].Route.ID != "good" ||
			len(plan.Quarantined) != 1 || plan.Quarantined[0].ID != "bad" {
			t.Fatalf("routes/quarantine = (%+v, %+v), want good/bad", plan.Routes, plan.Quarantined)
		}
	})

	t.Run("global metadata fails generation", func(t *testing.T) {
		_, err := PlanHTTPPlugins(context.Background(), PlanningInput{
			GlobalRules: []resource.GlobalRule{{ID: "g1", Plugins: map[string]resource.PluginConfig{
				"example-plugin": map[string]any{"_meta": "invalid"},
			}}},
			EnabledPlugins: []string{"example-plugin"},
		})
		if err == nil {
			t.Fatal("invalid global metadata error = nil")
		}
	})

	t.Run("missing consumer group fails generation", func(t *testing.T) {
		_, err := PlanHTTPPlugins(context.Background(), PlanningInput{
			Consumers: map[string]resource.Consumer{
				"alice": {Username: "alice", GroupID: "missing"},
			},
		})
		if err == nil {
			t.Fatal("missing consumer group error = nil")
		}
	})

	t.Run("dynamic plugin list overrides static list", func(t *testing.T) {
		plan, err := PlanHTTPPlugins(context.Background(), PlanningInput{
			Routes: []resource.Route{{ID: "r1", Plugins: map[string]resource.PluginConfig{
				"example-plugin": map[string]any{},
			}}},
			EnabledPlugins: []string{"example-plugin"},
			DynamicPlugins: []string{},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Routes) != 0 || len(plan.Quarantined) != 1 {
			t.Fatalf("dynamic disabled route plan = %+v, want quarantined", plan)
		}
	})
}

func assertPluginPlanSource(
	t *testing.T,
	plans []PluginPlan,
	factory string,
	want generation.ResourceKey,
) {
	t.Helper()
	plan := pluginPlanNamed(plans, factory)
	if plan == nil || plan.Source != want {
		t.Fatalf("plan %q source = %+v, want %+v", factory, plan, want)
	}
}

func pluginPlanNamed(plans []PluginPlan, factory string) *PluginPlan {
	for index := range plans {
		if plans[index].Factory == factory {
			return &plans[index]
		}
	}
	return nil
}
