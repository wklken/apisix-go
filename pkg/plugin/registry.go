package plugin

import (
	"sort"

	acl "github.com/wklken/apisix-go/pkg/plugin/acl"
	ai_aliyun_content_moderation "github.com/wklken/apisix-go/pkg/plugin/ai_aliyun_content_moderation"
	ai_aws_content_moderation "github.com/wklken/apisix-go/pkg/plugin/ai_aws_content_moderation"
	ai_prompt_decorator "github.com/wklken/apisix-go/pkg/plugin/ai_prompt_decorator"
	ai_prompt_guard "github.com/wklken/apisix-go/pkg/plugin/ai_prompt_guard"
	ai_prompt_template "github.com/wklken/apisix-go/pkg/plugin/ai_prompt_template"
	ai_proxy "github.com/wklken/apisix-go/pkg/plugin/ai_proxy"
	ai_proxy_multi "github.com/wklken/apisix-go/pkg/plugin/ai_proxy_multi"
	ai_rag "github.com/wklken/apisix-go/pkg/plugin/ai_rag"
	ai_rate_limiting "github.com/wklken/apisix-go/pkg/plugin/ai_rate_limiting"
	ai_request_rewrite "github.com/wklken/apisix-go/pkg/plugin/ai_request_rewrite"
	api_breaker "github.com/wklken/apisix-go/pkg/plugin/api_breaker"
	attach_consumer_label "github.com/wklken/apisix-go/pkg/plugin/attach_consumer_label"
	authz_casbin "github.com/wklken/apisix-go/pkg/plugin/authz_casbin"
	authz_casdoor "github.com/wklken/apisix-go/pkg/plugin/authz_casdoor"
	authz_keycloak "github.com/wklken/apisix-go/pkg/plugin/authz_keycloak"
	aws_lambda "github.com/wklken/apisix-go/pkg/plugin/aws_lambda"
	azure_functions "github.com/wklken/apisix-go/pkg/plugin/azure_functions"
	basic_auth "github.com/wklken/apisix-go/pkg/plugin/basic_auth"
	batch_requests "github.com/wklken/apisix-go/pkg/plugin/batch_requests"
	body_transformer "github.com/wklken/apisix-go/pkg/plugin/body_transformer"
	brotli "github.com/wklken/apisix-go/pkg/plugin/brotli"
	cas_auth "github.com/wklken/apisix-go/pkg/plugin/cas_auth"
	chaitin_waf "github.com/wklken/apisix-go/pkg/plugin/chaitin_waf"
	clickhouse_logger "github.com/wklken/apisix-go/pkg/plugin/clickhouse_logger"
	client_control "github.com/wklken/apisix-go/pkg/plugin/client_control"
	consumer_restriction "github.com/wklken/apisix-go/pkg/plugin/consumer_restriction"
	cors "github.com/wklken/apisix-go/pkg/plugin/cors"
	csrf "github.com/wklken/apisix-go/pkg/plugin/csrf"
	data_mask "github.com/wklken/apisix-go/pkg/plugin/data_mask"
	datadog "github.com/wklken/apisix-go/pkg/plugin/datadog"
	degraphql "github.com/wklken/apisix-go/pkg/plugin/degraphql"
	dingtalk_auth "github.com/wklken/apisix-go/pkg/plugin/dingtalk_auth"
	dubbo_proxy "github.com/wklken/apisix-go/pkg/plugin/dubbo_proxy"
	echo "github.com/wklken/apisix-go/pkg/plugin/echo"
	elasticsearch_logger "github.com/wklken/apisix-go/pkg/plugin/elasticsearch_logger"
	error_log_logger "github.com/wklken/apisix-go/pkg/plugin/error_log_logger"
	error_page "github.com/wklken/apisix-go/pkg/plugin/error_page"
	example_plugin "github.com/wklken/apisix-go/pkg/plugin/example_plugin"
	exit_transformer "github.com/wklken/apisix-go/pkg/plugin/exit_transformer"
	fault_injection "github.com/wklken/apisix-go/pkg/plugin/fault_injection"
	feishu_auth "github.com/wklken/apisix-go/pkg/plugin/feishu_auth"
	file_logger "github.com/wklken/apisix-go/pkg/plugin/file_logger"
	forward_auth "github.com/wklken/apisix-go/pkg/plugin/forward_auth"
	gm "github.com/wklken/apisix-go/pkg/plugin/gm"
	google_cloud_logging "github.com/wklken/apisix-go/pkg/plugin/google_cloud_logging"
	graphql_limit_count "github.com/wklken/apisix-go/pkg/plugin/graphql_limit_count"
	graphql_proxy_cache "github.com/wklken/apisix-go/pkg/plugin/graphql_proxy_cache"
	grpc_transcode "github.com/wklken/apisix-go/pkg/plugin/grpc_transcode"
	grpc_web "github.com/wklken/apisix-go/pkg/plugin/grpc_web"
	gzip "github.com/wklken/apisix-go/pkg/plugin/gzip"
	hmac_auth "github.com/wklken/apisix-go/pkg/plugin/hmac_auth"
	http_dubbo "github.com/wklken/apisix-go/pkg/plugin/http_dubbo"
	http_logger "github.com/wklken/apisix-go/pkg/plugin/http_logger"
	ip_restriction "github.com/wklken/apisix-go/pkg/plugin/ip_restriction"
	jwe_decrypt "github.com/wklken/apisix-go/pkg/plugin/jwe_decrypt"
	jwt_auth "github.com/wklken/apisix-go/pkg/plugin/jwt_auth"
	kafka_logger "github.com/wklken/apisix-go/pkg/plugin/kafka_logger"
	kafka_proxy "github.com/wklken/apisix-go/pkg/plugin/kafka_proxy"
	key_auth "github.com/wklken/apisix-go/pkg/plugin/key_auth"
	lago "github.com/wklken/apisix-go/pkg/plugin/lago"
	ldap_auth "github.com/wklken/apisix-go/pkg/plugin/ldap_auth"
	limit_conn "github.com/wklken/apisix-go/pkg/plugin/limit_conn"
	limit_count "github.com/wklken/apisix-go/pkg/plugin/limit_count"
	limit_req "github.com/wklken/apisix-go/pkg/plugin/limit_req"
	log_rotate "github.com/wklken/apisix-go/pkg/plugin/log_rotate"
	loggly "github.com/wklken/apisix-go/pkg/plugin/loggly"
	loki_logger "github.com/wklken/apisix-go/pkg/plugin/loki_logger"
	mcp_bridge "github.com/wklken/apisix-go/pkg/plugin/mcp_bridge"
	mocking "github.com/wklken/apisix-go/pkg/plugin/mocking"
	mqtt_proxy "github.com/wklken/apisix-go/pkg/plugin/mqtt_proxy"
	multi_auth "github.com/wklken/apisix-go/pkg/plugin/multi_auth"
	node_status "github.com/wklken/apisix-go/pkg/plugin/node_status"
	oas_validator "github.com/wklken/apisix-go/pkg/plugin/oas_validator"
	opa "github.com/wklken/apisix-go/pkg/plugin/opa"
	openfunction "github.com/wklken/apisix-go/pkg/plugin/openfunction"
	openid_connect "github.com/wklken/apisix-go/pkg/plugin/openid_connect"
	openwhisk "github.com/wklken/apisix-go/pkg/plugin/openwhisk"
	otel "github.com/wklken/apisix-go/pkg/plugin/otel"
	prometheus "github.com/wklken/apisix-go/pkg/plugin/prometheus"
	proxy_buffering "github.com/wklken/apisix-go/pkg/plugin/proxy_buffering"
	proxy_cache "github.com/wklken/apisix-go/pkg/plugin/proxy_cache"
	proxy_control "github.com/wklken/apisix-go/pkg/plugin/proxy_control"
	proxy_mirror "github.com/wklken/apisix-go/pkg/plugin/proxy_mirror"
	proxy_rewrite "github.com/wklken/apisix-go/pkg/plugin/proxy_rewrite"
	public_api "github.com/wklken/apisix-go/pkg/plugin/public_api"
	real_ip "github.com/wklken/apisix-go/pkg/plugin/real_ip"
	redirect "github.com/wklken/apisix-go/pkg/plugin/redirect"
	referer_restriction "github.com/wklken/apisix-go/pkg/plugin/referer_restriction"
	request_id "github.com/wklken/apisix-go/pkg/plugin/request_id"
	request_validation "github.com/wklken/apisix-go/pkg/plugin/request_validation"
	response_rewrite "github.com/wklken/apisix-go/pkg/plugin/response_rewrite"
	rocketmq_logger "github.com/wklken/apisix-go/pkg/plugin/rocketmq_logger"
	saml_auth "github.com/wklken/apisix-go/pkg/plugin/saml_auth"
	server_info "github.com/wklken/apisix-go/pkg/plugin/server_info"
	serverless "github.com/wklken/apisix-go/pkg/plugin/serverless"
	skywalking "github.com/wklken/apisix-go/pkg/plugin/skywalking"
	skywalking_logger "github.com/wklken/apisix-go/pkg/plugin/skywalking_logger"
	sls_logger "github.com/wklken/apisix-go/pkg/plugin/sls_logger"
	splunk_hec_logging "github.com/wklken/apisix-go/pkg/plugin/splunk_hec_logging"
	syslog "github.com/wklken/apisix-go/pkg/plugin/syslog"
	tcp_logger "github.com/wklken/apisix-go/pkg/plugin/tcp_logger"
	tencent_cloud_cls "github.com/wklken/apisix-go/pkg/plugin/tencent_cloud_cls"
	traffic_label "github.com/wklken/apisix-go/pkg/plugin/traffic_label"
	traffic_split "github.com/wklken/apisix-go/pkg/plugin/traffic_split"
	ua_restriction "github.com/wklken/apisix-go/pkg/plugin/ua_restriction"
	udp_logger "github.com/wklken/apisix-go/pkg/plugin/udp_logger"
	uri_blocker "github.com/wklken/apisix-go/pkg/plugin/uri_blocker"
	wolf_rbac "github.com/wklken/apisix-go/pkg/plugin/wolf_rbac"
	workflow "github.com/wklken/apisix-go/pkg/plugin/workflow"
	zipkin "github.com/wklken/apisix-go/pkg/plugin/zipkin"
)

type Domain uint8

const (
	DomainHTTP Domain = iota + 1
	DomainStream
)

type phaseMask uint16

const (
	phaseRewrite phaseMask = 1 << iota
	phaseConsumerRewrite
	phaseAccess
	phaseBeforeProxy
	phaseHeaderFilter
	phaseBodyFilter
	phaseLog
	phaseFinalizer
	phaseProtocol
)

type scopeMask uint8

const (
	scopeSystem scopeMask = 1 << iota
	scopeGlobal
	scopeRoute
	scopeConsumer
)

type registration struct {
	create              func() Plugin
	phases              phaseMask
	scopes              scopeMask
	instanceScope       InstanceScope
	conditionalTerminal bool
	domain              Domain
}

// Definition is the immutable execution metadata for one implemented plugin factory.
type Definition struct {
	Factory             string
	Domain              Domain
	Phases              []Phase
	Scopes              []Scope
	InstanceScope       InstanceScope
	ConditionalTerminal bool
}

// Definitions returns all implemented plugin factories in stable order.
func Definitions() []Definition {
	factories := make([]string, 0, len(pluginRegistry))
	for factory := range pluginRegistry {
		factories = append(factories, factory)
	}
	sort.Strings(factories)
	definitions := make([]Definition, 0, len(factories))
	for _, factory := range factories {
		definition, _ := DefinitionForFactory(factory)
		definitions = append(definitions, definition)
	}
	return definitions
}

// DefinitionForFactory returns the execution metadata for one implemented factory.
func DefinitionForFactory(factory string) (Definition, bool) {
	registered, ok := pluginRegistry[factory]
	if !ok {
		return Definition{}, false
	}
	return Definition{
		Factory:             factory,
		Domain:              registered.domain,
		Phases:              registered.phases.values(),
		Scopes:              registered.scopes.values(),
		InstanceScope:       registered.instanceScope,
		ConditionalTerminal: registered.conditionalTerminal,
	}, true
}

func (mask phaseMask) values() []Phase {
	values := make([]Phase, 0, 4)
	for _, candidate := range []struct {
		mask  phaseMask
		phase Phase
	}{
		{phaseRewrite, PhaseRewrite},
		{phaseConsumerRewrite, PhaseConsumerRewrite},
		{phaseAccess, PhaseAccess},
		{phaseBeforeProxy, PhaseBeforeProxy},
		{phaseHeaderFilter, PhaseHeaderFilter},
		{phaseBodyFilter, PhaseBodyFilter},
		{phaseLog, PhaseLog},
		{phaseFinalizer, PhaseFinalizer},
		{phaseProtocol, PhaseProtocol},
	} {
		if mask&candidate.mask != 0 {
			values = append(values, candidate.phase)
		}
	}
	return values
}

func (mask scopeMask) values() []Scope {
	values := make([]Scope, 0, 4)
	for _, candidate := range []struct {
		mask  scopeMask
		scope Scope
	}{
		{scopeSystem, ScopeSystem},
		{scopeGlobal, ScopeGlobal},
		{scopeRoute, ScopeRoute},
		{scopeConsumer, ScopeConsumer},
	} {
		if mask&candidate.mask != 0 {
			values = append(values, candidate.scope)
		}
	}
	return values
}

var pluginRegistry = map[string]registration{
	"batch-requests": {
		create:              func() Plugin { return &batch_requests.Plugin{} },
		phases:              0,
		scopes:              scopeSystem,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"redirect": {
		create:              func() Plugin { return &redirect.Plugin{} },
		phases:              phaseRewrite,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"echo": {
		create:              func() Plugin { return &echo.Plugin{} },
		phases:              phaseHeaderFilter | phaseBodyFilter,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"gzip": {
		create:              func() Plugin { return &gzip.Plugin{} },
		phases:              phaseHeaderFilter | phaseBodyFilter,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"brotli": {
		create:              func() Plugin { return &brotli.Plugin{} },
		phases:              phaseHeaderFilter | phaseBodyFilter,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"real-ip": {
		create:              func() Plugin { return &real_ip.Plugin{} },
		phases:              phaseRewrite,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"server-info": {
		create:              func() Plugin { return &server_info.Plugin{} },
		phases:              0,
		scopes:              scopeSystem,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"error-page": {
		create:              func() Plugin { return &error_page.Plugin{} },
		phases:              phaseBodyFilter,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"exit-transformer": {
		create:              func() Plugin { return &exit_transformer.Plugin{} },
		phases:              phaseBodyFilter,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"attach-consumer-label": {
		create:              func() Plugin { return &attach_consumer_label.Plugin{} },
		phases:              phaseConsumerRewrite,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"serverless-pre-function": {
		create:              func() Plugin { return serverless.NewPreFunction() },
		phases:              phaseRewrite | phaseAccess | phaseBeforeProxy | phaseHeaderFilter | phaseBodyFilter | phaseLog,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"serverless-post-function": {
		create:              func() Plugin { return serverless.NewPostFunction() },
		phases:              phaseRewrite | phaseAccess | phaseBeforeProxy | phaseHeaderFilter | phaseBodyFilter | phaseLog,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"azure-functions": {
		create:              func() Plugin { return &azure_functions.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"openfunction": {
		create:              func() Plugin { return &openfunction.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"openwhisk": {
		create:              func() Plugin { return &openwhisk.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"aws-lambda": {
		create:              func() Plugin { return &aws_lambda.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"response-rewrite": {
		create:              func() Plugin { return &response_rewrite.Plugin{} },
		phases:              phaseHeaderFilter | phaseBodyFilter,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"proxy-rewrite": {
		create:              func() Plugin { return &proxy_rewrite.Plugin{} },
		phases:              phaseRewrite,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"grpc-transcode": {
		create:              func() Plugin { return &grpc_transcode.Plugin{} },
		phases:              phaseAccess | phaseBodyFilter,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"grpc-web": {
		create:              func() Plugin { return &grpc_web.Plugin{} },
		phases:              phaseAccess | phaseHeaderFilter | phaseBodyFilter | phaseProtocol,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"fault-injection": {
		create:              func() Plugin { return &fault_injection.Plugin{} },
		phases:              phaseRewrite,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"mocking": {
		create:              func() Plugin { return &mocking.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"degraphql": {
		create:              func() Plugin { return &degraphql.Plugin{} },
		phases:              phaseRewrite,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"body-transformer": {
		create:              func() Plugin { return &body_transformer.Plugin{} },
		phases:              phaseRewrite | phaseBodyFilter,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"key-auth": {
		create:              func() Plugin { return &key_auth.Plugin{} },
		phases:              phaseAccess | phaseLog,
		scopes:              scopeGlobal | scopeRoute,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"jwt-auth": {
		create:              func() Plugin { return &jwt_auth.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"jwe-decrypt": {
		create:              func() Plugin { return &jwe_decrypt.Plugin{} },
		phases:              phaseRewrite,
		scopes:              scopeGlobal | scopeRoute,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"basic-auth": {
		create:              func() Plugin { return &basic_auth.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"authz-keycloak": {
		create:              func() Plugin { return &authz_keycloak.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"authz-casdoor": {
		create:              func() Plugin { return &authz_casdoor.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"dingtalk-auth": {
		create:              func() Plugin { return &dingtalk_auth.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"feishu-auth": {
		create:              func() Plugin { return &feishu_auth.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"saml-auth": {
		create:              func() Plugin { return &saml_auth.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"wolf-rbac": {
		create:              func() Plugin { return &wolf_rbac.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"openid-connect": {
		create:              func() Plugin { return &openid_connect.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"cas-auth": {
		create:              func() Plugin { return &cas_auth.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"hmac-auth": {
		create:              func() Plugin { return &hmac_auth.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"authz-casbin": {
		create:              func() Plugin { return &authz_casbin.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"ldap-auth": {
		create:              func() Plugin { return &ldap_auth.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"opa": {
		create:              func() Plugin { return &opa.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"forward-auth": {
		create:              func() Plugin { return &forward_auth.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"multi-auth": {
		create:              func() Plugin { return &multi_auth.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"cors": {
		create:              func() Plugin { return &cors.Plugin{} },
		phases:              phaseRewrite | phaseHeaderFilter,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"acl": {
		create:              func() Plugin { return &acl.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"uri-blocker": {
		create:              func() Plugin { return &uri_blocker.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"ip-restriction": {
		create:              func() Plugin { return &ip_restriction.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"ua-restriction": {
		create:              func() Plugin { return &ua_restriction.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"referer-restriction": {
		create:              func() Plugin { return &referer_restriction.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"consumer-restriction": {
		create:              func() Plugin { return &consumer_restriction.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"csrf": {
		create:              func() Plugin { return &csrf.Plugin{} },
		phases:              phaseAccess | phaseHeaderFilter,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"public-api": {
		create:              func() Plugin { return &public_api.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"gm": {
		create:              func() Plugin { return &gm.Plugin{} },
		phases:              0,
		scopes:              scopeSystem,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"chaitin-waf": {
		create:              func() Plugin { return &chaitin_waf.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"data-mask": {
		create:              func() Plugin { return &data_mask.Plugin{} },
		phases:              phaseLog,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"oas-validator": {
		create:              func() Plugin { return &oas_validator.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"limit-req": {
		create:              func() Plugin { return &limit_req.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"limit-conn": {
		create:              func() Plugin { return &limit_conn.Plugin{} },
		phases:              phaseAccess | phaseFinalizer,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstancePerGlobalRule,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"limit-count": {
		create:              func() Plugin { return &limit_count.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"graphql-limit-count": {
		create:              func() Plugin { return &graphql_limit_count.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"proxy-cache": {
		create:              func() Plugin { return &proxy_cache.Plugin{} },
		phases:              phaseAccess | phaseBodyFilter,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"graphql-proxy-cache": {
		create:              func() Plugin { return &graphql_proxy_cache.Plugin{} },
		phases:              phaseAccess | phaseBodyFilter,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"request-validation": {
		create:              func() Plugin { return &request_validation.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"proxy-mirror": {
		create:              func() Plugin { return &proxy_mirror.Plugin{} },
		phases:              phaseRewrite | phaseBeforeProxy,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"kafka-proxy": {
		create:              func() Plugin { return &kafka_proxy.Plugin{} },
		phases:              phaseAccess | phaseBeforeProxy | phaseProtocol,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"dubbo-proxy": {
		create:              func() Plugin { return &dubbo_proxy.Plugin{} },
		phases:              phaseAccess | phaseBeforeProxy | phaseProtocol,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"http-dubbo": {
		create:              func() Plugin { return &http_dubbo.Plugin{} },
		phases:              phaseBeforeProxy | phaseProtocol,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"api-breaker": {
		create:              func() Plugin { return &api_breaker.Plugin{} },
		phases:              phaseAccess | phaseFinalizer,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"traffic-split": {
		create:              func() Plugin { return &traffic_split.Plugin{} },
		phases:              phaseRewrite,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"traffic-label": {
		create:              func() Plugin { return &traffic_label.Plugin{} },
		phases:              phaseRewrite,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"request-id": {
		create:              func() Plugin { return &request_id.Plugin{} },
		phases:              phaseRewrite,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"proxy-control": {
		create:              func() Plugin { return &proxy_control.Plugin{} },
		phases:              phaseRewrite,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"proxy-buffering": {
		create:              func() Plugin { return &proxy_buffering.Plugin{} },
		phases:              phaseRewrite | phaseBodyFilter,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"client-control": {
		create:              func() Plugin { return &client_control.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"workflow": {
		create:              func() Plugin { return &workflow.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"zipkin": {
		create:              func() Plugin { return &zipkin.Plugin{} },
		phases:              phaseRewrite | phaseFinalizer,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"skywalking": {
		create:              func() Plugin { return &skywalking.Plugin{} },
		phases:              phaseRewrite | phaseFinalizer,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"opentelemetry": {
		create:              func() Plugin { return &otel.Plugin{} },
		phases:              phaseRewrite | phaseFinalizer,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"prometheus": {
		create:              func() Plugin { return &prometheus.Plugin{} },
		phases:              phaseLog,
		scopes:              scopeSystem | scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"node-status": {
		create:              func() Plugin { return &node_status.Plugin{} },
		phases:              0,
		scopes:              scopeSystem,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"datadog": {
		create:              func() Plugin { return &datadog.Plugin{} },
		phases:              phaseLog,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"http-logger": {
		create:              func() Plugin { return &http_logger.Plugin{} },
		phases:              phaseLog,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"skywalking-logger": {
		create:              func() Plugin { return &skywalking_logger.Plugin{} },
		phases:              phaseLog,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"tcp-logger": {
		create:              func() Plugin { return &tcp_logger.Plugin{} },
		phases:              phaseLog,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"kafka-logger": {
		create:              func() Plugin { return &kafka_logger.Plugin{} },
		phases:              phaseLog,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"rocketmq-logger": {
		create:              func() Plugin { return &rocketmq_logger.Plugin{} },
		phases:              phaseLog,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"udp-logger": {
		create:              func() Plugin { return &udp_logger.Plugin{} },
		phases:              phaseLog,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"clickhouse-logger": {
		create:              func() Plugin { return &clickhouse_logger.Plugin{} },
		phases:              phaseLog,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"syslog": {
		create:              func() Plugin { return &syslog.Plugin{} },
		phases:              phaseLog,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"log-rotate": {
		create:              func() Plugin { return &log_rotate.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeSystem,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"error-log-logger": {
		create:              func() Plugin { return &error_log_logger.Plugin{} },
		phases:              0,
		scopes:              scopeSystem,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"sls-logger": {
		create:              func() Plugin { return &sls_logger.Plugin{} },
		phases:              phaseLog,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"google-cloud-logging": {
		create:              func() Plugin { return &google_cloud_logging.Plugin{} },
		phases:              phaseLog,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"splunk-hec-logging": {
		create:              func() Plugin { return &splunk_hec_logging.Plugin{} },
		phases:              phaseLog,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"file-logger": {
		create:              func() Plugin { return &file_logger.Plugin{} },
		phases:              phaseLog,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"loggly": {
		create:              func() Plugin { return &loggly.Plugin{} },
		phases:              phaseLog,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"elasticsearch-logger": {
		create:              func() Plugin { return &elasticsearch_logger.Plugin{} },
		phases:              phaseLog,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"tencent-cloud-cls": {
		create:              func() Plugin { return &tencent_cloud_cls.Plugin{} },
		phases:              phaseLog,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"loki-logger": {
		create:              func() Plugin { return &loki_logger.Plugin{} },
		phases:              phaseLog,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"lago": {
		create:              func() Plugin { return &lago.Plugin{} },
		phases:              phaseLog,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
	"ai-aliyun-content-moderation": {
		create:              func() Plugin { return &ai_aliyun_content_moderation.Plugin{} },
		phases:              phaseAccess | phaseBodyFilter,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"ai-aws-content-moderation": {
		create:              func() Plugin { return &ai_aws_content_moderation.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"ai-rag": {
		create:              func() Plugin { return &ai_rag.Plugin{} },
		phases:              phaseRewrite,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"ai-prompt-decorator": {
		create:              func() Plugin { return &ai_prompt_decorator.Plugin{} },
		phases:              phaseRewrite,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"ai-prompt-guard": {
		create:              func() Plugin { return &ai_prompt_guard.Plugin{} },
		phases:              phaseAccess,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"ai-prompt-template": {
		create:              func() Plugin { return &ai_prompt_template.Plugin{} },
		phases:              phaseRewrite,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"ai-request-rewrite": {
		create:              func() Plugin { return &ai_request_rewrite.Plugin{} },
		phases:              phaseRewrite,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"ai-rate-limiting": {
		create:              func() Plugin { return &ai_rate_limiting.Plugin{} },
		phases:              phaseAccess | phaseBodyFilter | phaseFinalizer,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"ai-proxy": {
		create:              func() Plugin { return &ai_proxy.Plugin{} },
		phases:              phaseAccess | phaseBeforeProxy | phaseProtocol,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"ai-proxy-multi": {
		create:              func() Plugin { return &ai_proxy_multi.Plugin{} },
		phases:              phaseAccess | phaseBeforeProxy | phaseProtocol,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"mcp-bridge": {
		create:              func() Plugin { return &mcp_bridge.Plugin{} },
		phases:              phaseAccess | phaseProtocol,
		scopes:              scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: true,
		domain:              DomainHTTP,
	},
	"mqtt-proxy": {
		create:              func() Plugin { return &mqtt_proxy.Plugin{} },
		phases:              phaseProtocol,
		scopes:              scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainStream,
	},
	"example-plugin": {
		create:              func() Plugin { return &example_plugin.Plugin{} },
		phases:              phaseRewrite,
		scopes:              scopeSystem | scopeGlobal | scopeRoute | scopeConsumer,
		instanceScope:       InstanceEffectiveConfig,
		conditionalTerminal: false,
		domain:              DomainHTTP,
	},
}
