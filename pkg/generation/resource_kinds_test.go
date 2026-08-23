package generation

import (
	"slices"
	"testing"
)

func TestManagedResourceKindsReturnsSortedDefensiveCopy(t *testing.T) {
	want := []string{
		"consumer_groups",
		"consumers",
		"global_rules",
		"plugin_configs",
		"plugin_metadata",
		"plugins",
		"protos",
		"routes",
		"secrets",
		"services",
		"ssls",
		"stream_routes",
		"upstreams",
	}

	first := ManagedResourceKinds()
	if !slices.Equal(first, want) {
		t.Fatalf("ManagedResourceKinds() = %v, want %v", first, want)
	}
	first[0] = "mutated"
	if got := ManagedResourceKinds(); !slices.Equal(got, want) {
		t.Fatalf("ManagedResourceKinds() after caller mutation = %v, want %v", got, want)
	}
}

func TestManagedResourceKindsExactMembership(t *testing.T) {
	for _, kind := range ManagedResourceKinds() {
		if !IsManagedResourceKind(kind) {
			t.Errorf("IsManagedResourceKind(%q) = false, want true", kind)
		}
	}
	for _, kind := range []string{"", "route", "data_plane", "unknown"} {
		if IsManagedResourceKind(kind) {
			t.Errorf("IsManagedResourceKind(%q) = true, want false", kind)
		}
	}
}

func TestDomainsForResourceKindExactMappingAndDefensiveCopy(t *testing.T) {
	wantHTTP := []Domain{DomainHTTP}
	wantStream := []Domain{DomainStream}
	wantBoth := []Domain{DomainHTTP, DomainStream}

	for _, kind := range ManagedResourceKinds() {
		want := wantHTTP
		switch kind {
		case "stream_routes":
			want = wantStream
		case "services", "upstreams", "secrets":
			want = wantBoth
		}
		if got := DomainsForResourceKind(kind); !slices.Equal(got, want) {
			t.Errorf("DomainsForResourceKind(%q) = %v, want %v", kind, got, want)
		}
	}

	first := DomainsForResourceKind("services")
	first[0] = DomainStream
	if got := DomainsForResourceKind("services"); !slices.Equal(got, wantBoth) {
		t.Fatalf("DomainsForResourceKind after caller mutation = %v, want %v", got, wantBoth)
	}
	if got := DomainsForResourceKind("unknown"); got != nil {
		t.Fatalf("DomainsForResourceKind(unknown) = %v, want nil", got)
	}
}
