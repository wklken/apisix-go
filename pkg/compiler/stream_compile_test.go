package compiler

import (
	"context"
	"errors"
	"net"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/runtime"
	streamruntime "github.com/wklken/apisix-go/pkg/stream"
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

func TestCompileAndAttachStreamUsesRuntimeObserver(t *testing.T) {
	candidate := streamCompilerCandidate(t, 89, []generation.Resource{
		resourceValue("stream_routes", "raw", `{
			"id":"raw",
			"server_addr":"127.0.0.1",
			"server_port":19999,
			"upstream":{"scheme":"tcp","nodes":{"127.0.0.1:1883":1}}
		}`),
	})
	prepared, _ := newEffectiveBindingMaterializerFixture(
		t,
		nil,
		map[generation.Domain]generation.PublicationCandidate{generation.DomainStream: candidate},
	)
	results := make(chan streamruntime.Result, 1)
	prepared.observers.Stream = func(result streamruntime.Result) { results <- result }
	if err := prepared.compileAndAttachStream(context.Background()); err != nil {
		t.Fatal(err)
	}
	server, client := net.Pipe()
	defer func() { _ = client.Close() }()
	if err := prepared.Stream().Router().Serve(context.Background(), nil, server); !errors.Is(
		err, streamruntime.ErrNoStreamRoute,
	) {
		t.Fatalf("Serve() error = %v, want ErrNoStreamRoute", err)
	}
	select {
	case result := <-results:
		if !errors.Is(result.Err, streamruntime.ErrNoStreamRoute) {
			t.Fatalf("stream result = %#v", result)
		}
	default:
		t.Fatal("prepared stream router did not publish through the factory observer")
	}
}

func TestCompileAndAttachStreamRouteUpstreamIDSuppressesServiceDependency(t *testing.T) {
	candidate := streamCompilerCandidate(t, 90, []generation.Resource{
		resourceValue(
			"stream_routes",
			"stream",
			`{"id":"stream","service_id":"missing","upstream_id":"route-upstream"}`,
		),
		resourceValue(
			"upstreams",
			"route-upstream",
			`{"id":"route-upstream","scheme":"tcp","nodes":{"127.0.0.1:1883":1}}`,
		),
	})
	prepared, _ := newEffectiveBindingMaterializerFixture(
		t,
		nil,
		map[generation.Domain]generation.PublicationCandidate{generation.DomainStream: candidate},
	)

	if err := prepared.compileAndAttachStream(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snapshot := prepared.Stream(); snapshot == nil || snapshot.Router() == nil ||
		!slices.Equal(snapshot.Router().RouteIDs(), []string{"stream"}) {
		t.Fatalf("stream snapshot = %#v", snapshot)
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

func TestCompileAndAttachStreamValidatesEveryOccurrenceBeforeMaterialization(t *testing.T) {
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
	for _, test := range []struct {
		name              string
		secondOccurrences int
	}{
		{name: "missing second occurrence"},
		{name: "duplicate second occurrence", secondOccurrences: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared, fixture := newEffectiveBindingMaterializerFixture(
				t,
				nil,
				map[generation.Domain]generation.PublicationCandidate{generation.DomainStream: candidate},
			)
			prepared.effective.Config.StreamPlugins = []string{"mqtt-proxy"}
			installStreamOccurrence(
				prepared,
				generation.ResourceKey{Kind: "stream_routes", ID: "a"},
				"mqtt-proxy",
				generation.DomainStream,
			)
			for range test.secondOccurrences {
				installStreamOccurrence(
					prepared,
					generation.ResourceKey{Kind: "stream_routes", ID: "b"},
					"mqtt-proxy",
					generation.DomainStream,
				)
			}
			var observers atomic.Int64
			defaultStartObserver := prepared.bindingOps.startObserver
			prepared.bindingOps.startObserver = func(instance plugin.Plugin, tasks *runtime.TaskRegistry) error {
				observers.Add(1)
				return defaultStartObserver(instance, tasks)
			}

			if err := prepared.compileAndAttachStream(context.Background()); err == nil {
				t.Fatal("compileAndAttachStream() error = nil")
			}
			if prepared.Stream() != nil || fixture.constructed.Load() != 0 ||
				fixture.registration.materializeCalls.Load() != 0 || observers.Load() != 0 ||
				fixture.registry.Len() != 0 {
				t.Fatalf(
					"invalid occurrences published/constructed/materialized/observed/leased = %v/%d/%d/%d/%d",
					prepared.Stream() != nil,
					fixture.constructed.Load(),
					fixture.registration.materializeCalls.Load(),
					observers.Load(),
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
