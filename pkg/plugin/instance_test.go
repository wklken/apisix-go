package plugin

import "testing"

func TestNewInstanceKeySeparatesScopeButIgnoresMapOrder(t *testing.T) {
	descriptor := Descriptor{Factory: "limit-count", InstanceScope: InstancePerRoute}
	a, err := NewInstanceKey(
		descriptor,
		ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "r1"},
		map[string]any{"count": 10, "time_window": 60},
	)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewInstanceKey(
		descriptor,
		ScopeRoute,
		ResourceProvenance{Kind: ResourceRoute, ID: "r1"},
		map[string]any{"time_window": 60, "count": 10},
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
		map[string]any{"count": 10, "time_window": 60},
	)
	if err != nil {
		t.Fatal(err)
	}
	if a == c {
		t.Fatalf("different owners share identity: %#v", a)
	}
}
