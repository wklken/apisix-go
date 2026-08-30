package compiler

import (
	"context"
	"fmt"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin"
	routepkg "github.com/wklken/apisix-go/pkg/route"
)

func (prepared *PreparedGeneration) materializeHTTPPluginPlans(
	ctx context.Context,
	executionOwner generation.ResourceKey,
	plans []routepkg.PluginPlan,
	resourceContext effectiveBindingResourceContext,
	runtimeContext effectiveBindingRuntimeContext,
	recoverable bool,
) ([]plugin.Binding, error) {
	if len(plans) == 0 {
		return nil, nil
	}
	specs := make([]effectiveBindingSpec, 0, len(plans))
	for _, plan := range plans {
		spec, err := prepared.effectiveHTTPBindingSpec(
			executionOwner,
			plan,
			resourceContext,
			runtimeContext,
		)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	finalize := func(bindings []plugin.Binding) ([]plugin.Binding, error) {
		if len(bindings) != len(plans) {
			return nil, fmt.Errorf("%w: HTTP binding result count is invalid", ErrInvalidInput)
		}
		result := make([]plugin.Binding, len(bindings))
		for index, binding := range bindings {
			applied, err := plans[index].Apply(binding)
			if err != nil {
				return nil, err
			}
			result[index] = applied
		}
		return result, nil
	}
	if recoverable {
		return prepared.materializeEffectiveBindingsRecoverableFinalized(ctx, specs, finalize)
	}
	return prepared.materializeEffectiveBindingsWithPolicy(ctx, specs, false, finalize)
}

func (prepared *PreparedGeneration) effectiveHTTPBindingSpec(
	executionOwner generation.ResourceKey,
	plan routepkg.PluginPlan,
	resourceContext effectiveBindingResourceContext,
	runtimeContext effectiveBindingRuntimeContext,
) (effectiveBindingSpec, error) {
	if prepared == nil || !prepared.preparation.secrets.Valid() || plan.Factory == "" ||
		plan.Source.Kind == "" || plan.Source.ID == "" {
		return effectiveBindingSpec{}, fmt.Errorf(
			"%w: HTTP binding plan is incomplete",
			ErrInvalidInput,
		)
	}
	source, err := prepared.effectiveHTTPBindingSource(executionOwner, plan)
	if err != nil {
		return effectiveBindingSpec{}, err
	}
	return effectiveBindingSpec{
		domain: generation.DomainHTTP, executionOwner: executionOwner,
		source: source, factory: plan.Factory, config: plan.Config,
		scope: plan.Scope, provenance: plan.Provenance,
		resourceContext: resourceContext, runtimeContext: runtimeContext,
		filterIdentity: plan.FilterIdentity, errorIdentity: plan.ErrorResponse,
	}, nil
}

func (prepared *PreparedGeneration) effectiveHTTPBindingSource(
	executionOwner generation.ResourceKey,
	plan routepkg.PluginPlan,
) (effectiveBindingSource, error) {
	switch plan.Source.Kind {
	case "system":
		if plan.Source != executionOwner || plan.Source.ID != plan.Factory ||
			plan.Scope != plugin.ScopeSystem ||
			plan.Provenance != (plugin.ResourceProvenance{Kind: plugin.ResourceSystem, ID: plan.Factory}) {
			return effectiveBindingSource{}, fmt.Errorf(
				"%w: system HTTP binding authority is invalid",
				ErrInvalidInput,
			)
		}
		return effectiveBindingSource{kind: effectiveBindingSystem, resource: plan.Source}, nil
	case "consumers":
		if plan.Scope != plugin.ScopeConsumer ||
			plan.Provenance != (plugin.ResourceProvenance{Kind: plugin.ResourceConsumer, ID: plan.Source.ID}) {
			return effectiveBindingSource{}, fmt.Errorf(
				"%w: consumer HTTP binding authority is invalid",
				ErrInvalidInput,
			)
		}
		for _, occurrence := range prepared.preparation.Occurrences(capability.SecretConsumerConfig) {
			if occurrence.Domain() == generation.DomainHTTP &&
				occurrence.Resource() == plan.Source &&
				occurrence.Factory() == plan.Factory {
				return effectiveBindingSource{
					kind: effectiveBindingPreparedConsumer, resource: plan.Source,
					source: capability.SecretConsumerConfig, occurrence: occurrence,
				}, nil
			}
		}
		return effectiveBindingSource{}, fmt.Errorf(
			"%w: exact HTTP consumer factory occurrence is missing",
			ErrInvalidInput,
		)
	default:
		wantScope, wantProvenance, ok := effectivePluginSourceIdentity(plan.Source)
		if !ok || plan.Scope != wantScope || plan.Provenance != wantProvenance {
			return effectiveBindingSource{}, fmt.Errorf(
				"%w: HTTP binding source was relabeled",
				ErrInvalidInput,
			)
		}
		for _, occurrence := range prepared.preparation.Occurrences(capability.SecretPluginConfig) {
			if occurrence.Domain() == generation.DomainHTTP &&
				occurrence.Resource() == plan.Source &&
				occurrence.Factory() == plan.Factory {
				return effectiveBindingSource{
					kind: effectiveBindingPluginConfig, resource: plan.Source,
					source: capability.SecretPluginConfig, occurrence: occurrence,
				}, nil
			}
		}
		return effectiveBindingSource{}, fmt.Errorf(
			"%w: exact HTTP factory occurrence is missing",
			ErrInvalidInput,
		)
	}
}
