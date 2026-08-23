package plugin

import "testing"

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
