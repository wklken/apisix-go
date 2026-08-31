package plugin

import (
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	appconfig "github.com/wklken/apisix-go/pkg/config"
)

// apisix317DefaultHTTPPlugins mirrors apisix/cli/config.lua at
// 9ef2ecab67f652d38365049613610ef649bb4ad0.
var apisix317DefaultHTTPPlugins = []string{
	"real-ip",
	"ai",
	"client-control",
	"proxy-buffering",
	"proxy-control",
	"request-id",
	"zipkin",
	"ext-plugin-pre-req",
	"fault-injection",
	"mocking",
	"serverless-pre-function",
	"cors",
	"ip-restriction",
	"ua-restriction",
	"referer-restriction",
	"csrf",
	"uri-blocker",
	"request-validation",
	"chaitin-waf",
	"multi-auth",
	"openid-connect",
	"saml-auth",
	"cas-auth",
	"authz-casbin",
	"authz-casdoor",
	"wolf-rbac",
	"ldap-auth",
	"hmac-auth",
	"basic-auth",
	"jwt-auth",
	"jwe-decrypt",
	"key-auth",
	"dingtalk-auth",
	"feishu-auth",
	"acl",
	"consumer-restriction",
	"attach-consumer-label",
	"forward-auth",
	"opa",
	"authz-keycloak",
	"data-mask",
	"proxy-cache",
	"body-transformer",
	"ai-prompt-template",
	"ai-prompt-decorator",
	"ai-prompt-guard",
	"ai-rag",
	"ai-rate-limiting",
	"ai-proxy-multi",
	"ai-proxy",
	"ai-aws-content-moderation",
	"ai-aliyun-content-moderation",
	"proxy-mirror",
	"graphql-proxy-cache",
	"proxy-rewrite",
	"workflow",
	"api-breaker",
	"graphql-limit-count",
	"limit-conn",
	"limit-count",
	"limit-req",
	"gzip",
	"traffic-label",
	"traffic-split",
	"redirect",
	"response-rewrite",
	"oas-validator",
	"mcp-bridge",
	"degraphql",
	"kafka-proxy",
	"grpc-transcode",
	"grpc-web",
	"http-dubbo",
	"public-api",
	"prometheus",
	"datadog",
	"lago",
	"loki-logger",
	"elasticsearch-logger",
	"echo",
	"loggly",
	"http-logger",
	"splunk-hec-logging",
	"skywalking-logger",
	"google-cloud-logging",
	"sls-logger",
	"tcp-logger",
	"kafka-logger",
	"rocketmq-logger",
	"syslog",
	"udp-logger",
	"file-logger",
	"clickhouse-logger",
	"tencent-cloud-cls",
	"inspect",
	"example-plugin",
	"aws-lambda",
	"azure-functions",
	"openwhisk",
	"openfunction",
	"serverless-post-function",
	"ext-plugin-post-req",
	"ext-plugin-post-resp",
	"ai-request-rewrite",
}

func TestDefaultHTTPPluginsMatchAPISIX317ImplementedFactories(t *testing.T) {
	want := make([]string, 0, len(apisix317DefaultHTTPPlugins))
	for _, name := range apisix317DefaultHTTPPlugins {
		registered, implemented := pluginRegistry[name]
		if !implemented || registered.domain != DomainHTTP {
			continue
		}
		want = append(want, name)
	}

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate registry contract test")
	}
	defaultPath := filepath.Join(filepath.Dir(currentFile), "..", "..", "conf", "config-default.yaml")
	effective, err := appconfig.LoadEffective(appconfig.LoadRequest{DefaultPath: defaultPath})
	if err != nil {
		t.Fatalf("load default config: %v", err)
	}
	if !slices.Equal(effective.Config.Plugins, want) {
		t.Fatalf("default HTTP plugins = %q, want APISIX 3.17 implemented intersection %q",
			effective.Config.Plugins, want)
	}
}

func TestDefinitionsCoverEveryFactoryInStableOrder(t *testing.T) {
	definitions := Definitions()
	if len(definitions) != len(pluginRegistry) {
		t.Fatalf("definitions = %d, registry = %d", len(definitions), len(pluginRegistry))
	}
	factories := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		if _, ok := pluginRegistry[definition.Factory]; !ok {
			t.Fatalf("definition %q has no constructor", definition.Factory)
		}
		factories = append(factories, definition.Factory)
	}
	if !slices.IsSorted(factories) {
		t.Fatalf("definitions are not sorted: %v", factories)
	}
}

func TestRegistryMatchesPluginOwnedRuntimeFacts(t *testing.T) {
	for key, registered := range pluginRegistry {
		if registered.create == nil {
			t.Fatalf("factory %s is nil", key)
		}
		instance := registered.create()
		if instance == nil {
			t.Fatalf("factory %s returned nil", key)
		}
		if err := instance.Init(); err != nil {
			t.Fatalf("%s Init: %v", key, err)
		}
		wantName := key
		if alias, ok := pluginNameAliases[key]; ok {
			wantName = alias
		}
		if instance.GetName() != wantName {
			t.Fatalf("%s implementation name = %q, want %q", key, instance.GetName(), wantName)
		}

		descriptor, err := DescriptorForFactory(key)
		if err != nil {
			t.Fatalf("factory %s descriptor: %v", key, err)
		}
		if descriptor.Factory != key {
			t.Fatalf("factory %s descriptor identity = %q", key, descriptor.Factory)
		}
	}
}

func TestDefinitionReturnsDefensiveCopies(t *testing.T) {
	left, ok := DefinitionForFactory("request-id")
	if !ok {
		t.Fatal("request-id definition is missing")
	}
	left.Phases[0] = PhaseLog
	left.Scopes[0] = ScopeSystem
	right, _ := DefinitionForFactory("request-id")
	if right.Phases[0] != PhaseRewrite || right.Scopes[0] != ScopeGlobal {
		t.Fatalf("registry definition was mutated: %#v", right)
	}
}
