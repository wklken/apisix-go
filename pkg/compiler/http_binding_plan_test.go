package compiler

import (
	"context"
	"errors"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/resource"
	routepkg "github.com/wklken/apisix-go/pkg/route"
	"github.com/wklken/apisix-go/pkg/runtime"
)

func TestEffectiveHTTPBindingSpecSelectsExactAttemptOccurrence(t *testing.T) {
	prepared, fixture := newEffectiveBindingMaterializerFixture(t, []string{"request-id"}, nil)
	plan := routepkg.PluginPlan{
		Factory: "request-id", Config: map[string]any{}, Scope: plugin.ScopeRoute,
		Provenance: plugin.ResourceProvenance{Kind: plugin.ResourcePluginConfig, ID: "plugin-config-1"},
		Source:     generation.ResourceKey{Kind: "plugin_configs", ID: "plugin-config-1"},
	}
	spec, err := prepared.effectiveHTTPBindingSpec(
		generation.ResourceKey{Kind: "routes", ID: "route-1"},
		plan,
		effectiveBindingResourceContext{
			kind: effectiveBindingContextHTTP, route: resource.Route{ID: "route-1"},
		},
		effectiveBindingRuntimeContext{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if spec.source.kind != effectiveBindingPluginConfig ||
		spec.source.occurrence != fixture.occurrences["request-id"] ||
		spec.source.resource != plan.Source || spec.factory != plan.Factory {
		t.Fatalf("effective HTTP binding spec = %#v", spec)
	}

	plan.Source.ID = "different"
	if _, err := prepared.effectiveHTTPBindingSpec(
		generation.ResourceKey{Kind: "routes", ID: "route-1"}, plan,
		effectiveBindingResourceContext{kind: effectiveBindingContextHTTP, route: resource.Route{ID: "route-1"}},
		effectiveBindingRuntimeContext{},
	); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing exact occurrence error = %v, want ErrInvalidInput", err)
	}
}

func TestMaterializeHTTPPluginPlansAppliesPlanMetadataInTransaction(t *testing.T) {
	prepared, _ := newEffectiveBindingMaterializerFixture(t, []string{"request-id"}, nil)
	priority := 123
	bindings, err := prepared.materializeHTTPPluginPlans(
		context.Background(),
		generation.ResourceKey{Kind: "routes", ID: "route-1"},
		[]routepkg.PluginPlan{{
			Factory: "request-id", Config: map[string]any{}, Scope: plugin.ScopeRoute,
			Provenance: plugin.ResourceProvenance{Kind: plugin.ResourcePluginConfig, ID: "plugin-config-1"},
			Source:     generation.ResourceKey{Kind: "plugin_configs", ID: "plugin-config-1"},
			Priority:   &priority,
		}},
		effectiveBindingResourceContext{
			kind: effectiveBindingContextHTTP, route: resource.Route{ID: "route-1"},
		},
		effectiveBindingRuntimeContext{},
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].Priority != priority ||
		bindings[0].Provenance != (plugin.ResourceProvenance{
			Kind: plugin.ResourcePluginConfig, ID: "plugin-config-1",
		}) {
		t.Fatalf("materialized HTTP bindings = %#v", bindings)
	}
}

func TestEffectiveHTTPBindingSpecSupportsSystemAndPreparedConsumer(t *testing.T) {
	consumerKey := generation.ResourceKey{Kind: "consumers", ID: "consumer-1"}
	consumers, err := runtime.NewConsumerBindings([]runtime.ConsumerRecord{{
		ID: "consumer-1",
		Consumer: resource.Consumer{Username: "consumer-1", Plugins: map[string]resource.PluginConfig{
			"request-id": map[string]any{"header_name": "X-Consumer-ID"},
		}},
	}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	prepared, _ := newEffectiveBindingMaterializerFixtureWithOccurrenceSpecs(
		t,
		[]factoryOccurrenceSpec{{
			domain: generation.DomainHTTP, resource: consumerKey,
			source: capability.SecretConsumerConfig, factory: "request-id",
		}},
		consumers,
	)
	system, err := prepared.effectiveHTTPBindingSpec(
		generation.ResourceKey{Kind: "system", ID: "request-context"},
		routepkg.PluginPlan{
			Factory: "request-context", Config: map[string]any{}, Scope: plugin.ScopeSystem,
			Provenance: plugin.ResourceProvenance{Kind: plugin.ResourceSystem, ID: "request-context"},
			Source:     generation.ResourceKey{Kind: "system", ID: "request-context"},
		},
		effectiveBindingResourceContext{},
		effectiveBindingRuntimeContext{},
	)
	if err != nil || system.source.kind != effectiveBindingSystem {
		t.Fatalf("system spec = (%#v, %v)", system, err)
	}

	consumer, err := prepared.effectiveHTTPBindingSpec(
		generation.ResourceKey{Kind: "routes", ID: "route-1"},
		routepkg.PluginPlan{
			Factory: "request-id", Config: map[string]any{"header_name": "X-Consumer-ID"},
			Scope:      plugin.ScopeConsumer,
			Provenance: plugin.ResourceProvenance{Kind: plugin.ResourceConsumer, ID: "consumer-1"},
			Source:     consumerKey,
		},
		effectiveBindingResourceContext{
			kind: effectiveBindingContextHTTP, route: resource.Route{ID: "route-1"},
		},
		effectiveBindingRuntimeContext{},
	)
	if err != nil || consumer.source.kind != effectiveBindingPreparedConsumer ||
		consumer.source.occurrence != prepared.attempt.Occurrences(capability.SecretConsumerConfig)[0] {
		t.Fatalf("consumer spec = (%#v, %v)", consumer, err)
	}
}

func TestEffectiveHTTPBindingSpecValidatesPreparedConsumerAfterMetadataRemoval(t *testing.T) {
	consumers, err := runtime.NewConsumerBindings([]runtime.ConsumerRecord{{
		ID: "consumer-meta",
		Consumer: resource.Consumer{Username: "consumer-meta", Plugins: map[string]resource.PluginConfig{
			"request-id": map[string]any{
				"header_name": "X-Consumer-ID",
				"_meta":       map[string]any{"priority": 99},
			},
		}},
	}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	consumerKey := generation.ResourceKey{Kind: "consumers", ID: "consumer-meta"}
	prepared, _ := newEffectiveBindingMaterializerFixtureWithOccurrenceSpecs(
		t,
		[]factoryOccurrenceSpec{{
			domain: generation.DomainHTTP, resource: consumerKey,
			source: capability.SecretConsumerConfig, factory: "request-id",
		}},
		consumers,
	)
	plan := routepkg.PluginPlan{
		Factory: "request-id", Config: map[string]any{"header_name": "X-Consumer-ID"},
		Scope:      plugin.ScopeConsumer,
		Provenance: plugin.ResourceProvenance{Kind: plugin.ResourceConsumer, ID: "consumer-meta"},
		Source:     consumerKey,
	}
	spec, err := prepared.effectiveHTTPBindingSpec(
		generation.ResourceKey{Kind: "routes", ID: "route-1"}, plan,
		effectiveBindingResourceContext{
			kind: effectiveBindingContextHTTP, route: resource.Route{ID: "route-1"},
		},
		effectiveBindingRuntimeContext{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepared.validateEffectiveBindingSpec(spec); err != nil {
		t.Fatalf("metadata-stripped consumer authority error = %v", err)
	}
}
