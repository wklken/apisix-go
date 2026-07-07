package plugin

import (
	"testing"

	"github.com/wklken/apisix-go/pkg/util"
)

func TestNewOpentelemetryUsesOfficialPluginName(t *testing.T) {
	p := New("opentelemetry")
	if p == nil {
		t.Fatal(`New("opentelemetry") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "opentelemetry" {
		t.Fatalf("plugin name = %q, want opentelemetry", got)
	}
	if got := p.GetPriority(); got != 12009 {
		t.Fatalf("priority = %d, want 12009", got)
	}
	if err := util.Validate(map[string]any{}, p.GetSchema()); err != nil {
		t.Fatalf("empty official opentelemetry config should validate: %v", err)
	}
}

func TestNewOtelAliasStillWorks(t *testing.T) {
	p := New("otel")
	if p == nil {
		t.Fatal(`New("otel") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "opentelemetry" {
		t.Fatalf("plugin name = %q, want opentelemetry", got)
	}
}

func TestNewProxyControlUsesOfficialPluginName(t *testing.T) {
	p := New("proxy-control")
	if p == nil {
		t.Fatal(`New("proxy-control") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "proxy-control" {
		t.Fatalf("plugin name = %q, want proxy-control", got)
	}
	if got := p.GetPriority(); got != 21990 {
		t.Fatalf("priority = %d, want 21990", got)
	}
	if err := util.Validate(map[string]any{}, p.GetSchema()); err != nil {
		t.Fatalf("empty proxy-control config should validate: %v", err)
	}
}

func TestNewProxyBufferingUsesOfficialPluginName(t *testing.T) {
	p := New("proxy-buffering")
	if p == nil {
		t.Fatal(`New("proxy-buffering") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "proxy-buffering" {
		t.Fatalf("plugin name = %q, want proxy-buffering", got)
	}
	if got := p.GetPriority(); got != 21991 {
		t.Fatalf("priority = %d, want 21991", got)
	}
	if err := util.Validate(map[string]any{}, p.GetSchema()); err != nil {
		t.Fatalf("empty proxy-buffering config should validate: %v", err)
	}
}

func TestNewTrafficLabelUsesOfficialPluginName(t *testing.T) {
	p := New("traffic-label")
	if p == nil {
		t.Fatal(`New("traffic-label") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "traffic-label" {
		t.Fatalf("plugin name = %q, want traffic-label", got)
	}
	if got := p.GetPriority(); got != 967 {
		t.Fatalf("priority = %d, want 967", got)
	}
	config := map[string]any{
		"rules": []any{
			map[string]any{
				"actions": []any{
					map[string]any{"set_headers": map[string]any{"X-Bucket": "blue"}},
				},
			},
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("traffic-label config should validate: %v", err)
	}
}

func TestNewTrafficSplitAcceptsMatchVars(t *testing.T) {
	p := New("traffic-split")
	if p == nil {
		t.Fatal(`New("traffic-split") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "traffic-split" {
		t.Fatalf("plugin name = %q, want traffic-split", got)
	}
	if got := p.GetPriority(); got != 966 {
		t.Fatalf("priority = %d, want 966", got)
	}
	config := map[string]any{
		"rules": []any{
			map[string]any{
				"match": []any{
					map[string]any{
						"vars": []any{[]any{"http_x_stage", "==", "beta"}},
					},
				},
				"weighted_upstreams": []any{
					map[string]any{
						"weight": 1,
						"upstream": map[string]any{
							"type":   "roundrobin",
							"scheme": "http",
							"nodes": []any{
								map[string]any{"host": "beta.example.com", "port": 80, "weight": 1},
							},
						},
					},
				},
			},
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("traffic-split match vars config should validate: %v", err)
	}
}

func TestNewWorkflowUsesOfficialPluginName(t *testing.T) {
	p := New("workflow")
	if p == nil {
		t.Fatal(`New("workflow") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "workflow" {
		t.Fatalf("plugin name = %q, want workflow", got)
	}
	if got := p.GetPriority(); got != 1006 {
		t.Fatalf("priority = %d, want 1006", got)
	}
	config := map[string]any{
		"rules": []any{
			map[string]any{
				"actions": []any{
					[]any{"return", map[string]any{"code": 403}},
				},
			},
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("workflow config should validate: %v", err)
	}
}

func TestNewProxyCacheUsesOfficialPluginName(t *testing.T) {
	p := New("proxy-cache")
	if p == nil {
		t.Fatal(`New("proxy-cache") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "proxy-cache" {
		t.Fatalf("plugin name = %q, want proxy-cache", got)
	}
	if got := p.GetPriority(); got != 1085 {
		t.Fatalf("priority = %d, want 1085", got)
	}
	config := map[string]any{
		"cache_strategy":    "memory",
		"cache_zone":        "memory_cache",
		"cache_ttl":         10,
		"cache_key":         []any{"$host", "$request_uri"},
		"cache_method":      []any{"GET"},
		"cache_http_status": []any{200},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("proxy-cache config should validate: %v", err)
	}
}

func TestNewBodyTransformerUsesOfficialPluginName(t *testing.T) {
	p := New("body-transformer")
	if p == nil {
		t.Fatal(`New("body-transformer") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "body-transformer" {
		t.Fatalf("plugin name = %q, want body-transformer", got)
	}
	if got := p.GetPriority(); got != 1080 {
		t.Fatalf("priority = %d, want 1080", got)
	}
	config := map[string]any{
		"request": map[string]any{
			"input_format": "json",
			"template":     `{"name":"{{name}}"}`,
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("body-transformer config should validate: %v", err)
	}
}

func TestNewDegraphqlUsesOfficialPluginName(t *testing.T) {
	p := New("degraphql")
	if p == nil {
		t.Fatal(`New("degraphql") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "degraphql" {
		t.Fatalf("plugin name = %q, want degraphql", got)
	}
	if got := p.GetPriority(); got != 509 {
		t.Fatalf("priority = %d, want 509", got)
	}
	config := map[string]any{
		"query":          "{ getAllPokemon { key } }",
		"variables":      []any{"pokemon"},
		"operation_name": "GetPokemon",
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("degraphql config should validate: %v", err)
	}
}

func TestNewGraphQLLimitCountUsesOfficialPluginName(t *testing.T) {
	p := New("graphql-limit-count")
	if p == nil {
		t.Fatal(`New("graphql-limit-count") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "graphql-limit-count" {
		t.Fatalf("plugin name = %q, want graphql-limit-count", got)
	}
	if got := p.GetPriority(); got != 1004 {
		t.Fatalf("priority = %d, want 1004", got)
	}
	config := map[string]any{
		"count":         10,
		"time_window":   60,
		"key":           "remote_addr",
		"rejected_code": 429,
		"policy":        "local",
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("graphql-limit-count config should validate: %v", err)
	}
}

func TestNewLimitCountAcceptsRulesOnlyConfig(t *testing.T) {
	p := New("limit-count")
	if p == nil {
		t.Fatal(`New("limit-count") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"rules": []any{
			map[string]any{
				"count":         10,
				"time_window":   60,
				"key":           "$http_x_user",
				"header_prefix": "User",
			},
		},
		"policy": "local",
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("limit-count rules config should validate: %v", err)
	}
}

func TestNewLimitConnAcceptsRulesOnlyConfig(t *testing.T) {
	p := New("limit-conn")
	if p == nil {
		t.Fatal(`New("limit-conn") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"default_conn_delay": 0.1,
		"rules": []any{
			map[string]any{
				"conn":  1,
				"burst": 0,
				"key":   "$http_x_user",
			},
		},
		"policy": "local",
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("limit-conn rules config should validate: %v", err)
	}
}

func TestNewGraphQLProxyCacheUsesOfficialPluginName(t *testing.T) {
	p := New("graphql-proxy-cache")
	if p == nil {
		t.Fatal(`New("graphql-proxy-cache") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "graphql-proxy-cache" {
		t.Fatalf("plugin name = %q, want graphql-proxy-cache", got)
	}
	if got := p.GetPriority(); got != 1009 {
		t.Fatalf("priority = %d, want 1009", got)
	}
	config := map[string]any{
		"cache_strategy":     "memory",
		"cache_zone":         "memory_cache",
		"cache_ttl":          60,
		"consumer_isolation": false,
		"cache_set_cookie":   true,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("graphql-proxy-cache config should validate: %v", err)
	}
}

func TestNewGrpcWebUsesOfficialPluginName(t *testing.T) {
	p := New("grpc-web")
	if p == nil {
		t.Fatal(`New("grpc-web") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "grpc-web" {
		t.Fatalf("plugin name = %q, want grpc-web", got)
	}
	if got := p.GetPriority(); got != 505 {
		t.Fatalf("priority = %d, want 505", got)
	}
	config := map[string]any{
		"cors_allow_headers": "content-type,x-grpc-web,authorization",
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("grpc-web config should validate: %v", err)
	}
}

func TestNewGrpcTranscodeUsesOfficialPluginName(t *testing.T) {
	p := New("grpc-transcode")
	if p == nil {
		t.Fatal(`New("grpc-transcode") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "grpc-transcode" {
		t.Fatalf("plugin name = %q, want grpc-transcode", got)
	}
	if got := p.GetPriority(); got != 506 {
		t.Fatalf("priority = %d, want 506", got)
	}
	config := map[string]any{
		"proto_id": "echo-proto",
		"service":  "echo.EchoService",
		"method":   "Echo",
		"deadline": 100,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("grpc-transcode config should validate: %v", err)
	}
}

func TestNewBatchRequestsUsesOfficialPluginName(t *testing.T) {
	p := New("batch-requests")
	if p == nil {
		t.Fatal(`New("batch-requests") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "batch-requests" {
		t.Fatalf("plugin name = %q, want batch-requests", got)
	}
	if got := p.GetPriority(); got != 4010 {
		t.Fatalf("priority = %d, want 4010", got)
	}
	if err := util.Validate(map[string]any{}, p.GetSchema()); err != nil {
		t.Fatalf("empty batch-requests config should validate: %v", err)
	}
}

func TestNewPublicAPIUsesOfficialPluginName(t *testing.T) {
	p := New("public-api")
	if p == nil {
		t.Fatal(`New("public-api") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "public-api" {
		t.Fatalf("plugin name = %q, want public-api", got)
	}
	if got := p.GetPriority(); got != 501 {
		t.Fatalf("priority = %d, want 501", got)
	}
	config := map[string]any{"uri": "/apisix/batch-requests"}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("public-api config should validate: %v", err)
	}
}

func TestNewBrotliUsesOfficialPluginName(t *testing.T) {
	p := New("brotli")
	if p == nil {
		t.Fatal(`New("brotli") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "brotli" {
		t.Fatalf("plugin name = %q, want brotli", got)
	}
	if got := p.GetPriority(); got != 996 {
		t.Fatalf("priority = %d, want 996", got)
	}
	config := map[string]any{
		"types":        []any{"text/plain"},
		"min_length":   10,
		"comp_level":   6,
		"http_version": 1.1,
		"vary":         true,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("brotli config should validate: %v", err)
	}
}

func TestNewMultiAuthUsesOfficialPluginName(t *testing.T) {
	p := New("multi-auth")
	if p == nil {
		t.Fatal(`New("multi-auth") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "multi-auth" {
		t.Fatalf("plugin name = %q, want multi-auth", got)
	}
	if got := p.GetPriority(); got != 2600 {
		t.Fatalf("priority = %d, want 2600", got)
	}
	config := map[string]any{
		"auth_plugins": []any{
			map[string]any{"basic-auth": map[string]any{}},
			map[string]any{"key-auth": map[string]any{}},
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("multi-auth config should validate: %v", err)
	}
}

func TestNewAuthzCasbinUsesOfficialPluginName(t *testing.T) {
	p := New("authz-casbin")
	if p == nil {
		t.Fatal(`New("authz-casbin") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "authz-casbin" {
		t.Fatalf("plugin name = %q, want authz-casbin", got)
	}
	if got := p.GetPriority(); got != 2560 {
		t.Fatalf("priority = %d, want 2560", got)
	}
	config := map[string]any{
		"model":    "[request_definition]\nr = sub, obj, act",
		"policy":   "p, alice, /orders/123, GET",
		"username": "X-User",
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("authz-casbin config should validate: %v", err)
	}
}

func TestNewLDAPAuthUsesOfficialPluginName(t *testing.T) {
	p := New("ldap-auth")
	if p == nil {
		t.Fatal(`New("ldap-auth") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "ldap-auth" {
		t.Fatalf("plugin name = %q, want ldap-auth", got)
	}
	if got := p.GetPriority(); got != 2540 {
		t.Fatalf("priority = %d, want 2540", got)
	}
	config := map[string]any{
		"base_dn":  "dc=example,dc=org",
		"ldap_uri": "ldap://127.0.0.1:389",
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("ldap-auth config should validate: %v", err)
	}
}

func TestNewJWEDecryptUsesOfficialPluginName(t *testing.T) {
	p := New("jwe-decrypt")
	if p == nil {
		t.Fatal(`New("jwe-decrypt") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "jwe-decrypt" {
		t.Fatalf("plugin name = %q, want jwe-decrypt", got)
	}
	if got := p.GetPriority(); got != 2509 {
		t.Fatalf("priority = %d, want 2509", got)
	}
	config := map[string]any{
		"header":         "Authorization",
		"forward_header": "Authorization",
		"strict":         true,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("jwe-decrypt config should validate: %v", err)
	}
}

func TestNewCASAuthUsesOfficialPluginName(t *testing.T) {
	p := New("cas-auth")
	if p == nil {
		t.Fatal(`New("cas-auth") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "cas-auth" {
		t.Fatalf("plugin name = %q, want cas-auth", got)
	}
	if got := p.GetPriority(); got != 2597 {
		t.Fatalf("priority = %d, want 2597", got)
	}
	config := map[string]any{
		"idp_uri":          "https://cas.example.com",
		"cas_callback_uri": "/cas_callback",
		"logout_uri":       "/logout",
		"cookie": map[string]any{
			"secret": "12345678901234567890123456789012",
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("cas-auth config should validate: %v", err)
	}
}

func TestNewAuthzCasdoorUsesOfficialPluginName(t *testing.T) {
	p := New("authz-casdoor")
	if p == nil {
		t.Fatal(`New("authz-casdoor") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "authz-casdoor" {
		t.Fatalf("plugin name = %q, want authz-casdoor", got)
	}
	if got := p.GetPriority(); got != 2559 {
		t.Fatalf("priority = %d, want 2559", got)
	}
	config := map[string]any{
		"endpoint_addr": "https://door.example.com",
		"client_id":     "client-a",
		"client_secret": "secret-a",
		"callback_url":  "https://gateway.example.com/callback",
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("authz-casdoor config should validate: %v", err)
	}
}

func TestNewDingTalkAuthUsesOfficialPluginName(t *testing.T) {
	p := New("dingtalk-auth")
	if p == nil {
		t.Fatal(`New("dingtalk-auth") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "dingtalk-auth" {
		t.Fatalf("plugin name = %q, want dingtalk-auth", got)
	}
	if got := p.GetPriority(); got != 2430 {
		t.Fatalf("priority = %d, want 2430", got)
	}
	config := map[string]any{
		"app_key":      "app-key",
		"app_secret":   "app-secret",
		"secret":       "12345678",
		"redirect_uri": "https://login.dingtalk.com/oauth2/auth",
		"code_header":  "X-DingTalk-Code",
		"code_query":   "code",
		"timeout":      1000,
		"ssl_verify":   false,
		"secret_fallbacks": []any{
			"87654321",
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("dingtalk-auth config should validate: %v", err)
	}
}

func TestNewFeishuAuthUsesOfficialPluginName(t *testing.T) {
	p := New("feishu-auth")
	if p == nil {
		t.Fatal(`New("feishu-auth") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "feishu-auth" {
		t.Fatalf("plugin name = %q, want feishu-auth", got)
	}
	if got := p.GetPriority(); got != 2420 {
		t.Fatalf("priority = %d, want 2420", got)
	}
	config := map[string]any{
		"app_id":            "app-id",
		"app_secret":        "app-secret",
		"secret":            "12345678",
		"auth_redirect_uri": "https://gateway.example.com/callback",
		"redirect_uri":      "https://login.feishu.cn/oauth",
		"code_header":       "X-Feishu-Code",
		"code_query":        "code",
		"timeout":           1000,
		"ssl_verify":        false,
		"secret_fallbacks": []any{
			"87654321",
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("feishu-auth config should validate: %v", err)
	}
}

func TestNewAuthzKeycloakUsesOfficialPluginName(t *testing.T) {
	p := New("authz-keycloak")
	if p == nil {
		t.Fatal(`New("authz-keycloak") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "authz-keycloak" {
		t.Fatalf("plugin name = %q, want authz-keycloak", got)
	}
	if got := p.GetPriority(); got != 2000 {
		t.Fatalf("priority = %d, want 2000", got)
	}
	config := map[string]any{
		"token_endpoint": "https://keycloak.example.com/token",
		"client_id":      "apisix",
		"permissions":    []any{"orders"},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("authz-keycloak config should validate: %v", err)
	}
}

func TestNewOpenIDConnectUsesOfficialPluginName(t *testing.T) {
	p := New("openid-connect")
	if p == nil {
		t.Fatal(`New("openid-connect") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "openid-connect" {
		t.Fatalf("plugin name = %q, want openid-connect", got)
	}
	if got := p.GetPriority(); got != 2599 {
		t.Fatalf("priority = %d, want 2599", got)
	}
	config := map[string]any{
		"client_id":              "apisix",
		"client_secret":          "secret-a",
		"discovery":              "http://idp.example.com/.well-known/openid-configuration",
		"bearer_only":            true,
		"required_scopes":        []any{"read"},
		"introspection_endpoint": "http://idp.example.com/introspect",
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("openid-connect config should validate: %v", err)
	}
}

func TestNewLogRotateUsesOfficialPluginName(t *testing.T) {
	p := New("log-rotate")
	if p == nil {
		t.Fatal(`New("log-rotate") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "log-rotate" {
		t.Fatalf("plugin name = %q, want log-rotate", got)
	}
	if got := p.GetPriority(); got != 100 {
		t.Fatalf("priority = %d, want 100", got)
	}
	if err := util.Validate(map[string]any{}, p.GetSchema()); err != nil {
		t.Fatalf("empty log-rotate route config should validate: %v", err)
	}
}

func TestNewErrorLogLoggerUsesOfficialPluginName(t *testing.T) {
	p := New("error-log-logger")
	if p == nil {
		t.Fatal(`New("error-log-logger") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "error-log-logger" {
		t.Fatalf("plugin name = %q, want error-log-logger", got)
	}
	if got := p.GetPriority(); got != 1091 {
		t.Fatalf("priority = %d, want 1091", got)
	}
	if err := util.Validate(map[string]any{}, p.GetSchema()); err != nil {
		t.Fatalf("empty error-log-logger route config should validate: %v", err)
	}
}

func TestNewSkywalkingUsesOfficialPluginName(t *testing.T) {
	p := New("skywalking")
	if p == nil {
		t.Fatal(`New("skywalking") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "skywalking" {
		t.Fatalf("plugin name = %q, want skywalking", got)
	}
	if got := p.GetPriority(); got != 12010 {
		t.Fatalf("priority = %d, want 12010", got)
	}
	config := map[string]any{"sample_ratio": 1}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("skywalking config should validate: %v", err)
	}
}

func TestNewChaitinWAFUsesOfficialPluginName(t *testing.T) {
	p := New("chaitin-waf")
	if p == nil {
		t.Fatal(`New("chaitin-waf") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "chaitin-waf" {
		t.Fatalf("plugin name = %q, want chaitin-waf", got)
	}
	if got := p.GetPriority(); got != 2700 {
		t.Fatalf("priority = %d, want 2700", got)
	}
	config := map[string]any{
		"mode": "block",
		"config": map[string]any{
			"read_timeout": 1000,
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("chaitin-waf config should validate: %v", err)
	}
}

func TestNewGMUsesOfficialPluginName(t *testing.T) {
	p := New("gm")
	if p == nil {
		t.Fatal(`New("gm") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "gm" {
		t.Fatalf("plugin name = %q, want gm", got)
	}
	if got := p.GetPriority(); got != -43 {
		t.Fatalf("priority = %d, want -43", got)
	}
	if err := util.Validate(map[string]any{}, p.GetSchema()); err != nil {
		t.Fatalf("empty official gm config should validate: %v", err)
	}
}

func TestNewDataMaskUsesOfficialPluginName(t *testing.T) {
	p := New("data-mask")
	if p == nil {
		t.Fatal(`New("data-mask") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "data-mask" {
		t.Fatalf("plugin name = %q, want data-mask", got)
	}
	if got := p.GetPriority(); got != 1500 {
		t.Fatalf("priority = %d, want 1500", got)
	}
	config := map[string]any{
		"request": []any{
			map[string]any{"type": "query", "name": "token", "action": "replace", "value": "*****"},
			map[string]any{
				"type":        "body",
				"body_format": "json",
				"name":        "$.password",
				"action":      "remove",
			},
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("data-mask config should validate: %v", err)
	}
}

func TestNewErrorPageUsesOfficialPluginName(t *testing.T) {
	p := New("error-page")
	if p == nil {
		t.Fatal(`New("error-page") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "error-page" {
		t.Fatalf("plugin name = %q, want error-page", got)
	}
	if got := p.GetPriority(); got != 450 {
		t.Fatalf("priority = %d, want 450", got)
	}
	if err := util.Validate(map[string]any{}, p.GetSchema()); err != nil {
		t.Fatalf("empty official error-page config should validate: %v", err)
	}
}

func TestNewExitTransformerUsesOfficialPluginName(t *testing.T) {
	p := New("exit-transformer")
	if p == nil {
		t.Fatal(`New("exit-transformer") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "exit-transformer" {
		t.Fatalf("plugin name = %q, want exit-transformer", got)
	}
	if got := p.GetPriority(); got != 22950 {
		t.Fatalf("priority = %d, want 22950", got)
	}
	config := map[string]any{
		"functions": []any{
			"return (function(code, body, header) if code == 401 then return 403, body, header end return code, body, header end)(...)",
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("exit-transformer config should validate: %v", err)
	}
}

func TestNewAttachConsumerLabelUsesOfficialPluginName(t *testing.T) {
	p := New("attach-consumer-label")
	if p == nil {
		t.Fatal(`New("attach-consumer-label") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "attach-consumer-label" {
		t.Fatalf("plugin name = %q, want attach-consumer-label", got)
	}
	if got := p.GetPriority(); got != 2399 {
		t.Fatalf("priority = %d, want 2399", got)
	}
	config := map[string]any{
		"headers": map[string]any{
			"X-Consumer-Department": "$department",
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("attach-consumer-label config should validate: %v", err)
	}
}

func TestNewServerlessPreFunctionUsesOfficialPluginName(t *testing.T) {
	p := New("serverless-pre-function")
	if p == nil {
		t.Fatal(`New("serverless-pre-function") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "serverless-pre-function" {
		t.Fatalf("plugin name = %q, want serverless-pre-function", got)
	}
	if got := p.GetPriority(); got != 10000 {
		t.Fatalf("priority = %d, want 10000", got)
	}
	config := map[string]any{
		"phase":     "access",
		"functions": []any{`return function(conf, ctx) return 200, "ok" end`},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("serverless-pre-function config should validate: %v", err)
	}
}

func TestNewServerlessPostFunctionUsesOfficialPluginName(t *testing.T) {
	p := New("serverless-post-function")
	if p == nil {
		t.Fatal(`New("serverless-post-function") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "serverless-post-function" {
		t.Fatalf("plugin name = %q, want serverless-post-function", got)
	}
	if got := p.GetPriority(); got != -2000 {
		t.Fatalf("priority = %d, want -2000", got)
	}
	config := map[string]any{
		"phase":     "body_filter",
		"functions": []any{`return function(conf, ctx) ngx.arg[1] = "ok" end`},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("serverless-post-function config should validate: %v", err)
	}
}

func TestNewAzureFunctionsUsesOfficialPluginName(t *testing.T) {
	p := New("azure-functions")
	if p == nil {
		t.Fatal(`New("azure-functions") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "azure-functions" {
		t.Fatalf("plugin name = %q, want azure-functions", got)
	}
	if got := p.GetPriority(); got != -1900 {
		t.Fatalf("priority = %d, want -1900", got)
	}
	config := map[string]any{
		"function_uri": "https://example.azurewebsites.net/api/HttpTrigger",
		"authorization": map[string]any{
			"apikey": "key",
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("azure-functions config should validate: %v", err)
	}
}

func TestNewOpenFunctionUsesOfficialPluginName(t *testing.T) {
	p := New("openfunction")
	if p == nil {
		t.Fatal(`New("openfunction") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "openfunction" {
		t.Fatalf("plugin name = %q, want openfunction", got)
	}
	if got := p.GetPriority(); got != -1902 {
		t.Fatalf("priority = %d, want -1902", got)
	}
	config := map[string]any{
		"function_uri": "https://openfunction.example/default/function-sample",
		"authorization": map[string]any{
			"service_token": "test:test",
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("openfunction config should validate: %v", err)
	}
}

func TestNewOpenWhiskUsesOfficialPluginName(t *testing.T) {
	p := New("openwhisk")
	if p == nil {
		t.Fatal(`New("openwhisk") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "openwhisk" {
		t.Fatalf("plugin name = %q, want openwhisk", got)
	}
	if got := p.GetPriority(); got != -1901 {
		t.Fatalf("priority = %d, want -1901", got)
	}
	config := map[string]any{
		"api_host":      "https://openwhisk.example",
		"service_token": "user:pass",
		"namespace":     "guest",
		"action":        "hello",
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("openwhisk config should validate: %v", err)
	}
}

func TestNewAWSLambdaUsesOfficialPluginName(t *testing.T) {
	p := New("aws-lambda")
	if p == nil {
		t.Fatal(`New("aws-lambda") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "aws-lambda" {
		t.Fatalf("plugin name = %q, want aws-lambda", got)
	}
	if got := p.GetPriority(); got != -1899 {
		t.Fatalf("priority = %d, want -1899", got)
	}
	config := map[string]any{
		"function_uri": "https://lambda-url.us-west-2.on.aws/",
		"authorization": map[string]any{
			"iam": map[string]any{
				"accesskey": "AKID",
				"secretkey": "SECRET",
			},
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("aws-lambda config should validate: %v", err)
	}
}

func TestNewWolfRBACUsesOfficialPluginName(t *testing.T) {
	p := New("wolf-rbac")
	if p == nil {
		t.Fatal(`New("wolf-rbac") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "wolf-rbac" {
		t.Fatalf("plugin name = %q, want wolf-rbac", got)
	}
	if got := p.GetPriority(); got != 2555 {
		t.Fatalf("priority = %d, want 2555", got)
	}
	config := map[string]any{
		"appid":         "app-a",
		"server":        "https://wolf.example.com",
		"header_prefix": "X-",
		"ssl_verify":    true,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("wolf-rbac config should validate: %v", err)
	}
}

func TestNewKafkaLoggerUsesOfficialPluginName(t *testing.T) {
	p := New("kafka-logger")
	if p == nil {
		t.Fatal(`New("kafka-logger") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "kafka-logger" {
		t.Fatalf("plugin name = %q, want kafka-logger", got)
	}
	if got := p.GetPriority(); got != 403 {
		t.Fatalf("priority = %d, want 403", got)
	}
	config := map[string]any{
		"brokers": []any{
			map[string]any{"host": "127.0.0.1", "port": 9092},
		},
		"kafka_topic": "apisix-logs",
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("kafka-logger config should validate: %v", err)
	}
}

func TestNewRocketMQLoggerUsesOfficialPluginName(t *testing.T) {
	p := New("rocketmq-logger")
	if p == nil {
		t.Fatal(`New("rocketmq-logger") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "rocketmq-logger" {
		t.Fatalf("plugin name = %q, want rocketmq-logger", got)
	}
	if got := p.GetPriority(); got != 402 {
		t.Fatalf("priority = %d, want 402", got)
	}
	config := map[string]any{
		"nameserver_list": []any{"127.0.0.1:9876"},
		"topic":           "apisix-logs",
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("rocketmq-logger config should validate: %v", err)
	}
}

func TestNewLagoUsesOfficialPluginName(t *testing.T) {
	p := New("lago")
	if p == nil {
		t.Fatal(`New("lago") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "lago" {
		t.Fatalf("plugin name = %q, want lago", got)
	}
	if got := p.GetPriority(); got != 415 {
		t.Fatalf("priority = %d, want 415", got)
	}
	config := map[string]any{
		"endpoint_addrs":        []any{"http://127.0.0.1:3000"},
		"token":                 "lago-token",
		"event_transaction_id":  "${request_id}",
		"event_subscription_id": "${consumer_name}",
		"event_code":            "api-call",
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("lago config should validate: %v", err)
	}
}

func TestNewKafkaProxyUsesOfficialPluginName(t *testing.T) {
	p := New("kafka-proxy")
	if p == nil {
		t.Fatal(`New("kafka-proxy") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "kafka-proxy" {
		t.Fatalf("plugin name = %q, want kafka-proxy", got)
	}
	if got := p.GetPriority(); got != 508 {
		t.Fatalf("priority = %d, want 508", got)
	}
	config := map[string]any{
		"sasl": map[string]any{
			"username": "user",
			"password": "pwd",
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("kafka-proxy config should validate: %v", err)
	}
}

func TestNewDubboProxyUsesOfficialPluginName(t *testing.T) {
	p := New("dubbo-proxy")
	if p == nil {
		t.Fatal(`New("dubbo-proxy") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "dubbo-proxy" {
		t.Fatalf("plugin name = %q, want dubbo-proxy", got)
	}
	if got := p.GetPriority(); got != 507 {
		t.Fatalf("priority = %d, want 507", got)
	}
	config := map[string]any{
		"service_name":    "org.apache.dubbo.sample.DemoService",
		"service_version": "0.0.0",
		"method":          "sayHello",
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("dubbo-proxy config should validate: %v", err)
	}
}

func TestNewHTTPDubboUsesOfficialPluginName(t *testing.T) {
	p := New("http-dubbo")
	if p == nil {
		t.Fatal(`New("http-dubbo") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "http-dubbo" {
		t.Fatalf("plugin name = %q, want http-dubbo", got)
	}
	if got := p.GetPriority(); got != 504 {
		t.Fatalf("priority = %d, want 504", got)
	}
	config := map[string]any{
		"service_name":             "org.apache.dubbo.sample.DemoService",
		"service_version":          "0.0.0",
		"method":                   "sayHello",
		"params_type_desc":         "Ljava/lang/String;",
		"serialization_header_key": "X-Dubbo-Serialized",
		"serialized":               false,
		"connect_timeout":          100,
		"read_timeout":             100,
		"send_timeout":             100,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("http-dubbo config should validate: %v", err)
	}
}

func TestNewAIPromptDecoratorUsesOfficialPluginName(t *testing.T) {
	p := New("ai-prompt-decorator")
	if p == nil {
		t.Fatal(`New("ai-prompt-decorator") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "ai-prompt-decorator" {
		t.Fatalf("plugin name = %q, want ai-prompt-decorator", got)
	}
	if got := p.GetPriority(); got != 1070 {
		t.Fatalf("priority = %d, want 1070", got)
	}
	config := map[string]any{
		"prepend": []any{
			map[string]any{"role": "system", "content": "answer briefly"},
		},
		"append": []any{
			map[string]any{"role": "user", "content": "end with analogy"},
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("ai-prompt-decorator config should validate: %v", err)
	}
}

func TestNewAIPromptGuardUsesOfficialPluginName(t *testing.T) {
	p := New("ai-prompt-guard")
	if p == nil {
		t.Fatal(`New("ai-prompt-guard") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "ai-prompt-guard" {
		t.Fatalf("plugin name = %q, want ai-prompt-guard", got)
	}
	if got := p.GetPriority(); got != 1072 {
		t.Fatalf("priority = %d, want 1072", got)
	}
	config := map[string]any{
		"match_all_roles":                true,
		"match_all_conversation_history": true,
		"allow_patterns":                 []any{`\\$?\\d+(\\.\\d+)?`},
		"deny_patterns":                  []any{`\\d{3}-\\d{3}-\\d{4}`},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("ai-prompt-guard config should validate: %v", err)
	}
}

func TestNewAIPromptTemplateUsesOfficialPluginName(t *testing.T) {
	p := New("ai-prompt-template")
	if p == nil {
		t.Fatal(`New("ai-prompt-template") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "ai-prompt-template" {
		t.Fatalf("plugin name = %q, want ai-prompt-template", got)
	}
	if got := p.GetPriority(); got != 1071 {
		t.Fatalf("priority = %d, want 1071", got)
	}
	config := map[string]any{
		"templates": []any{
			map[string]any{
				"name": "QnA with complexity",
				"template": map[string]any{
					"model": "gpt-4",
					"messages": []any{
						map[string]any{"role": "system", "content": "Answer in {{complexity}}."},
						map[string]any{"role": "user", "content": "Explain {{prompt}}."},
					},
				},
			},
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("ai-prompt-template config should validate: %v", err)
	}
}

func TestNewAIRequestRewriteUsesOfficialPluginName(t *testing.T) {
	p := New("ai-request-rewrite")
	if p == nil {
		t.Fatal(`New("ai-request-rewrite") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "ai-request-rewrite" {
		t.Fatalf("plugin name = %q, want ai-request-rewrite", got)
	}
	if got := p.GetPriority(); got != 1073 {
		t.Fatalf("priority = %d, want 1073", got)
	}
	config := map[string]any{
		"prompt":   "rewrite sensitive data",
		"provider": "openai-compatible",
		"auth": map[string]any{
			"header": map[string]any{"Authorization": "Bearer token"},
		},
		"options": map[string]any{
			"model": "gpt-4",
		},
		"override": map[string]any{
			"endpoint": "https://llm.example.test/v1/chat/completions",
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("ai-request-rewrite config should validate: %v", err)
	}
}

func TestNewAIRateLimitingUsesOfficialPluginName(t *testing.T) {
	p := New("ai-rate-limiting")
	if p == nil {
		t.Fatal(`New("ai-rate-limiting") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "ai-rate-limiting" {
		t.Fatalf("plugin name = %q, want ai-rate-limiting", got)
	}
	if got := p.GetPriority(); got != 1030 {
		t.Fatalf("priority = %d, want 1030", got)
	}
	config := map[string]any{
		"limit":                   300,
		"time_window":             60,
		"show_limit_quota_header": true,
		"limit_strategy":          "prompt_tokens",
		"rejected_code":           429,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("ai-rate-limiting config should validate: %v", err)
	}
}

func TestNewAIProxyUsesOfficialPluginName(t *testing.T) {
	p := New("ai-proxy")
	if p == nil {
		t.Fatal(`New("ai-proxy") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "ai-proxy" {
		t.Fatalf("plugin name = %q, want ai-proxy", got)
	}
	if got := p.GetPriority(); got != 1040 {
		t.Fatalf("priority = %d, want 1040", got)
	}
	config := map[string]any{
		"provider": "openai-compatible",
		"auth": map[string]any{
			"header": map[string]any{"Authorization": "Bearer token"},
		},
		"options": map[string]any{
			"model": "gpt-4",
		},
		"override": map[string]any{
			"endpoint": "https://llm.example.test/v1/chat/completions",
		},
		"timeout":           30000,
		"max_req_body_size": 67108864,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("ai-proxy config should validate: %v", err)
	}
}

func TestNewAIProxyMultiUsesOfficialPluginName(t *testing.T) {
	p := New("ai-proxy-multi")
	if p == nil {
		t.Fatal(`New("ai-proxy-multi") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "ai-proxy-multi" {
		t.Fatalf("plugin name = %q, want ai-proxy-multi", got)
	}
	if got := p.GetPriority(); got != 1041 {
		t.Fatalf("priority = %d, want 1041", got)
	}
	config := map[string]any{
		"balancer": map[string]any{"algorithm": "roundrobin"},
		"instances": []any{
			map[string]any{
				"name":     "one",
				"provider": "openai-compatible",
				"weight":   1,
				"auth": map[string]any{
					"header": map[string]any{"Authorization": "Bearer one"},
				},
				"override": map[string]any{
					"endpoint": "https://one.example.test/v1/chat/completions",
				},
			},
			map[string]any{
				"name":     "two",
				"provider": "openai-compatible",
				"weight":   1,
				"auth": map[string]any{
					"header": map[string]any{"Authorization": "Bearer two"},
				},
				"override": map[string]any{
					"endpoint": "https://two.example.test/v1/chat/completions",
				},
			},
		},
		"fallback_strategy": []any{"http_5xx", "http_429"},
		"max_retries":       1,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("ai-proxy-multi config should validate: %v", err)
	}
}

func TestNewAIAWSContentModerationUsesOfficialPluginName(t *testing.T) {
	p := New("ai-aws-content-moderation")
	if p == nil {
		t.Fatal(`New("ai-aws-content-moderation") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "ai-aws-content-moderation" {
		t.Fatalf("plugin name = %q, want ai-aws-content-moderation", got)
	}
	if got := p.GetPriority(); got != 1050 {
		t.Fatalf("priority = %d, want 1050", got)
	}
	config := map[string]any{
		"comprehend": map[string]any{
			"access_key_id":     "test-access",
			"secret_access_key": "test-secret",
			"region":            "us-east-1",
			"endpoint":          "https://comprehend.us-east-1.amazonaws.com",
		},
		"moderation_categories": map[string]any{
			"PROFANITY": 0.2,
		},
		"moderation_threshold": 0.5,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("ai-aws-content-moderation config should validate: %v", err)
	}
}

func TestNewAIAliyunContentModerationUsesOfficialPluginName(t *testing.T) {
	p := New("ai-aliyun-content-moderation")
	if p == nil {
		t.Fatal(`New("ai-aliyun-content-moderation") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "ai-aliyun-content-moderation" {
		t.Fatalf("plugin name = %q, want ai-aliyun-content-moderation", got)
	}
	if got := p.GetPriority(); got != 1029 {
		t.Fatalf("priority = %d, want 1029", got)
	}
	config := map[string]any{
		"endpoint":          "https://green-cip.cn-shanghai.aliyuncs.com",
		"region_id":         "cn-shanghai",
		"access_key_id":     "test-access",
		"access_key_secret": "test-secret",
		"risk_level_bar":    "high",
		"deny_code":         200,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("ai-aliyun-content-moderation config should validate: %v", err)
	}
}

func TestNewAIRAGUsesOfficialPluginName(t *testing.T) {
	p := New("ai-rag")
	if p == nil {
		t.Fatal(`New("ai-rag") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "ai-rag" {
		t.Fatalf("plugin name = %q, want ai-rag", got)
	}
	if got := p.GetPriority(); got != 1060 {
		t.Fatalf("priority = %d, want 1060", got)
	}
	config := map[string]any{
		"embeddings_provider": map[string]any{
			"azure_openai": map[string]any{
				"endpoint": "https://example.openai.azure.com/openai/deployments/embed/embeddings?api-version=2023-05-15",
				"api_key":  "embedding-key",
			},
		},
		"vector_search_provider": map[string]any{
			"azure_ai_search": map[string]any{
				"endpoint": "https://example.search.windows.net/indexes/vectest/docs/search?api-version=2024-07-01",
				"api_key":  "search-key",
			},
		},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("ai-rag config should validate: %v", err)
	}
}

func TestNewOASValidatorUsesOfficialPluginName(t *testing.T) {
	p := New("oas-validator")
	if p == nil {
		t.Fatal(`New("oas-validator") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "oas-validator" {
		t.Fatalf("plugin name = %q, want oas-validator", got)
	}
	if got := p.GetPriority(); got != 512 {
		t.Fatalf("priority = %d, want 512", got)
	}
	config := map[string]any{
		"spec": `{"openapi":"3.0.2","info":{"title":"Pet API","version":"1.0.0"},"paths":{}}`,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("oas-validator config should validate: %v", err)
	}
}

func TestNewTencentCloudCLSUsesOfficialPluginName(t *testing.T) {
	p := New("tencent-cloud-cls")
	if p == nil {
		t.Fatal(`New("tencent-cloud-cls") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "tencent-cloud-cls" {
		t.Fatalf("plugin name = %q, want tencent-cloud-cls", got)
	}
	if got := p.GetPriority(); got != 397 {
		t.Fatalf("priority = %d, want 397", got)
	}
	config := map[string]any{
		"cls_host":   "ap-guangzhou.cls.tencentcs.com",
		"cls_topic":  "topic-a",
		"secret_id":  "secret-id",
		"secret_key": "secret-key",
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("tencent-cloud-cls config should validate: %v", err)
	}
}

func TestNewSAMLAuthUsesOfficialPluginName(t *testing.T) {
	p := New("saml-auth")
	if p == nil {
		t.Fatal(`New("saml-auth") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "saml-auth" {
		t.Fatalf("plugin name = %q, want saml-auth", got)
	}
	if got := p.GetPriority(); got != 2598 {
		t.Fatalf("priority = %d, want 2598", got)
	}
	config := map[string]any{
		"sp_issuer":           "https://sp.example.com",
		"idp_uri":             "https://idp.example.com/sso",
		"idp_cert":            "-----BEGIN CERTIFICATE-----\nMIIC\n-----END CERTIFICATE-----",
		"login_callback_uri":  "https://sp.example.com/login/callback",
		"logout_uri":          "/logout",
		"logout_callback_uri": "https://sp.example.com/logout/callback",
		"logout_redirect_uri": "/logged-out",
		"sp_cert":             "-----BEGIN CERTIFICATE-----\nMIIC\n-----END CERTIFICATE-----",
		"sp_private_key":      "-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----",
		"secret":              "session-secret",
		"secret_fallbacks":    []any{"old-secret"},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("saml-auth config should validate: %v", err)
	}
}

func TestNewMCPBridgeUsesOfficialPluginName(t *testing.T) {
	p := New("mcp-bridge")
	if p == nil {
		t.Fatal(`New("mcp-bridge") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "mcp-bridge" {
		t.Fatalf("plugin name = %q, want mcp-bridge", got)
	}
	if got := p.GetPriority(); got != 510 {
		t.Fatalf("priority = %d, want 510", got)
	}
	config := map[string]any{
		"base_uri": "/mcp",
		"command":  "node",
		"args":     []any{"server.js"},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("mcp-bridge config should validate: %v", err)
	}
}

func TestNewAIUsesOfficialPluginName(t *testing.T) {
	p := New("ai")
	if p == nil {
		t.Fatal(`New("ai") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "ai" {
		t.Fatalf("plugin name = %q, want ai", got)
	}
	if got := p.GetPriority(); got != 22900 {
		t.Fatalf("priority = %d, want 22900", got)
	}
	if err := util.Validate(map[string]any{}, p.GetSchema()); err != nil {
		t.Fatalf("ai config should validate: %v", err)
	}
}

func TestNewExamplePluginUsesOfficialPluginName(t *testing.T) {
	p := New("example-plugin")
	if p == nil {
		t.Fatal(`New("example-plugin") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "example-plugin" {
		t.Fatalf("plugin name = %q, want example-plugin", got)
	}
	if got := p.GetPriority(); got != 0 {
		t.Fatalf("priority = %d, want 0", got)
	}
	config := map[string]any{
		"i":    1,
		"s":    "demo",
		"t":    []any{"a"},
		"ip":   "127.0.0.1",
		"port": 1980,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("example-plugin config should validate: %v", err)
	}
}

func TestNewPrometheusUsesOfficialPluginName(t *testing.T) {
	p := New("prometheus")
	if p == nil {
		t.Fatal(`New("prometheus") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "prometheus" {
		t.Fatalf("plugin name = %q, want prometheus", got)
	}
	if got := p.GetPriority(); got != 500 {
		t.Fatalf("priority = %d, want 500", got)
	}
	if err := util.Validate(map[string]any{"prefer_name": true}, p.GetSchema()); err != nil {
		t.Fatalf("prometheus config should validate: %v", err)
	}
}

func TestNewMQTTProxyUsesOfficialPluginName(t *testing.T) {
	p := New("mqtt-proxy")
	if p == nil {
		t.Fatal(`New("mqtt-proxy") returned nil`)
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if got := p.GetName(); got != "mqtt-proxy" {
		t.Fatalf("plugin name = %q, want mqtt-proxy", got)
	}
	if got := p.GetPriority(); got != 1000 {
		t.Fatalf("priority = %d, want 1000", got)
	}
	config := map[string]any{
		"protocol_name":  "MQTT",
		"protocol_level": 5,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("mqtt-proxy config should validate: %v", err)
	}
}
