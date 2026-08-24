package compiler

import (
	"errors"
	"slices"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
)

var ErrInvalidInput = errors.New("invalid compiler input")

type Compiler struct {
	manifest *capability.Manifest
	schemas  *schemaSet
}

type normalizedInput struct {
	revision   uint64
	resources  map[generation.ResourceKey]normalizedResource
	tombstones map[generation.ResourceKey]generation.Tombstone
}

type normalizedResource struct {
	key      generation.ResourceKey
	raw      []byte
	document any
	view     structuralView
}

type structuralView struct {
	embeddedID        string
	hasEmbeddedID     bool
	serviceID         string
	upstreamID        string
	pluginConfigID    string
	consumerGroupID   string
	hasInlineUpstream bool
	plugins           map[string]any
}

func newNormalizedInput(revision uint64) normalizedInput {
	return normalizedInput{
		revision:   revision,
		resources:  make(map[generation.ResourceKey]normalizedResource),
		tombstones: make(map[generation.ResourceKey]generation.Tombstone),
	}
}

func (input normalizedInput) keys() []generation.ResourceKey {
	keys := make([]generation.ResourceKey, 0, len(input.resources))
	for key := range input.resources {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, compareResourceKey)
	return keys
}

type resourceIssue struct {
	Key  generation.ResourceKey
	Code string
	Err  error
}

type dependencyGraph struct {
	edges       map[generation.ResourceKey][]generation.ResourceKey
	edgeDomains map[dependencyEdge][]generation.Domain
}

type dependencyEdge struct {
	from generation.ResourceKey
	to   generation.ResourceKey
}

func newDependencyGraph() dependencyGraph {
	return dependencyGraph{
		edges:       make(map[generation.ResourceKey][]generation.ResourceKey),
		edgeDomains: make(map[dependencyEdge][]generation.Domain),
	}
}

func (graph *dependencyGraph) add(from generation.ResourceKey, to ...generation.ResourceKey) {
	graph.addForDomains(
		from,
		[]generation.Domain{generation.DomainHTTP, generation.DomainStream},
		to...,
	)
}

func (graph *dependencyGraph) addForDomains(
	from generation.ResourceKey,
	domains []generation.Domain,
	to ...generation.ResourceKey,
) {
	if len(to) == 0 {
		if _, exists := graph.edges[from]; !exists {
			graph.edges[from] = nil
		}
		return
	}
	combined := append(slices.Clone(graph.edges[from]), to...)
	slices.SortFunc(combined, compareResourceKey)
	graph.edges[from] = slices.Compact(combined)
	for _, dependency := range to {
		edge := dependencyEdge{from: from, to: dependency}
		combinedDomains := append(slices.Clone(graph.edgeDomains[edge]), domains...)
		slices.Sort(combinedDomains)
		graph.edgeDomains[edge] = slices.Compact(combinedDomains)
	}
}

func (graph dependencyGraph) supports(from, to generation.ResourceKey, domain generation.Domain) bool {
	return slices.Contains(graph.edgeDomains[dependencyEdge{from: from, to: to}], domain)
}

type validationResult struct {
	graph          dependencyGraph
	issues         []resourceIssue
	issuesByDomain map[generation.Domain][]resourceIssue
}

func (result validationResult) issuesForDomain(domain generation.Domain) []resourceIssue {
	return slices.Clone(result.issuesByDomain[domain])
}
