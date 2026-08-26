package compiler

import (
	"context"
	"testing"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
)

func TestPlanHTTPPreparationUsesPublishedCandidateAndPreparedConsumers(t *testing.T) {
	snapshot, err := generation.NewSnapshot(31, []generation.Resource{
		resourceValue(
			"routes",
			"r1",
			`{"id":"r1","uri":"/v1","service_id":"s1","plugin_config_id":"pc1"}`,
		),
		resourceValue("services", "s1", `{"id":"s1"}`),
		resourceValue("plugin_configs", "pc1", `{"id":"pc1","plugins":{"request-id":{}}}`),
		resourceValue(
			"consumers",
			"consumer-1",
			`{"username":"consumer-1","plugins":{"key-auth":{"key":"consumer-key"}}}`,
		),
		resourceValue("plugins", "plugins", `[{"name":"request-id"}]`),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate := compileDomain(
		t,
		generation.DomainHTTP,
		snapshot,
		generation.PublishedGeneration{},
		false,
	)
	consumers, err := runtime.NewConsumerBindings([]runtime.ConsumerRecord{{
		ID: "consumer-1", Consumer: resource.Consumer{
			Username: "consumer-1",
			Plugins: map[string]resource.PluginConfig{
				"key-auth": map[string]any{"key": "consumer-key"},
			},
		},
	}}, nil, []runtime.ConsumerCredentialBinding{{
		Plugin: "key-auth", Key: "consumer-key", ConsumerID: "consumer-1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	prepared, _ := newEffectiveBindingMaterializerFixtureWithConsumers(
		t,
		[]string{"request-id"},
		consumers,
		map[generation.Domain]generation.PublicationCandidate{generation.DomainHTTP: candidate},
	)

	plan, err := prepared.planHTTPPreparation(context.Background(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil || plan.resources.revision != 31 || plan.publicAPIRegistry == nil ||
		len(plan.plugins.Routes) != 1 || plan.plugins.Routes[0].Route.ID != "r1" {
		t.Fatalf("HTTP preparation plan = %#v", plan)
	}
	if got := plan.plugins.Routes[0].Local; len(got) != 1 || got[0].Factory != "request-id" ||
		got[0].Source.Kind != "plugin_configs" || got[0].Source.ID != "pc1" {
		t.Fatalf("planned route bindings = %#v", got)
	}
	if _, exists := plan.plugins.Consumers["consumer-1"]; !exists {
		t.Fatalf(
			"prepared consumer plans/ids = %#v/%#v",
			plan.plugins.Consumers,
			plan.resources.consumerIDs,
		)
	}
}

func TestPlanHTTPPreparationRejectsCandidateOutsideAttempt(t *testing.T) {
	first, err := generation.NewSnapshot(32, []generation.Resource{
		resourceValue("routes", "r1", `{"id":"r1","uri":"/first"}`),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := generation.NewSnapshot(33, []generation.Resource{
		resourceValue("routes", "r2", `{"id":"r2","uri":"/second"}`),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	owned := compileDomain(t, generation.DomainHTTP, first, generation.PublishedGeneration{}, false)
	foreign := compileDomain(
		t,
		generation.DomainHTTP,
		second,
		generation.PublishedGeneration{},
		false,
	)
	prepared, _ := newEffectiveBindingMaterializerFixture(
		t,
		nil,
		map[generation.Domain]generation.PublicationCandidate{generation.DomainHTTP: owned},
	)
	if _, err := prepared.planHTTPPreparation(context.Background(), foreign); err == nil {
		t.Fatal("foreign HTTP candidate error = nil")
	}
}

func TestPlanHTTPPreparationPreservesConfiguredEmptyDynamicPluginList(t *testing.T) {
	snapshot := mustGenerationSnapshot(t, 34, []generation.Resource{
		resourceValue("routes", "r1", `{"id":"r1","uri":"/","plugins":{"request-id":{}}}`),
		resourceValue("plugins", "plugins", `[]`),
	}, nil)
	candidate := compileDomain(t, generation.DomainHTTP, snapshot, generation.PublishedGeneration{}, false)
	prepared, _ := newEffectiveBindingMaterializerFixture(
		t,
		[]string{"request-id"},
		map[generation.Domain]generation.PublicationCandidate{generation.DomainHTTP: candidate},
	)
	prepared.effective.Config.Plugins = []string{"request-id"}

	plan, err := prepared.planHTTPPreparation(context.Background(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.plugins.Routes) != 0 || len(plan.plugins.Quarantined) != 1 ||
		plan.plugins.Quarantined[0] != (generation.ResourceKey{Kind: "routes", ID: "r1"}) {
		t.Fatalf("empty dynamic plugin list fell back to static plugins: %#v", plan.plugins)
	}
}
