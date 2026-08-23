package route

import (
	"fmt"
	"strings"
	"testing"

	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/resource"
)

var (
	compatPolicySelection = appconfig.ProfileSelection{
		Compatibility: appconfig.CompatibilityAPISIX317,
		Security:      appconfig.SecurityCompat,
	}
	strictPolicySelection = appconfig.ProfileSelection{
		Compatibility: appconfig.CompatibilityAPISIX317,
		Security:      appconfig.SecurityStrict,
	}
	qualificationPolicySelection = appconfig.ProfileSelection{
		Compatibility: appconfig.CompatibilityAPISIX317,
		Security:      appconfig.SecurityCompat,
		Qualification: appconfig.QualificationHTTPDataPlaneV1,
	}
)

func TestProductionPolicyPluginSecurityAxis(t *testing.T) {
	tests := []struct {
		name       string
		selection  appconfig.ProfileSelection
		plugins    map[string]resource.PluginConfig
		wantErr    bool
		wantFields []string
	}{
		{
			name:      "HTTP qualification preserves compatibility security defaults",
			selection: qualificationPolicySelection,
			plugins:   map[string]resource.PluginConfig{"key-auth": map[string]any{}},
		},
		{
			name:      "key-auth must hide credentials",
			selection: strictPolicySelection,
			plugins:   map[string]resource.PluginConfig{"key-auth": map[string]any{}},
			wantErr:   true, wantFields: []string{"key-auth", "hide_credentials"},
		},
		{
			name:      "key-auth false must hide credentials",
			selection: strictPolicySelection,
			plugins:   map[string]resource.PluginConfig{"key-auth": map[string]any{"hide_credentials": false}},
			wantErr:   true, wantFields: []string{"key-auth", "hide_credentials"},
		},
		{
			name:      "basic-auth must hide credentials",
			selection: strictPolicySelection,
			plugins:   map[string]resource.PluginConfig{"basic-auth": map[string]any{}},
			wantErr:   true, wantFields: []string{"basic-auth", "hide_credentials"},
		},
		{
			name:      "basic-auth false must hide credentials",
			selection: strictPolicySelection,
			plugins:   map[string]resource.PluginConfig{"basic-auth": map[string]any{"hide_credentials": false}},
			wantErr:   true, wantFields: []string{"basic-auth", "hide_credentials"},
		},
		{
			name:      "jwt-auth must hide credentials",
			selection: strictPolicySelection,
			plugins:   map[string]resource.PluginConfig{"jwt-auth": map[string]any{"claims_to_verify": []any{"exp"}}},
			wantErr:   true, wantFields: []string{"jwt-auth", "hide_credentials"},
		},
		{
			name:      "jwt-auth requires exp",
			selection: strictPolicySelection,
			plugins:   map[string]resource.PluginConfig{"jwt-auth": map[string]any{"hide_credentials": true}},
			wantErr:   true, wantFields: []string{"jwt-auth", "claims_to_verify", "exp"},
		},
		{
			name:      "jwt-auth false must hide credentials",
			selection: strictPolicySelection,
			plugins: map[string]resource.PluginConfig{
				"jwt-auth": map[string]any{"hide_credentials": false, "claims_to_verify": []any{"exp"}},
			},
			wantErr: true, wantFields: []string{"jwt-auth", "hide_credentials"},
		},
		{
			name:      "jwt-auth nbf alone is not enough",
			selection: strictPolicySelection,
			plugins: map[string]resource.PluginConfig{
				"jwt-auth": map[string]any{"hide_credentials": true, "claims_to_verify": []any{"nbf"}},
			},
			wantErr: true, wantFields: []string{"jwt-auth", "claims_to_verify", "exp"},
		},
		{
			name:      "jwt-auth rejects non-array claims",
			selection: strictPolicySelection,
			plugins: map[string]resource.PluginConfig{
				"jwt-auth": map[string]any{"hide_credentials": true, "claims_to_verify": "exp"},
			},
			wantErr: true, wantFields: []string{"jwt-auth", "claims_to_verify", "exp"},
		},
		{
			name:      "jwt-auth accepts exp",
			selection: strictPolicySelection,
			plugins: map[string]resource.PluginConfig{
				"jwt-auth": map[string]any{"hide_credentials": true, "claims_to_verify": []any{"exp"}},
			},
		},
		{
			name:      "disabled auth config is inert",
			selection: strictPolicySelection,
			plugins: map[string]resource.PluginConfig{
				"key-auth": map[string]any{"_meta": map[string]any{"disable": true}},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSecurityPluginPolicy(test.selection, test.plugins, "route policy test")
			if test.wantErr {
				if err == nil {
					t.Fatal("validateSecurityPluginPolicy() error = nil, want rejection")
				}
				for _, field := range test.wantFields {
					if !strings.Contains(err.Error(), field) {
						t.Fatalf("error = %q, want field %q", err, field)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("validateSecurityPluginPolicy() error = %v, want nil", err)
			}
		})
	}
}

func TestProductionPolicyUpstreamSecurityAxis(t *testing.T) {
	tests := []struct {
		name       string
		selection  appconfig.ProfileSelection
		upstream   resource.Upstream
		wantErr    bool
		wantFields []string
	}{
		{
			name:      "HTTP qualification preserves compatibility TLS defaults",
			selection: qualificationPolicySelection,
			upstream:  resource.Upstream{Scheme: "https"},
		},
		{
			name:      "http upstream is allowed",
			selection: strictPolicySelection,
			upstream:  resource.Upstream{Scheme: "http"},
		},
		{
			name:       "https requires tls verify",
			selection:  strictPolicySelection,
			upstream:   resource.Upstream{Scheme: "https"},
			wantErr:    true,
			wantFields: []string{"https", "tls.verify"},
		},
		{
			name:       "https false verify is rejected",
			selection:  strictPolicySelection,
			upstream:   resource.Upstream{Scheme: "https", TLS: &resource.UpstreamTLS{Verify: false}},
			wantErr:    true,
			wantFields: []string{"https", "tls.verify"},
		},
		{
			name:      "https true verify is allowed",
			selection: strictPolicySelection,
			upstream:  resource.Upstream{Scheme: "https", TLS: &resource.UpstreamTLS{Verify: true}},
		},
		{
			name:       "grpcs requires tls verify",
			selection:  strictPolicySelection,
			upstream:   resource.Upstream{Scheme: "grpcs", TLS: &resource.UpstreamTLS{Verify: false}},
			wantErr:    true,
			wantFields: []string{"grpcs", "tls.verify"},
		},
		{
			name:       "grpcs without tls is rejected",
			selection:  strictPolicySelection,
			upstream:   resource.Upstream{Scheme: "grpcs"},
			wantErr:    true,
			wantFields: []string{"grpcs", "tls.verify"},
		},
		{
			name:       "strict Kafka TLS false verify is rejected",
			selection:  strictPolicySelection,
			upstream:   resource.Upstream{Scheme: "kafka", TLS: &resource.UpstreamTLS{Verify: false}},
			wantErr:    true,
			wantFields: []string{"kafka", "tls.verify"},
		},
		{
			name:      "strict Kafka TLS true verify is allowed",
			selection: strictPolicySelection,
			upstream:  resource.Upstream{Scheme: "kafka", TLS: &resource.UpstreamTLS{Verify: true}},
		},
		{
			name:      "compatibility Kafka TLS false verify is allowed",
			selection: compatPolicySelection,
			upstream:  resource.Upstream{Scheme: "kafka", TLS: &resource.UpstreamTLS{Verify: false}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSecurityUpstreamPolicy(test.selection, test.upstream, "upstream policy test")
			if test.wantErr {
				if err == nil {
					t.Fatal("validateSecurityUpstreamPolicy() error = nil, want rejection")
				}
				for _, field := range test.wantFields {
					if !strings.Contains(err.Error(), field) {
						t.Fatalf("error = %q, want field %q", err, field)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("validateSecurityUpstreamPolicy() error = %v, want nil", err)
			}
		})
	}
}

func TestProductionPolicyBuildStrictEnforcesSecurityAxis(t *testing.T) {
	ensureRouteStore(t)

	t.Run("unsafe service jwt is rejected", func(t *testing.T) {
		setProductionPolicySelection(t, strictPolicySelection, "jwt-auth")
		const serviceID = "production-policy-unsafe-service"
		const routeID = "production-policy-service-route"
		putHTTPAllowlistResource(t, "services", serviceID, fmt.Appendf(nil,
			`{"id":%q,"plugins":{"jwt-auth":{"hide_credentials":true,"claims_to_verify":["nbf"]}}}`,
			serviceID,
		))
		putRouteResource(t, routeID, fmt.Appendf(nil,
			`{"id":%q,"uri":"/production-policy-service","service_id":%q,"upstream":{"nodes":{"127.0.0.1:1":1}}}`,
			routeID, serviceID,
		))

		builder := NewBuilder(nil)
		t.Cleanup(builder.Stop)
		handler, err := builder.BuildStrict()
		if err == nil || handler != nil {
			t.Fatalf("BuildStrict() = (%T, %v), want service policy rejection", handler, err)
		}
		for _, field := range []string{routeID, serviceID, "jwt-auth", "claims_to_verify", "exp"} {
			if !strings.Contains(err.Error(), field) {
				t.Fatalf("BuildStrict() error = %q, want %q", err, field)
			}
		}
	})

	t.Run("safe route jwt overrides unsafe service jwt", func(t *testing.T) {
		setProductionPolicySelection(t, strictPolicySelection, "jwt-auth")
		const serviceID = "production-policy-override-service"
		const routeID = "production-policy-override-route"
		putHTTPAllowlistResource(t, "services", serviceID, fmt.Appendf(nil,
			`{"id":%q,"plugins":{"jwt-auth":{"hide_credentials":true,"claims_to_verify":["nbf"]}}}`,
			serviceID,
		))
		putRouteResource(t, routeID, fmt.Appendf(
			nil,
			`{"id":%q,"uri":"/production-policy-override","service_id":%q,"plugins":{"jwt-auth":{"hide_credentials":true,"claims_to_verify":["exp"]}},"upstream":{"nodes":{"127.0.0.1:1":1}}}`,
			routeID,
			serviceID,
		))

		builder := NewBuilder(nil)
		t.Cleanup(builder.Stop)
		handler, err := builder.BuildStrict()
		if err != nil || handler == nil {
			t.Fatalf("BuildStrict() = (%T, %v), want safe route winner", handler, err)
		}
	})

	t.Run("unsafe global rule is rejected", func(t *testing.T) {
		setProductionPolicySelection(t, strictPolicySelection, "jwt-auth")
		const ruleID = "production-policy-unsafe-global"
		const routeID = "production-policy-global-route"
		putHTTPAllowlistResource(t, "global_rules", ruleID, fmt.Appendf(nil,
			`{"id":%q,"plugins":{"jwt-auth":{"hide_credentials":true,"claims_to_verify":["nbf"]}}}`,
			ruleID,
		))
		putRouteResource(t, routeID, fmt.Appendf(nil,
			`{"id":%q,"uri":"/production-policy-global","upstream":{"nodes":{"127.0.0.1:1":1}}}`,
			routeID,
		))

		builder := NewBuilder(nil)
		t.Cleanup(builder.Stop)
		handler, err := builder.BuildStrict()
		if err == nil || handler != nil {
			t.Fatalf("BuildStrict() = (%T, %v), want global policy rejection", handler, err)
		}
		for _, field := range []string{routeID, ruleID, "jwt-auth", "claims_to_verify", "exp"} {
			if !strings.Contains(err.Error(), field) {
				t.Fatalf("BuildStrict() error = %q, want %q", err, field)
			}
		}
	})

	t.Run("unsafe https upstream is rejected after upstream resolution", func(t *testing.T) {
		setProductionPolicySelection(t, strictPolicySelection)
		const upstreamID = "production-policy-unsafe-upstream"
		const routeID = "production-policy-upstream-route"
		putHTTPAllowlistResource(t, "upstreams", upstreamID, fmt.Appendf(nil,
			`{"id":%q,"scheme":"https","nodes":{"127.0.0.1:443":1}}`, upstreamID,
		))
		putRouteResource(t, routeID, fmt.Appendf(nil,
			`{"id":%q,"uri":"/production-policy-upstream","upstream_id":%q}`,
			routeID, upstreamID,
		))

		builder := NewBuilder(nil)
		t.Cleanup(builder.Stop)
		handler, err := builder.BuildStrict()
		if err == nil || handler != nil {
			t.Fatalf("BuildStrict() = (%T, %v), want upstream policy rejection", handler, err)
		}
		for _, field := range []string{routeID, upstreamID, "https", "tls.verify"} {
			if !strings.Contains(err.Error(), field) {
				t.Fatalf("BuildStrict() error = %q, want %q", err, field)
			}
		}
	})

	t.Run("compatibility profile retains auth and upstream defaults", func(t *testing.T) {
		setProductionPolicySelection(t, qualificationPolicySelection, "jwt-auth")
		const upstreamID = "production-policy-compat-upstream"
		const routeID = "production-policy-compat-route"
		putHTTPAllowlistResource(t, "upstreams", upstreamID, fmt.Appendf(nil,
			`{"id":%q,"scheme":"https","nodes":{"127.0.0.1:443":1}}`, upstreamID,
		))
		putRouteResource(t, routeID, fmt.Appendf(nil,
			`{"id":%q,"uri":"/production-policy-compat","plugins":{"jwt-auth":{}},"upstream_id":%q}`,
			routeID, upstreamID,
		))

		builder := NewBuilder(nil)
		t.Cleanup(builder.Stop)
		handler, err := builder.BuildStrict()
		if err != nil || handler == nil {
			t.Fatalf("BuildStrict() = (%T, %v), want compatibility defaults preserved", handler, err)
		}
	})
}

func setProductionPolicySelection(
	t *testing.T,
	selection appconfig.ProfileSelection,
	plugins ...string,
) {
	t.Helper()
	previous := appconfig.GlobalConfig
	appconfig.GlobalConfig = &appconfig.Config{
		CompatibilityTarget:  selection.Compatibility,
		SecurityProfile:      selection.Security,
		QualificationProfile: selection.Qualification,
		Plugins:              plugins,
	}
	t.Cleanup(func() { appconfig.GlobalConfig = previous })
}
