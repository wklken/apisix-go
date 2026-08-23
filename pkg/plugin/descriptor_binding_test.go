package plugin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func TestDescriptorBindingResolvesConfigAwareStageOnce(t *testing.T) {
	config := &countingResponseTestConfig{descriptor: base.BindingPhaseDescriptor{Header: true}}
	p := newResponseTestPlugin("echo", 1, config)
	binding, err := BindPluginChecked(
		"echo",
		p,
		ScopeRoute,
		ResourceProvenance{Kind: ResourcePluginConfig, ID: "pc1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if binding.Descriptor.RequestStage() != RequestStageNone || binding.Descriptor.Factory != "echo" ||
		!binding.Descriptor.HasPhase(PhaseHeaderFilter) {
		t.Fatalf("binding = %#v", binding)
	}
	if calls := config.calls.Load(); calls != 1 {
		t.Fatalf("DescribeBindingPhases calls = %d, want 1", calls)
	}
}

func TestDescriptorBindingRejectsUndeclaredConfigPhase(t *testing.T) {
	p := newResponseTestPlugin(
		"echo",
		1,
		&countingResponseTestConfig{descriptor: base.BindingPhaseDescriptor{RequestStage: "access"}},
	)
	if _, err := BindPluginChecked(
		"echo",
		p,
		ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "r1"},
	); err == nil {
		t.Fatal("BindPluginChecked() error = nil")
	}
}

func TestDescriptorBindingUsesManifestPriorityInsteadOfMutablePluginPriority(t *testing.T) {
	p := New("request-id", base.Dependencies{})
	if p == nil {
		t.Fatal("request-id factory is not registered")
	}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	setter, ok := p.(interface{ SetPriority(int) })
	if !ok {
		t.Fatal("request-id plugin has no priority setter")
	}
	setter.SetPriority(-1)
	binding, err := BindPluginChecked(
		"request-id",
		p,
		ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "r1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if binding.Priority != binding.Descriptor.Priority || binding.Priority == p.GetPriority() {
		t.Fatalf(
			"binding priority = %d, descriptor = %d, mutable plugin = %d",
			binding.Priority,
			binding.Descriptor.Priority,
			p.GetPriority(),
		)
	}
}

func TestDescriptorBindingRejectsScopeOutsideManifest(t *testing.T) {
	p := New("request-context", base.Dependencies{})
	if p == nil {
		t.Fatal("request-context factory is not registered")
	}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if _, err := BindPluginChecked(
		"request-context",
		p,
		ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "r1"},
	); err == nil {
		t.Fatal("BindPluginChecked() error = nil, want manifest scope rejection")
	}
}

func TestDescriptorBindingAcceptsExistingMaterializationScopes(t *testing.T) {
	tests := []struct {
		name       string
		factory    string
		scope      Scope
		provenance ResourceProvenance
	}{
		{
			name: "system", factory: "request-context", scope: ScopeSystem,
			provenance: ResourceProvenance{Kind: ResourceSystem, ID: "system"},
		},
		{
			name: "global", factory: "request-id", scope: ScopeGlobal,
			provenance: ResourceProvenance{Kind: ResourceGlobalRule, ID: "global"},
		},
		{
			name: "route", factory: "request-id", scope: ScopeRoute,
			provenance: ResourceProvenance{Kind: ResourceRoute, ID: "route"},
		},
		{
			name: "service", factory: "request-id", scope: ScopeRoute,
			provenance: ResourceProvenance{Kind: ResourceService, ID: "service"},
		},
		{
			name: "consumer", factory: "request-id", scope: ScopeConsumer,
			provenance: ResourceProvenance{Kind: ResourceConsumer, ID: "consumer"},
		},
		{
			name: "consumer-group", factory: "request-id", scope: ScopeConsumer,
			provenance: ResourceProvenance{Kind: ResourceConsumerGroup, ID: "group"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := New(test.factory, base.Dependencies{})
			if p == nil {
				t.Fatalf("factory %q is not registered", test.factory)
			}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}
			if _, err := BindPluginChecked(test.factory, p, test.scope, test.provenance); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type descriptorBindingTraceKey struct{}

func TestDescriptorStageRunsHandlerAndPropagatesReplacementRequest(t *testing.T) {
	p := newExecutorLegacyPlugin("real_ip", 1, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(
				r.Context(),
				descriptorBindingTraceKey{},
				"replacement",
			)))
		})
	})
	binding, err := BindPluginChecked(
		"real-ip",
		p,
		ScopeGlobal,
		ResourceProvenance{Kind: ResourceRoute, ID: "r1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	var terminalRequest *http.Request
	NewScopedExecutor(binding).Then(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		terminalRequest = r
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if terminalRequest == nil ||
		terminalRequest.Context().Value(descriptorBindingTraceKey{}) != "replacement" {
		t.Fatalf("terminal request = %#v", terminalRequest)
	}
}
