package compiler

import (
	"context"
	"fmt"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	streamruntime "github.com/wklken/apisix-go/pkg/stream"
)

func (prepared *PreparedGeneration) materializePlannedStreamRoute(
	ctx context.Context,
	planned plannedStreamRoute,
) (streamruntime.PreparedRoute, error) {
	result := streamruntime.PreparedRoute{Route: planned.route}
	if planned.binding == nil {
		return result, nil
	}
	spec, err := prepared.effectiveStreamBindingSpec(planned)
	if err != nil {
		return streamruntime.PreparedRoute{}, err
	}
	bindings, err := prepared.materializeEffectiveBindings(ctx, []effectiveBindingSpec{spec})
	if err != nil {
		return streamruntime.PreparedRoute{}, err
	}
	if len(bindings) != 1 || bindings[0].Descriptor.Factory != planned.binding.factory ||
		bindings[0].Scope != planned.binding.scope ||
		bindings[0].Provenance != planned.binding.provenance {
		return streamruntime.PreparedRoute{}, fmt.Errorf(
			"%w: stream binding result does not match its final plan",
			ErrInvalidInput,
		)
	}
	result.Protocol = bindings[0]
	return result, nil
}

func (prepared *PreparedGeneration) effectiveStreamBindingSpec(
	planned plannedStreamRoute,
) (effectiveBindingSpec, error) {
	if prepared == nil || prepared.attempt.authority == nil || planned.route.ID == "" ||
		planned.binding == nil || planned.binding.factory == "" {
		return effectiveBindingSpec{}, fmt.Errorf(
			"%w: stream binding plan is incomplete",
			ErrInvalidInput,
		)
	}
	executionOwner := generation.ResourceKey{Kind: "stream_routes", ID: planned.route.ID}
	source, err := prepared.effectiveStreamBindingSource(executionOwner, *planned.binding)
	if err != nil {
		return effectiveBindingSpec{}, err
	}
	return effectiveBindingSpec{
		domain:         generation.DomainStream,
		executionOwner: executionOwner,
		source:         source,
		factory:        planned.binding.factory,
		config:         planned.binding.config,
		scope:          planned.binding.scope,
		provenance:     planned.binding.provenance,
		resourceContext: effectiveBindingResourceContext{
			kind: effectiveBindingContextStream, streamRoute: planned.route, service: planned.service,
		},
	}, nil
}

func (prepared *PreparedGeneration) effectiveStreamBindingSource(
	executionOwner generation.ResourceKey,
	request streamBindingRequest,
) (effectiveBindingSource, error) {
	wantScope, wantProvenance, ok := effectivePluginSourceIdentity(request.source)
	if executionOwner.Kind != "stream_routes" || executionOwner.ID == "" ||
		!ok || request.scope != wantScope || request.provenance != wantProvenance {
		return effectiveBindingSource{}, fmt.Errorf(
			"%w: stream binding source was relabeled",
			ErrInvalidInput,
		)
	}
	var exact FactoryOccurrence
	matches := 0
	for _, occurrence := range prepared.attempt.Occurrences(capability.SecretPluginConfig) {
		if occurrence.Domain() == generation.DomainStream &&
			occurrence.Resource() == request.source &&
			occurrence.Factory() == request.factory {
			exact = occurrence
			matches++
		}
	}
	if matches != 1 {
		return effectiveBindingSource{}, fmt.Errorf(
			"%w: exact stream factory occurrence is missing or duplicated",
			ErrInvalidInput,
		)
	}
	return effectiveBindingSource{
		kind: effectiveBindingPluginConfig, resource: request.source,
		source: capability.SecretPluginConfig, occurrence: exact,
	}, nil
}
