package compiler

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
)

func validateContext(
	ctx context.Context,
	input normalizedInput,
	manifest *capability.Manifest,
) (validationResult, error) {
	result := validationResult{
		graph:          newDependencyGraph(),
		issuesByDomain: make(map[generation.Domain][]resourceIssue, 2),
	}
	for _, key := range input.keys() {
		if err := ctx.Err(); err != nil {
			return validationResult{}, err
		}
		resource := input.resources[key]
		result.graph.add(key)
		if resource.document == nil {
			continue
		}
		addStructuralEdges(&result.graph, input, resource)
		validatePluginNames(resource, manifest, &result.issues)
		addDocumentEdges(&result.graph, resource, manifest, &result.issues, result.issuesByDomain)
	}
	baseIssues := slices.Clone(result.issues)
	result.issues = nil
	for _, domain := range []generation.Domain{generation.DomainHTTP, generation.DomainStream} {
		if err := ctx.Err(); err != nil {
			return validationResult{}, err
		}
		domainIssues := slices.Clone(result.issuesByDomain[domain])
		for _, issue := range baseIssues {
			if slices.Contains(generation.DomainsForResourceKind(issue.Key.Kind), domain) {
				domainIssues = append(domainIssues, issue)
			}
		}
		dependencyIssues, err := validateDomainDependencies(ctx, input, result.graph, domain)
		if err != nil {
			return validationResult{}, err
		}
		domainIssues = append(domainIssues, dependencyIssues...)
		domainIssues = compactIssues(domainIssues)
		result.issuesByDomain[domain] = domainIssues
		result.issues = append(result.issues, domainIssues...)
	}
	result.issues = compactIssues(result.issues)
	return result, nil
}

func validateDomainDependencies(
	ctx context.Context,
	input normalizedInput,
	graph dependencyGraph,
	domain generation.Domain,
) ([]resourceIssue, error) {
	filtered := newDependencyGraph()
	issues := make([]resourceIssue, 0)
	for _, from := range sortedGraphKeys(graph.edges) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !slices.Contains(generation.DomainsForResourceKind(from.Kind), domain) {
			continue
		}
		filtered.add(from)
		for _, to := range graph.edges[from] {
			if !graph.supports(from, to, domain) {
				continue
			}
			if !slices.Contains(generation.DomainsForResourceKind(to.Kind), domain) {
				continue
			}
			filtered.add(from, to)
			if _, exists := input.resources[to]; !exists {
				issues = append(issues, resourceIssue{
					Key: from, Code: "dependency-missing",
					Err: fmt.Errorf("dependency %s/%s is missing", to.Kind, to.ID),
				})
				break
			}
		}
	}
	cycleKeys, err := dependencyCycles(ctx, filtered)
	if err != nil {
		return nil, err
	}
	for _, key := range cycleKeys {
		issues = append(issues, newIssue(key, "dependency-cycle", "resource dependency cycle"))
	}
	return issues, nil
}

func addStructuralEdges(graph *dependencyGraph, input normalizedInput, resource normalizedResource) {
	key := resource.key
	switch key.Kind {
	case "routes", "stream_routes":
		if resource.view.pluginConfigID != "" {
			graph.add(key, generation.ResourceKey{Kind: "plugin_configs", ID: resource.view.pluginConfigID})
		}
		addEffectiveUpstreamEdges(graph, input, resource)
	case "services":
		if resource.view.upstreamID != "" && !resource.view.hasInlineUpstream {
			graph.add(key, generation.ResourceKey{Kind: "upstreams", ID: resource.view.upstreamID})
		}
	case "consumers":
		if resource.view.consumerGroupID != "" {
			graph.add(key, generation.ResourceKey{Kind: "consumer_groups", ID: resource.view.consumerGroupID})
		}
	}
}

func addEffectiveUpstreamEdges(graph *dependencyGraph, input normalizedInput, resource normalizedResource) {
	key := resource.key
	if resource.view.serviceID != "" {
		graph.add(key, generation.ResourceKey{Kind: "services", ID: resource.view.serviceID})
	}
	if resource.view.hasInlineUpstream {
		return
	}
	if resource.view.upstreamID != "" {
		graph.add(key, generation.ResourceKey{Kind: "upstreams", ID: resource.view.upstreamID})
		return
	}
	if resource.view.serviceID == "" {
		return
	}
	serviceKey := generation.ResourceKey{Kind: "services", ID: resource.view.serviceID}
	service, exists := input.resources[serviceKey]
	if !exists || service.view.hasInlineUpstream || service.view.upstreamID == "" {
		return
	}
	graph.add(key, generation.ResourceKey{Kind: "upstreams", ID: service.view.upstreamID})
}

func validatePluginNames(resource normalizedResource, manifest *capability.Manifest, issues *[]resourceIssue) {
	for name := range resource.view.plugins {
		if _, exists := manifest.Plugin(name); !exists {
			*issues = append(
				*issues,
				newIssue(resource.key, "plugin-unsupported", "plugin is absent from capability manifest"),
			)
			return
		}
	}
}

func addDocumentEdges(
	graph *dependencyGraph,
	resource normalizedResource,
	manifest *capability.Manifest,
	issues *[]resourceIssue,
	issuesByDomain map[generation.Domain][]resourceIssue,
) {
	walkNonPluginDocument(resource.document, resourceKindHasPlugins(resource.key.Kind), func(value string) {
		if !strings.HasPrefix(value, "$secret://") {
			return
		}
		key, valid := parseSecretReference(value)
		if !valid {
			*issues = append(
				*issues,
				newIssue(resource.key, "secret-reference-invalid", "managed secret reference must be manager/id/path"),
			)
			return
		}
		graph.add(resource.key, key)
	})
	object, ok := resource.document.(map[string]any)
	if !ok {
		return
	}
	addUpstreamTLSReference(graph, resource.key, object["upstream"], nil, issues)
	if resource.key.Kind == "upstreams" {
		addUpstreamTLSReference(graph, resource.key, object, nil, issues)
	}
	for name, config := range resource.view.plugins {
		domains := pluginDependencyDomains(manifest, name)
		if len(domains) == 0 {
			continue
		}
		pluginIssues := make([]resourceIssue, 0)
		walkDocument(config, func(value string) {
			if !strings.HasPrefix(value, "$secret://") {
				return
			}
			key, valid := parseSecretReference(value)
			if !valid {
				pluginIssues = append(
					pluginIssues,
					newIssue(
						resource.key,
						"secret-reference-invalid",
						"managed secret reference must be manager/id/path",
					),
				)
				return
			}
			graph.addForDomains(resource.key, domains, key)
		})
		switch name {
		case "grpc-transcode":
			if pluginObject, ok := config.(map[string]any); ok {
				if rawID, exists := pluginObject["proto_id"]; exists {
					if id, valid := referenceID(rawID); valid && id != "" {
						graph.addForDomains(
							resource.key,
							domains,
							generation.ResourceKey{Kind: "protos", ID: id},
						)
					} else {
						pluginIssues = append(
							pluginIssues,
							newIssue(resource.key, "reference-invalid", "grpc-transcode proto_id is invalid"),
						)
					}
				}
			}
		case "traffic-split":
			addTrafficSplitEdges(graph, resource.key, config, domains, &pluginIssues)
		}
		for _, domain := range domains {
			issuesByDomain[domain] = append(issuesByDomain[domain], pluginIssues...)
		}
	}
}

func walkNonPluginDocument(document any, pluginsAreRuntimeMap bool, visitString func(string)) {
	if !pluginsAreRuntimeMap {
		walkDocument(document, visitString)
		return
	}
	object, ok := document.(map[string]any)
	if !ok {
		walkDocument(document, visitString)
		return
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		if key != "plugins" {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	for _, key := range keys {
		walkDocument(object[key], visitString)
	}
}

func pluginDependencyDomains(manifest *capability.Manifest, name string) []generation.Domain {
	plugin, exists := manifest.Plugin(name)
	if !exists {
		return []generation.Domain{}
	}
	domains := make([]generation.Domain, 0, len(plugin.Domains))
	for _, domain := range plugin.Domains {
		switch domain {
		case capability.DomainHTTP:
			domains = append(domains, generation.DomainHTTP)
		case capability.DomainStream:
			domains = append(domains, generation.DomainStream)
		}
	}
	slices.Sort(domains)
	return slices.Compact(domains)
}

func addUpstreamTLSReference(
	graph *dependencyGraph,
	owner generation.ResourceKey,
	raw any,
	domains []generation.Domain,
	issues *[]resourceIssue,
) {
	upstream, ok := raw.(map[string]any)
	if !ok {
		return
	}
	tls, ok := upstream["tls"].(map[string]any)
	if !ok {
		return
	}
	rawID, exists := tls["client_cert_id"]
	if !exists {
		return
	}
	if id, valid := tlsReferenceID(rawID); valid {
		addScopedDependency(graph, owner, domains, generation.ResourceKey{Kind: "ssls", ID: id})
	} else {
		*issues = append(*issues, newIssue(owner, "reference-invalid", "client_cert_id is invalid"))
	}
}

func addScopedDependency(
	graph *dependencyGraph,
	owner generation.ResourceKey,
	domains []generation.Domain,
	dependency generation.ResourceKey,
) {
	if domains == nil {
		graph.add(owner, dependency)
		return
	}
	graph.addForDomains(owner, domains, dependency)
}

func tlsReferenceID(value any) (string, bool) {
	if text, ok := value.(string); ok {
		text = strings.TrimSpace(text)
		return text, text != ""
	}
	id, valid := referenceID(value)
	return id, valid && id != ""
}

func addTrafficSplitEdges(
	graph *dependencyGraph,
	owner generation.ResourceKey,
	raw any,
	domains []generation.Domain,
	issues *[]resourceIssue,
) {
	config, ok := raw.(map[string]any)
	if !ok {
		return
	}
	rules, _ := config["rules"].([]any)
	for _, rawRule := range rules {
		rule, _ := rawRule.(map[string]any)
		weighted, _ := rule["weighted_upstreams"].([]any)
		for _, rawTarget := range weighted {
			target, _ := rawTarget.(map[string]any)
			if inline, exists := target["upstream"]; exists && inline != nil {
				addUpstreamTLSReference(graph, owner, inline, domains, issues)
				continue
			}
			if rawID, exists := target["upstream_id"]; exists {
				if id, valid := trafficSplitUpstreamID(rawID); valid {
					addScopedDependency(
						graph,
						owner,
						domains,
						generation.ResourceKey{Kind: "upstreams", ID: id},
					)
				} else {
					*issues = append(
						*issues,
						newIssue(owner, "reference-invalid", "traffic-split upstream_id is invalid"),
					)
				}
			}
		}
	}
}

func trafficSplitUpstreamID(value any) (string, bool) {
	if text, ok := value.(string); ok {
		return text, text != ""
	}
	id, valid := referenceID(value)
	return id, valid && id != "" && id != "0" && !strings.HasPrefix(id, "-")
}

func parseSecretReference(reference string) (generation.ResourceKey, bool) {
	parts := strings.Split(strings.TrimPrefix(reference, "$secret://"), "/")
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return generation.ResourceKey{}, false
	}
	if slices.Contains(parts[2:], "") {
		return generation.ResourceKey{}, false
	}
	return generation.ResourceKey{Kind: "secrets", ID: parts[0] + "/" + parts[1]}, true
}

func walkDocument(value any, visitString func(string)) {
	switch typed := value.(type) {
	case string:
		visitString(typed)
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			walkDocument(typed[key], visitString)
		}
	case []any:
		for _, item := range typed {
			walkDocument(item, visitString)
		}
	}
}

func dependencyCycles(
	ctx context.Context,
	graph dependencyGraph,
) ([]generation.ResourceKey, error) {
	const (
		white = iota
		gray
		black
	)
	colors := make(map[generation.ResourceKey]int, len(graph.edges))
	stack := make([]generation.ResourceKey, 0)
	inCycle := make(map[generation.ResourceKey]struct{})
	var visit func(generation.ResourceKey) error
	visit = func(key generation.ResourceKey) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		colors[key] = gray
		stack = append(stack, key)
		for _, dependency := range graph.edges[key] {
			switch colors[dependency] {
			case white:
				if err := visit(dependency); err != nil {
					return err
				}
			case gray:
				for _, member := range slices.Backward(stack) {
					inCycle[member] = struct{}{}
					if member == dependency {
						break
					}
				}
			}
		}
		stack = stack[:len(stack)-1]
		colors[key] = black
		return nil
	}
	for _, key := range sortedGraphKeys(graph.edges) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if colors[key] == white {
			if err := visit(key); err != nil {
				return nil, err
			}
		}
	}
	keys := make([]generation.ResourceKey, 0, len(inCycle))
	for key := range inCycle {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, compareResourceKey)
	return keys, nil
}

func sortedGraphKeys(edges map[generation.ResourceKey][]generation.ResourceKey) []generation.ResourceKey {
	keys := make([]generation.ResourceKey, 0, len(edges))
	for key := range edges {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, compareResourceKey)
	return keys
}

func compactIssues(issues []resourceIssue) []resourceIssue {
	sortIssues(issues)
	result := make([]resourceIssue, 0, len(issues))
	seen := make(map[struct {
		key  generation.ResourceKey
		code string
	}]struct{}, len(issues))
	for _, issue := range issues {
		identity := struct {
			key  generation.ResourceKey
			code string
		}{key: issue.Key, code: issue.Code}
		if _, exists := seen[identity]; exists {
			continue
		}
		seen[identity] = struct{}{}
		result = append(result, issue)
	}
	return result
}
