package compiler

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/resource"
)

func TestCompileAndAttachStreamSkipsHTTPOnlyGeneration(t *testing.T) {
	prepared, _ := newEffectiveBindingMaterializerFixture(
		t,
		nil,
		map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: {},
		},
	)
	if err := prepared.compileAndAttachStream(context.Background()); err != nil {
		t.Fatal(err)
	}
	if prepared.Stream() != nil {
		t.Fatal("HTTP-only generation exposed a stream snapshot")
	}
}

func TestCompileAndAttachStreamUsesExactRouteOccurrence(t *testing.T) {
	candidate := streamCompilerCandidate(t, 91, []generation.Resource{
		resourceValue("stream_routes", "mqtt", `{
			"id":"mqtt",
			"plugins":{"mqtt-proxy":{"protocol_level":4}},
			"upstream":{"scheme":"tcp","nodes":{"127.0.0.1:1883":1}}
		}`),
	})
	prepared, fixture := newEffectiveBindingMaterializerFixture(
		t,
		nil,
		map[generation.Domain]generation.PublicationCandidate{generation.DomainStream: candidate},
	)
	prepared.effective.Config.StreamPlugins = []string{"mqtt-proxy"}
	installStreamOccurrence(
		prepared,
		generation.ResourceKey{Kind: "stream_routes", ID: "mqtt"},
		"mqtt-proxy",
		generation.DomainStream,
	)

	var gotProvenance plugin.ResourceProvenance
	prepared.bindingOps.bind = func(
		descriptor plugin.Descriptor,
		instance plugin.Plugin,
		scope plugin.Scope,
		provenance plugin.ResourceProvenance,
		identity plugin.InstanceIdentityInput,
	) (plugin.Binding, error) {
		gotProvenance = provenance
		return plugin.BindAttemptResolvedPlugin(
			prepared.attempt.AttemptID(), descriptor, instance, scope, provenance, identity,
		)
	}

	if err := prepared.compileAndAttachStream(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := prepared.Stream()
	if snapshot == nil || snapshot.Revision() != 91 || snapshot.Router() == nil ||
		!slices.Equal(snapshot.Router().RouteIDs(), []string{"mqtt"}) {
		t.Fatalf("stream snapshot = %#v", snapshot)
	}
	if fixture.constructed.Load() != 1 || fixture.registry.Len() != 1 {
		t.Fatalf(
			"stream materialization constructed/leases = %d/%d, want 1/1",
			fixture.constructed.Load(),
			fixture.registry.Len(),
		)
	}
	if gotProvenance != (plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: "mqtt"}) {
		t.Fatalf("stream binding provenance = %#v", gotProvenance)
	}
}

func TestCompileAndAttachStreamUsesExactInheritedServiceOccurrence(t *testing.T) {
	candidate := streamCompilerCandidate(t, 92, []generation.Resource{
		resourceValue("stream_routes", "mqtt", `{"id":"mqtt","service_id":"svc"}`),
		resourceValue("services", "svc", `{
			"id":"svc",
			"plugins":{"mqtt-proxy":{"protocol_level":4}},
			"upstream_id":"up"
		}`),
		resourceValue("upstreams", "up", `{"scheme":"tcp","nodes":{"127.0.0.1:1883":1}}`),
	})
	prepared, fixture := newEffectiveBindingMaterializerFixture(
		t,
		nil,
		map[generation.Domain]generation.PublicationCandidate{generation.DomainStream: candidate},
	)
	prepared.effective.Config.StreamPlugins = []string{"mqtt-proxy"}
	installStreamOccurrence(
		prepared,
		generation.ResourceKey{Kind: "services", ID: "svc"},
		"mqtt-proxy",
		generation.DomainStream,
	)

	if err := prepared.compileAndAttachStream(context.Background()); err != nil {
		t.Fatal(err)
	}
	if prepared.Stream() == nil || fixture.constructed.Load() != 1 || fixture.registry.Len() != 1 {
		t.Fatalf(
			"inherited stream materialization snapshot/constructed/leases = %v/%d/%d",
			prepared.Stream() != nil,
			fixture.constructed.Load(),
			fixture.registry.Len(),
		)
	}
}

func TestCompileAndAttachStreamRejectsMissingOrCrossDomainOccurrenceBeforeConstruction(t *testing.T) {
	candidate := streamCompilerCandidate(t, 93, []generation.Resource{
		resourceValue("stream_routes", "mqtt", `{
			"id":"mqtt",
			"plugins":{"mqtt-proxy":{"protocol_level":4}},
			"upstream":{"scheme":"tcp","nodes":{"127.0.0.1:1883":1}}
		}`),
	})
	for _, domain := range []generation.Domain{"", generation.DomainHTTP} {
		t.Run(string(domain), func(t *testing.T) {
			prepared, fixture := newEffectiveBindingMaterializerFixture(
				t,
				nil,
				map[generation.Domain]generation.PublicationCandidate{generation.DomainStream: candidate},
			)
			prepared.effective.Config.StreamPlugins = []string{"mqtt-proxy"}
			if domain != "" {
				installStreamOccurrence(
					prepared,
					generation.ResourceKey{Kind: "stream_routes", ID: "mqtt"},
					"mqtt-proxy",
					domain,
				)
			}
			if err := prepared.compileAndAttachStream(context.Background()); err == nil {
				t.Fatal("compileAndAttachStream() error = nil")
			}
			if prepared.Stream() != nil || fixture.constructed.Load() != 0 || fixture.registry.Len() != 0 {
				t.Fatalf(
					"invalid occurrence published/constructed/leased = %v/%d/%d",
					prepared.Stream() != nil,
					fixture.constructed.Load(),
					fixture.registry.Len(),
				)
			}
		})
	}
}

func TestCompileAndAttachStreamLaterBindingFailureReleasesEarlierLeaseOnce(t *testing.T) {
	candidate := streamCompilerCandidate(t, 94, []generation.Resource{
		resourceValue("stream_routes", "a", `{
			"id":"a",
			"plugins":{"mqtt-proxy":{"protocol_level":4}},
			"upstream":{"scheme":"tcp","nodes":{"127.0.0.1:1883":1}}
		}`),
		resourceValue("stream_routes", "b", `{
			"id":"b",
			"plugins":{"mqtt-proxy":{"protocol_level":4}},
			"upstream":{"scheme":"tcp","nodes":{"127.0.0.1:1884":1}}
		}`),
	})
	prepared, fixture := newEffectiveBindingMaterializerFixture(
		t,
		nil,
		map[generation.Domain]generation.PublicationCandidate{generation.DomainStream: candidate},
	)
	prepared.effective.Config.StreamPlugins = []string{"mqtt-proxy"}
	for _, id := range []string{"a", "b"} {
		installStreamOccurrence(
			prepared,
			generation.ResourceKey{Kind: "stream_routes", ID: id},
			"mqtt-proxy",
			generation.DomainStream,
		)
	}
	var validations atomic.Int64
	defaultValidate := prepared.bindingOps.validateConfig
	prepared.bindingOps.validateConfig = func(
		instance plugin.Plugin,
		config resource.PluginConfig,
	) error {
		if validations.Add(1) == 2 {
			return errors.New("second binding rejected")
		}
		return defaultValidate(instance, config)
	}
	var trace []string
	prepared.bindingOps.trace = func(stage string) { trace = append(trace, stage) }

	if err := prepared.compileAndAttachStream(context.Background()); err == nil {
		t.Fatal("compileAndAttachStream() error = nil")
	}
	if prepared.Stream() != nil || fixture.registry.Len() != 0 || fixture.constructed.Load() != 2 {
		t.Fatalf(
			"failed stream snapshot/leases/constructions = %v/%d/%d",
			prepared.Stream() != nil,
			fixture.registry.Len(),
			fixture.constructed.Load(),
		)
	}
	releases := 0
	for _, stage := range trace {
		if stage == "lease-release:mqtt-proxy" {
			releases++
		}
	}
	if releases != 1 {
		t.Fatalf("earlier stream lease releases = %d, want 1; trace=%v", releases, trace)
	}
}

func streamCompilerCandidate(
	t *testing.T,
	revision uint64,
	resources []generation.Resource,
) generation.PublicationCandidate {
	t.Helper()
	snapshot := mustGenerationSnapshot(t, revision, resources, nil)
	return compileDomain(t, generation.DomainStream, snapshot, generation.PublishedGeneration{}, false)
}

func installStreamOccurrence(
	prepared *PreparedGeneration,
	key generation.ResourceKey,
	factory string,
	domain generation.Domain,
) {
	prepared.attempt.occurrences = append(prepared.attempt.occurrences, FactoryOccurrence{
		authority: prepared.attempt.authority,
		domain:    domain,
		resource:  key,
		source:    capability.SecretPluginConfig,
		factory:   factory,
	})
}
