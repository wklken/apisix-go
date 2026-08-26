package plugin

import (
	"testing"

	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
)

func TestLegacyInstanceKeyStringRemainsCompatible(t *testing.T) {
	key, err := NewInstanceKey(
		Descriptor{Factory: "request-id"},
		ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "r1"},
		InstanceIdentityInput{PluginConfig: nil},
	)
	if err != nil {
		t.Fatal(err)
	}
	const want = "request-id/2/route/r1/07aed809efda1763e9e7ba55d90a0699d7e3b73512d8aa511bde6de6bfae8ab8"
	if got := key.String(); got != want {
		t.Fatalf("legacy String() = %q, want %q", got, want)
	}
	if key.Attempt != (secret.AttemptID{}) {
		t.Fatalf("legacy Attempt = %x, want zero", key.Attempt)
	}
}

func TestAttemptInstanceKeyRejectsZeroAndSeparatesAttempts(t *testing.T) {
	descriptor := Descriptor{Factory: "request-id"}
	owner := ResourceProvenance{Kind: ResourceRoute, ID: "r1"}
	identity := InstanceIdentityInput{PluginConfig: nil}
	if _, err := NewAttemptInstanceKey(secret.AttemptID{}, descriptor, ScopeRoute, owner, identity); err == nil {
		t.Fatal("NewAttemptInstanceKey(zero) error = nil")
	}
	first, err := NewAttemptInstanceKey(secret.AttemptID{1}, descriptor, ScopeRoute, owner, identity)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewAttemptInstanceKey(secret.AttemptID{2}, descriptor, ScopeRoute, owner, identity)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || first.String() == second.String() {
		t.Fatalf("different attempts share identity: first=%#v second=%#v", first, second)
	}
	const want = "request-id/0100000000000000000000000000000000000000000000000000000000000000/2/route/r1/07aed809efda1763e9e7ba55d90a0699d7e3b73512d8aa511bde6de6bfae8ab8"
	if got := first.String(); got != want {
		t.Fatalf("attempt String() = %q, want %q", got, want)
	}
}

func TestBindAttemptResolvedPluginRequiresAndPreservesAttempt(t *testing.T) {
	p := New("request-id", base.Dependencies{})
	if p == nil {
		t.Fatal("request-id factory is not registered")
	}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	descriptor, err := ResolveDescriptorForFactory("request-id", p)
	if err != nil {
		t.Fatal(err)
	}
	owner := ResourceProvenance{Kind: ResourceRoute, ID: "r1"}
	identity := InstanceIdentityInput{PluginConfig: p.Config()}
	if _, err := BindAttemptResolvedPlugin(
		secret.AttemptID{}, descriptor, p, ScopeRoute, owner, identity,
	); err == nil {
		t.Fatal("BindAttemptResolvedPlugin(zero) error = nil")
	}
	attempt := secret.AttemptID{1}
	binding, err := BindAttemptResolvedPlugin(attempt, descriptor, p, ScopeRoute, owner, identity)
	if err != nil {
		t.Fatal(err)
	}
	if binding.InstanceKey.Attempt != attempt {
		t.Fatalf("binding attempt = %x, want %x", binding.InstanceKey.Attempt, attempt)
	}
	legacy, err := BindResolvedPlugin(descriptor, p, ScopeRoute, owner, identity)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.InstanceKey.Attempt != (secret.AttemptID{}) {
		t.Fatalf("legacy binding attempt = %x, want zero", legacy.InstanceKey.Attempt)
	}
}

func TestNewInstanceKeySeparatesScopeButIgnoresMapOrder(t *testing.T) {
	descriptor := Descriptor{Factory: "limit-count", InstanceScope: InstancePerRoute}
	a, err := NewInstanceKey(
		descriptor,
		ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "r1"},
		InstanceIdentityInput{PluginConfig: map[string]any{"count": 10, "time_window": 60}},
	)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewInstanceKey(
		descriptor,
		ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "r1"},
		InstanceIdentityInput{PluginConfig: map[string]any{"time_window": 60, "count": 10}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("canonical identities differ: %#v %#v", a, b)
	}

	c, err := NewInstanceKey(
		descriptor,
		ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "r2"},
		InstanceIdentityInput{PluginConfig: map[string]any{"count": 10, "time_window": 60}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if a == c {
		t.Fatalf("different owners share identity: %#v", a)
	}
}

func TestNewInstanceKeyIncludesBehaviorChangingMetadata(t *testing.T) {
	descriptor := Descriptor{Factory: "request-id", InstanceScope: InstancePerRoute}
	base := InstanceIdentityInput{
		PluginConfig:  map[string]any{"header_name": "X-Request-ID"},
		Filter:        []any{[]any{"route_id", "==", "r1"}},
		ErrorResponse: map[string]any{"message": "denied", "code": 1},
	}
	a, err := NewInstanceKey(
		descriptor,
		ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "r1"},
		base,
	)
	if err != nil {
		t.Fatal(err)
	}
	reordered, err := NewInstanceKey(
		descriptor,
		ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "r1"},
		InstanceIdentityInput{
			PluginConfig:  map[string]any{"header_name": "X-Request-ID"},
			Filter:        []any{[]any{"route_id", "==", "r1"}},
			ErrorResponse: map[string]any{"code": 1, "message": "denied"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if a != reordered {
		t.Fatalf("map order changed identity: %#v %#v", a, reordered)
	}
	for name, changed := range map[string]InstanceIdentityInput{
		"filter": {
			PluginConfig:  base.PluginConfig,
			Filter:        []any{[]any{"route_id", "==", "r2"}},
			ErrorResponse: base.ErrorResponse,
		},
		"error-response": {
			PluginConfig:  base.PluginConfig,
			Filter:        base.Filter,
			ErrorResponse: map[string]any{"message": "other", "code": 1},
		},
	} {
		t.Run(name, func(t *testing.T) {
			key, keyErr := NewInstanceKey(
				descriptor,
				ScopeRoute,
				ResourceProvenance{Kind: ResourceRoute, ID: "r1"},
				changed,
			)
			if keyErr != nil {
				t.Fatal(keyErr)
			}
			if key == a {
				t.Fatalf("behavior-changing %s did not change instance key", name)
			}
		})
	}
}
