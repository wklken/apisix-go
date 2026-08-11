package plugin

import (
	"fmt"
	"slices"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type ResponsePhaseMask uint8

const (
	ResponsePhaseHeader ResponsePhaseMask = 1 << iota
	ResponsePhaseBufferedBody
	ResponsePhaseFinalStore
)

type ResponseBinding struct {
	Plugin     Plugin
	Scope      Scope
	Provenance ResourceProvenance
	Phases     ResponsePhaseMask
	factoryKey string
}

type responseFactorySpec struct {
	configAware bool
	mask        ResponsePhaseMask
	allowHeader bool
	allowBody   bool
}

var responseFactoryRegistry = map[string]responseFactorySpec{
	"api-breaker":              {},
	"body-transformer":         {configAware: true, allowBody: true},
	"echo":                     {configAware: true, allowHeader: true, allowBody: true},
	"error-page":               {mask: ResponsePhaseBufferedBody, allowBody: true},
	"exit-transformer":         {mask: ResponsePhaseBufferedBody, allowBody: true},
	"graphql-proxy-cache":      {mask: ResponsePhaseFinalStore},
	"proxy-cache":              {mask: ResponsePhaseFinalStore},
	"response-rewrite":         {mask: ResponsePhaseBufferedBody, allowBody: true},
	"serverless-pre-function":  {configAware: true, allowHeader: true, allowBody: true},
	"serverless-post-function": {configAware: true, allowHeader: true, allowBody: true},
}

func responseFactoryAllowsDescriptor(factoryKey string, descriptor base.BindingPhaseDescriptor) bool {
	spec, ok := responseFactoryRegistry[factoryKey]
	if !ok {
		return false
	}
	if descriptor.Header && !spec.allowHeader {
		return false
	}
	if descriptor.BufferedBody && !spec.allowBody {
		return false
	}
	if !spec.configAware && (descriptor.Header || descriptor.BufferedBody) {
		return false
	}
	stage, err := parseRequestStage(descriptor.RequestStage)
	if err != nil {
		return false
	}
	if descriptor.RequestStage == "" {
		stage = requestStageRegistry[factoryKey].Stage
	}
	switch factoryKey {
	case "echo":
		return stage == RequestStageNone && (descriptor.Header || descriptor.BufferedBody)
	case "body-transformer":
		return (stage == RequestStageRewrite && !descriptor.Header) ||
			(stage == RequestStageNone && !descriptor.Header && descriptor.BufferedBody)
	case "serverless-pre-function", "serverless-post-function":
		if descriptor.Header || descriptor.BufferedBody {
			return stage == RequestStageNone && descriptor.Header != descriptor.BufferedBody
		}
		return stage == RequestStageRewrite || stage == RequestStageAccess ||
			stage == RequestStageBeforeProxy || stage == RequestStageLegacy
	}
	return true
}

func MaterializeResponseBindings(effective EffectiveBindingSet) ([]ResponseBinding, error) {
	result := make([]ResponseBinding, 0, len(effective.global)+len(effective.merged))
	for _, partition := range [][]Binding{effective.global, effective.merged} {
		ordered := append([]Binding(nil), partition...)
		slices.SortStableFunc(ordered, func(a, b Binding) int {
			if a.Plugin == nil || b.Plugin == nil {
				return 0
			}
			if priority := b.Plugin.GetPriority() - a.Plugin.GetPriority(); priority != 0 {
				return priority
			}
			return 0
		})
		for _, binding := range ordered {
			if binding.Plugin == nil {
				return nil, fmt.Errorf(
					"response binding has nil plugin (factory=%q resource=%s/%s)",
					binding.factoryName,
					binding.Provenance.Kind,
					binding.Provenance.ID,
				)
			}
			responseSpec, known := responseFactoryRegistry[binding.factoryName]
			if !known {
				if hasResponseCallbacks(binding.Plugin) {
					return nil, fmt.Errorf(
						"response callback is undeclared (factory=%q resource=%s/%s)",
						binding.factoryName,
						binding.Provenance.Kind,
						binding.Provenance.ID,
					)
				}
				continue
			}
			phases := responseSpec.mask
			if responseSpec.configAware {
				describer, ok := binding.Plugin.Config().(base.BindingPhaseDescriber)
				if !ok {
					return nil, fmt.Errorf(
						"factory %q requires a binding phase descriptor (resource=%s/%s)",
						binding.factoryName,
						binding.Provenance.Kind,
						binding.Provenance.ID,
					)
				}
				descriptor, err := describer.DescribeBindingPhases()
				if err != nil {
					return nil, fmt.Errorf("factory %q descriptor: %w", binding.factoryName, err)
				}
				if err := validateBindingPhaseDescriptor(binding.factoryName, descriptor); err != nil {
					return nil, err
				}
				resolved := requestStageRegistry[binding.factoryName]
				if descriptor.RequestStage != "" {
					resolved.Stage, err = parseRequestStage(descriptor.RequestStage)
					if err != nil {
						return nil, err
					}
				}
				if resolved.Stage != binding.Stage {
					return nil, fmt.Errorf(
						"factory %q stage disagreement: binding=%d descriptor=%d (resource=%s/%s)",
						binding.factoryName,
						binding.Stage,
						resolved.Stage,
						binding.Provenance.Kind,
						binding.Provenance.ID,
					)
				}
				if descriptor.Header {
					phases |= ResponsePhaseHeader
				}
				if descriptor.BufferedBody {
					phases |= ResponsePhaseBufferedBody
				}
			}
			if err := validateResponseCallbacks(binding, phases); err != nil {
				return nil, err
			}
			if phases == 0 {
				continue
			}
			result = append(result, ResponseBinding{
				Plugin: binding.Plugin, Scope: binding.Scope, Provenance: binding.Provenance,
				Phases: phases, factoryKey: binding.factoryName,
			})
		}
	}
	return result, nil
}

func hasResponseCallbacks(plugin Plugin) bool {
	_, header := plugin.(base.HeaderFilterPlugin)
	_, body := plugin.(base.BufferedBodyFilterPlugin)
	_, store := plugin.(base.FinalResponseStorePlugin)
	return header || body || store
}

func validateResponseCallbacks(binding Binding, phases ResponsePhaseMask) error {
	if phases&ResponsePhaseHeader != 0 {
		if _, ok := binding.Plugin.(base.HeaderFilterPlugin); !ok {
			return fmt.Errorf(
				"factory %q declares header filter without callback (resource=%s/%s)",
				binding.factoryName,
				binding.Provenance.Kind,
				binding.Provenance.ID,
			)
		}
	}
	if phases&ResponsePhaseBufferedBody != 0 {
		if _, ok := binding.Plugin.(base.BufferedBodyFilterPlugin); !ok {
			return fmt.Errorf(
				"factory %q declares buffered body filter without callback (resource=%s/%s)",
				binding.factoryName,
				binding.Provenance.Kind,
				binding.Provenance.ID,
			)
		}
	}
	if phases&ResponsePhaseFinalStore != 0 {
		if _, ok := binding.Plugin.(base.FinalResponseStorePlugin); !ok {
			return fmt.Errorf(
				"factory %q declares final response store without callback (resource=%s/%s)",
				binding.factoryName,
				binding.Provenance.Kind,
				binding.Provenance.ID,
			)
		}
	}
	return nil
}
