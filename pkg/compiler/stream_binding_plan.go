package compiler

import (
	"context"
	"fmt"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin"
	streamruntime "github.com/wklken/apisix-go/pkg/stream"
)

func (prepared *PreparedGeneration) materializePlannedStreamRoutes(
	ctx context.Context,
	planned []plannedStreamRoute,
) ([]streamruntime.PreparedRoute, error) {
	routes := make([]streamruntime.PreparedRoute, len(planned))
	specs := make([]effectiveBindingSpec, 0, len(planned))
	bindingRoutes := make([]int, 0, len(planned))
	for index := range planned {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		routes[index].Route = planned[index].route
		if planned[index].binding == nil {
			continue
		}
		spec, err := prepared.effectiveStreamBindingSpec(planned[index])
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
		bindingRoutes = append(bindingRoutes, index)
	}
	if len(specs) == 0 {
		return routes, nil
	}
	bindings, err := prepared.materializeEffectiveBindingsWithPolicy(
		ctx,
		specs,
		false,
		func(bindings []plugin.Binding) ([]plugin.Binding, error) {
			if len(bindings) != len(bindingRoutes) {
				return nil, fmt.Errorf(
					"%w: stream binding result count does not match its final plan",
					ErrInvalidInput,
				)
			}
			for index, binding := range bindings {
				request := planned[bindingRoutes[index]].binding
				if binding.Descriptor.Factory != request.factory ||
					binding.Scope != request.scope ||
					binding.Provenance != request.provenance {
					return nil, fmt.Errorf(
						"%w: stream binding result does not match its final plan",
						ErrInvalidInput,
					)
				}
			}
			return bindings, nil
		},
	)
	if err != nil {
		return nil, err
	}
	for index, binding := range bindings {
		routes[bindingRoutes[index]].Protocol = binding
	}
	return routes, nil
}

func (prepared *PreparedGeneration) effectiveStreamBindingSpec(
	planned plannedStreamRoute,
) (effectiveBindingSpec, error) {
	if prepared == nil || !prepared.preparation.secrets.Valid() || planned.route.ID == "" ||
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
	for _, occurrence := range prepared.preparation.Occurrences(capability.SecretPluginConfig) {
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
