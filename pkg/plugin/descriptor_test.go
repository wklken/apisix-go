package plugin

import (
	"slices"
	"testing"

	"github.com/wklken/apisix-go/pkg/capability"
)

func TestDescriptorForFactoryUsesManifestPhasePriorityScope(t *testing.T) {
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := DescriptorForFactory(manifest, "request-id")
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := manifest.Plugin("request-id")
	if descriptor.Priority != entry.Priority || descriptor.Factory != "request-id" ||
		!slices.Equal(descriptor.Phases, []Phase{PhaseRewrite}) ||
		!slices.Equal(entry.Scopes, []string{"global_rule", "route", "service", "consumer", "consumer_group"}) ||
		!slices.Equal(descriptor.Scopes, []Scope{ScopeGlobal, ScopeRoute, ScopeConsumer}) {
		t.Fatalf("descriptor = %#v, manifest = %#v", descriptor, entry)
	}

	credentialDescriptor, err := DescriptorForFactory(manifest, "key-auth")
	if err != nil {
		t.Fatal(err)
	}
	credentialEntry, _ := manifest.Plugin("key-auth")
	if !slices.Equal(credentialEntry.Scopes, []string{"global_rule", "route", "service"}) ||
		!slices.Equal(credentialDescriptor.Scopes, []Scope{ScopeGlobal, ScopeRoute}) {
		t.Fatalf("credential descriptor = %#v, manifest = %#v", credentialDescriptor, credentialEntry)
	}
}

func TestDescriptorForFactoryRejectsFactoryOutsideEntry(t *testing.T) {
	manifest, err := capability.Parse([]byte(`
schema_version: 1
target:
  name: apisix-3.17
  version: 3.17.0
  source_commit: 9ef2ecab67f652d38365049613610ef649bb4ad0
  image: apache/apisix:3.17.0
plugins:
  - name: request-id
    implementation: request-id
    namespace: apisix
    domains: [http]
    apisix_default: true
    factories:
      - key: alias-only
        import_path: example.invalid/request-id
        import_alias: request_id
        constructor: New
    phases: [rewrite]
    priority: 100
    scopes: [route]
    instance_scope: route
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DescriptorForFactory(manifest, "request-id"); err == nil {
		t.Fatal("DescriptorForFactory() error = nil")
	}
}

func TestDescriptorConditionalTerminalUsesAuditedManifestFact(t *testing.T) {
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, factory := range []string{"proxy-rewrite", "real-ip"} {
		descriptor, descriptorErr := DescriptorForFactory(manifest, factory)
		if descriptorErr != nil {
			t.Fatal(descriptorErr)
		}
		if hasConditionalTerminalBinding([]Binding{{Descriptor: descriptor}}) {
			t.Fatalf("factory %q unexpectedly triggers bounded response planning", factory)
		}
	}
	for _, factory := range []string{"request-id", "limit-count"} {
		descriptor, descriptorErr := DescriptorForFactory(manifest, factory)
		if descriptorErr != nil {
			t.Fatal(descriptorErr)
		}
		if !hasConditionalTerminalBinding([]Binding{{Descriptor: descriptor}}) {
			t.Fatalf("factory %q did not trigger bounded response planning", factory)
		}
	}
}

func TestManifestConditionalTerminalSetMatchesAudit(t *testing.T) {
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0)
	for _, entry := range manifest.Plugins {
		if entry.ConditionalTerminal {
			got = append(got, entry.Name)
		}
	}
	slices.Sort(got)
	want := []string{
		"acl", "ai-aliyun-content-moderation", "ai-aws-content-moderation", "ai-prompt-decorator",
		"ai-prompt-guard", "ai-prompt-template", "ai-proxy", "ai-proxy-multi", "ai-rag",
		"ai-rate-limiting", "ai-request-rewrite", "api-breaker", "authz-casbin", "authz-casdoor",
		"authz-keycloak", "aws-lambda", "azure-functions", "basic-auth", "cas-auth", "chaitin-waf",
		"client-control", "consumer-restriction", "cors", "csrf", "degraphql", "dingtalk-auth",
		"fault-injection", "feishu-auth", "forward-auth", "graphql-limit-count", "graphql-proxy-cache",
		"grpc-transcode", "grpc-web", "hmac-auth", "ip-restriction", "jwe-decrypt", "jwt-auth",
		"key-auth", "ldap-auth", "limit-conn", "limit-count", "limit-req", "mcp-bridge", "mocking",
		"multi-auth", "oas-validator", "opa", "openfunction", "openid-connect", "openwhisk",
		"proxy-cache", "public-api", "redirect", "referer-restriction", "request-id",
		"request-validation", "saml-auth", "serverless-post-function", "serverless-pre-function",
		"traffic-split", "ua-restriction", "uri-blocker", "wolf-rbac", "workflow",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("conditional-terminal factories = %v, want %v", got, want)
	}
}
