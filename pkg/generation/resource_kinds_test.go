package generation

import (
	"slices"
	"testing"
)

func TestManagedResourceKindsExactMembership(t *testing.T) {
	for _, resource := range managedResources {
		if !IsManagedResourceKind(resource.kind) {
			t.Errorf("IsManagedResourceKind(%q) = false, want true", resource.kind)
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

	for _, resource := range managedResources {
		kind := resource.kind
		want := wantHTTP
		switch kind {
		case "stream_routes":
			want = wantStream
		case "plugins", "services", "upstreams", "secrets":
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
