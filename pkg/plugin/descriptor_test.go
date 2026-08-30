package plugin

import (
	"slices"
	"testing"
)

func TestDescriptorForFactoryUsesRegistryPhaseAndScope(t *testing.T) {
	descriptor, err := DescriptorForFactory("request-id")
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Priority != 0 || descriptor.Factory != "request-id" ||
		!slices.Equal(descriptor.Phases, []Phase{PhaseRewrite}) ||
		!slices.Equal(descriptor.Scopes, []Scope{ScopeGlobal, ScopeRoute, ScopeConsumer}) {
		t.Fatalf("descriptor = %#v", descriptor)
	}

	credentialDescriptor, err := DescriptorForFactory("key-auth")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(credentialDescriptor.Scopes, []Scope{ScopeGlobal, ScopeRoute}) {
		t.Fatalf("credential descriptor = %#v", credentialDescriptor)
	}
}

func TestDescriptorForFactoryRejectsUnknownFactory(t *testing.T) {
	if _, err := DescriptorForFactory("unknown"); err == nil {
		t.Fatal("DescriptorForFactory() error = nil")
	}
}

func TestDescriptorConditionalTerminalUsesRegistryFact(t *testing.T) {
	for _, factory := range []string{"proxy-rewrite", "real-ip"} {
		descriptor, descriptorErr := DescriptorForFactory(factory)
		if descriptorErr != nil {
			t.Fatal(descriptorErr)
		}
		if hasConditionalTerminalBinding([]Binding{{Descriptor: descriptor}}) {
			t.Fatalf("factory %q unexpectedly triggers bounded response planning", factory)
		}
	}
	for _, factory := range []string{"request-id", "limit-count"} {
		descriptor, descriptorErr := DescriptorForFactory(factory)
		if descriptorErr != nil {
			t.Fatal(descriptorErr)
		}
		if !hasConditionalTerminalBinding([]Binding{{Descriptor: descriptor}}) {
			t.Fatalf("factory %q did not trigger bounded response planning", factory)
		}
	}
}

func TestRegistryConditionalTerminalSetMatchesAudit(t *testing.T) {
	got := make([]string, 0)
	for _, entry := range Definitions() {
		if entry.ConditionalTerminal {
			got = append(got, entry.Factory)
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
