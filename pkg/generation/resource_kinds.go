package generation

import "slices"

var managedResources = []struct {
	kind    string
	domains []Domain
}{
	{kind: "consumer_groups", domains: []Domain{DomainHTTP}},
	{kind: "consumers", domains: []Domain{DomainHTTP}},
	{kind: "global_rules", domains: []Domain{DomainHTTP}},
	{kind: "plugin_configs", domains: []Domain{DomainHTTP}},
	{kind: "plugin_metadata", domains: []Domain{DomainHTTP}},
	{kind: "plugins", domains: []Domain{DomainHTTP}},
	{kind: "protos", domains: []Domain{DomainHTTP}},
	{kind: "routes", domains: []Domain{DomainHTTP}},
	{kind: "secrets", domains: []Domain{DomainHTTP, DomainStream}},
	{kind: "services", domains: []Domain{DomainHTTP, DomainStream}},
	{kind: "ssls", domains: []Domain{DomainHTTP}},
	{kind: "stream_routes", domains: []Domain{DomainStream}},
	{kind: "upstreams", domains: []Domain{DomainHTTP, DomainStream}},
}

// ManagedResourceKinds returns every provider-managed resource kind in stable order.
func ManagedResourceKinds() []string {
	kinds := make([]string, len(managedResources))
	for index, resource := range managedResources {
		kinds[index] = resource.kind
	}
	return kinds
}

// IsManagedResourceKind reports whether kind belongs to the provider-managed namespace.
func IsManagedResourceKind(kind string) bool {
	for _, resource := range managedResources {
		if resource.kind == kind {
			return true
		}
	}
	return false
}

// DomainsForResourceKind returns the publication domains affected by kind.
func DomainsForResourceKind(kind string) []Domain {
	for _, resource := range managedResources {
		if resource.kind == kind {
			return slices.Clone(resource.domains)
		}
	}
	return nil
}
