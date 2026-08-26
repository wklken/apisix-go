package route

import (
	"fmt"
	"slices"
	"strings"

	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/resource"
)

var productionPolicyAuthPlugins = [...]string{"key-auth", "jwt-auth", "basic-auth"}

func validateSecurityPluginPolicy(
	selection appconfig.ProfileSelection,
	pluginConfigs map[string]resource.PluginConfig,
	source string,
) error {
	if selection.Security != appconfig.SecurityStrict {
		return nil
	}

	for _, name := range productionPolicyAuthPlugins {
		config, ok := pluginConfigs[name]
		if !ok {
			continue
		}

		normalized, metadata, err := parsePluginMetadata(config)
		if err != nil {
			return fmt.Errorf("plugin %q from %s metadata: %w", name, policySource(source), err)
		}
		if metadata.disabled {
			continue
		}

		values, ok := normalized.(map[string]any)
		if !ok {
			return fmt.Errorf(
				"plugin %q from %s must be an object with hide_credentials: true",
				name,
				policySource(source),
			)
		}
		hideCredentials, ok := values["hide_credentials"].(bool)
		if !ok || !hideCredentials {
			return fmt.Errorf(
				"plugin %q from %s must set hide_credentials: true",
				name,
				policySource(source),
			)
		}

		if name != "jwt-auth" {
			continue
		}
		claims, ok := values["claims_to_verify"]
		if !ok {
			return fmt.Errorf(
				"plugin %q from %s must include literal exp in claims_to_verify",
				name,
				policySource(source),
			)
		}
		if err := validateJWTClaimsToVerify(claims); err != nil {
			return fmt.Errorf(
				"plugin %q from %s: %w",
				name,
				policySource(source),
				err,
			)
		}
	}
	return nil
}

func validateJWTClaimsToVerify(claims any) error {
	containsExp := false
	switch values := claims.(type) {
	case []string:
		containsExp = slices.Contains(values, "exp")
	case []any:
		for _, claim := range values {
			literal, ok := claim.(string)
			if ok && literal == "exp" {
				containsExp = true
				break
			}
		}
	default:
		return fmt.Errorf("claims_to_verify must be an array containing literal exp")
	}
	if !containsExp {
		return fmt.Errorf("claims_to_verify must include literal exp")
	}
	return nil
}

func validateSecurityUpstreamPolicy(
	selection appconfig.ProfileSelection,
	upstream resource.Upstream,
	source string,
) error {
	if selection.Security != appconfig.SecurityStrict || !securityPolicyUpstreamUsesTLS(upstream) {
		return nil
	}
	if upstream.TLS == nil || !upstream.TLS.Verify {
		return fmt.Errorf(
			"upstream scheme %q from %s requires tls.verify: true",
			upstream.Scheme,
			policySource(source),
		)
	}
	return nil
}

func securityPolicyUpstreamUsesTLS(upstream resource.Upstream) bool {
	return upstreamUsesTLS(upstream) ||
		(strings.EqualFold(upstream.Scheme, "kafka") && upstream.TLS != nil)
}

func validateSecurityMaterializedPluginSources(
	selection appconfig.ProfileSelection,
	sources []materializedPluginSource,
	routeID string,
) error {
	for _, source := range sources {
		provenance := fmt.Sprintf("%s %q for route %q", source.provenance.Kind, source.provenance.ID, routeID)
		if err := validateSecurityPluginPolicy(
			selection,
			map[string]resource.PluginConfig{source.name: source.config},
			provenance,
		); err != nil {
			return fmt.Errorf("route %q: %w", routeID, err)
		}
	}
	return nil
}

func validateSecurityGlobalRulePolicy(
	selection appconfig.ProfileSelection,
	globalRules []resource.GlobalRule,
	routeID string,
) error {
	for _, rule := range globalRules {
		source := fmt.Sprintf("global_rule %q", rule.ID)
		if routeID != "" {
			source = fmt.Sprintf("%s for route %q", source, routeID)
		}
		if err := validateSecurityPluginPolicy(selection, rule.Plugins, source); err != nil {
			if routeID == "" {
				return err
			}
			return fmt.Errorf("route %q: %w", routeID, err)
		}
	}
	return nil
}

func policySource(source string) string {
	if strings.TrimSpace(source) == "" {
		return "configuration"
	}
	return source
}
