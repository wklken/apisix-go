package compiler

import (
	"context"
	"fmt"
	"slices"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

// streamResourceSet is the owned, dependency-closed stream view of one
// publication candidate. dynamicPlugins distinguishes an absent /plugins
// document from a present empty document.
type streamResourceSet struct {
	revision       uint64
	routes         []resource.StreamRoute
	services       map[string]resource.Service
	upstreams      map[string]resource.Upstream
	enabledPlugins []string
	dynamicPlugins bool
}

func decodeStreamResourceSet(
	ctx context.Context,
	candidate generation.PublicationCandidate,
) (streamResourceSet, error) {
	if ctx == nil {
		return streamResourceSet{}, fmt.Errorf("%w: stream decode context is required", ErrInvalidInput)
	}
	if err := generation.ValidatePublicationCandidate(
		generation.DomainStream,
		candidate.Artifact.Revision,
		candidate,
	); err != nil {
		return streamResourceSet{}, err
	}
	input, issues, err := normalizeContext(ctx, candidate.Snapshot)
	if err != nil {
		return streamResourceSet{}, err
	}
	if len(issues) != 0 {
		return streamResourceSet{}, fmt.Errorf("%w: stream candidate is not normalized", ErrInvalidInput)
	}
	result := streamResourceSet{
		revision:  candidate.Artifact.Revision,
		services:  make(map[string]resource.Service),
		upstreams: make(map[string]resource.Upstream),
	}
	for _, key := range input.keys() {
		if err := ctx.Err(); err != nil {
			return streamResourceSet{}, err
		}
		normalized := input.resources[key]
		typedDocument := typedResourceDocument(normalized)
		switch key.Kind {
		case "stream_routes":
			var value resource.StreamRoute
			if err := util.Parse(typedDocument, &value); err != nil {
				return streamResourceSet{}, streamResourceDecodeError(key, err)
			}
			if value.ID == "" {
				value.ID = key.ID
			}
			result.routes = append(result.routes, value)
		case "services":
			var value resource.Service
			if err := util.Parse(typedDocument, &value); err != nil {
				return streamResourceSet{}, streamResourceDecodeError(key, err)
			}
			if value.ID == "" {
				value.ID = key.ID
			}
			result.services[key.ID] = value
		case "upstreams":
			var value resource.Upstream
			if err := util.Parse(typedDocument, &value); err != nil {
				return streamResourceSet{}, streamResourceDecodeError(key, err)
			}
			result.upstreams[key.ID] = value
		case "plugins":
			result.dynamicPlugins = true
			result.enabledPlugins = make([]string, 0)
			for _, name := range sortedFactories(normalized.view.plugins) {
				entry, ok := normalized.view.plugins[name].(map[string]any)
				if !ok {
					return streamResourceSet{}, streamResourceDecodeError(key, ErrInvalidInput)
				}
				stream, _ := entry["stream"].(bool)
				if stream {
					result.enabledPlugins = append(result.enabledPlugins, name)
				}
			}
		}
	}
	result.enabledPlugins = slices.Clone(result.enabledPlugins)
	return result, nil
}

func streamResourceDecodeError(key generation.ResourceKey, err error) error {
	return fmt.Errorf("%w: decode stream %s/%s: %v", ErrInvalidInput, key.Kind, key.ID, err)
}
