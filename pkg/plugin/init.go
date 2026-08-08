package plugin

import (
	"cmp"
	"slices"

	"github.com/justinas/alice"
	"github.com/wklken/apisix-go/pkg/plugin/acl"
	"github.com/wklken/apisix-go/pkg/plugin/ai"
	"github.com/wklken/apisix-go/pkg/plugin/ai_aliyun_content_moderation"
	"github.com/wklken/apisix-go/pkg/plugin/ai_aws_content_moderation"
	"github.com/wklken/apisix-go/pkg/plugin/ai_prompt_decorator"
	"github.com/wklken/apisix-go/pkg/plugin/ai_prompt_guard"
	"github.com/wklken/apisix-go/pkg/plugin/ai_prompt_template"
	"github.com/wklken/apisix-go/pkg/plugin/ai_proxy"
	"github.com/wklken/apisix-go/pkg/plugin/ai_proxy_multi"
	"github.com/wklken/apisix-go/pkg/plugin/ai_rag"
	"github.com/wklken/apisix-go/pkg/plugin/ai_rate_limiting"
	"github.com/wklken/apisix-go/pkg/plugin/ai_request_rewrite"
	"github.com/wklken/apisix-go/pkg/plugin/api_breaker"
	"github.com/wklken/apisix-go/pkg/plugin/attach_consumer_label"
	"github.com/wklken/apisix-go/pkg/plugin/authz_casbin"
	"github.com/wklken/apisix-go/pkg/plugin/authz_casdoor"
	"github.com/wklken/apisix-go/pkg/plugin/authz_keycloak"
	"github.com/wklken/apisix-go/pkg/plugin/aws_lambda"
	"github.com/wklken/apisix-go/pkg/plugin/azure_functions"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/basic_auth"
	"github.com/wklken/apisix-go/pkg/plugin/batch_requests"
	"github.com/wklken/apisix-go/pkg/plugin/body_transformer"
	"github.com/wklken/apisix-go/pkg/plugin/brotli"
	"github.com/wklken/apisix-go/pkg/plugin/cas_auth"
	"github.com/wklken/apisix-go/pkg/plugin/chaitin_waf"
	"github.com/wklken/apisix-go/pkg/plugin/clickhouse_logger"
	"github.com/wklken/apisix-go/pkg/plugin/client_control"
	"github.com/wklken/apisix-go/pkg/plugin/consumer_restriction"
	"github.com/wklken/apisix-go/pkg/plugin/cors"
	"github.com/wklken/apisix-go/pkg/plugin/csrf"
	"github.com/wklken/apisix-go/pkg/plugin/data_mask"
	"github.com/wklken/apisix-go/pkg/plugin/datadog"
	"github.com/wklken/apisix-go/pkg/plugin/degraphql"
	"github.com/wklken/apisix-go/pkg/plugin/dingtalk_auth"
	"github.com/wklken/apisix-go/pkg/plugin/dubbo_proxy"
	"github.com/wklken/apisix-go/pkg/plugin/echo"
	"github.com/wklken/apisix-go/pkg/plugin/elasticsearch_logger"
	"github.com/wklken/apisix-go/pkg/plugin/error_log_logger"
	"github.com/wklken/apisix-go/pkg/plugin/error_page"
	"github.com/wklken/apisix-go/pkg/plugin/example_plugin"
	"github.com/wklken/apisix-go/pkg/plugin/exit_transformer"
	"github.com/wklken/apisix-go/pkg/plugin/fault_injection"
	"github.com/wklken/apisix-go/pkg/plugin/feishu_auth"
	"github.com/wklken/apisix-go/pkg/plugin/file_logger"
	"github.com/wklken/apisix-go/pkg/plugin/forward_auth"
	"github.com/wklken/apisix-go/pkg/plugin/gm"
	"github.com/wklken/apisix-go/pkg/plugin/google_cloud_logging"
	"github.com/wklken/apisix-go/pkg/plugin/graphql_limit_count"
	"github.com/wklken/apisix-go/pkg/plugin/graphql_proxy_cache"
	"github.com/wklken/apisix-go/pkg/plugin/grpc_transcode"
	"github.com/wklken/apisix-go/pkg/plugin/grpc_web"
	"github.com/wklken/apisix-go/pkg/plugin/gzip"
	"github.com/wklken/apisix-go/pkg/plugin/hmac_auth"
	"github.com/wklken/apisix-go/pkg/plugin/http_dubbo"
	"github.com/wklken/apisix-go/pkg/plugin/http_logger"
	"github.com/wklken/apisix-go/pkg/plugin/ip_restriction"
	"github.com/wklken/apisix-go/pkg/plugin/jwe_decrypt"
	"github.com/wklken/apisix-go/pkg/plugin/jwt_auth"
	"github.com/wklken/apisix-go/pkg/plugin/kafka_logger"
	"github.com/wklken/apisix-go/pkg/plugin/kafka_proxy"
	"github.com/wklken/apisix-go/pkg/plugin/key_auth"
	"github.com/wklken/apisix-go/pkg/plugin/lago"
	"github.com/wklken/apisix-go/pkg/plugin/ldap_auth"
	"github.com/wklken/apisix-go/pkg/plugin/limit_conn"
	"github.com/wklken/apisix-go/pkg/plugin/limit_count"
	"github.com/wklken/apisix-go/pkg/plugin/limit_req"
	"github.com/wklken/apisix-go/pkg/plugin/log_rotate"
	"github.com/wklken/apisix-go/pkg/plugin/loggly"
	"github.com/wklken/apisix-go/pkg/plugin/loki_logger"
	"github.com/wklken/apisix-go/pkg/plugin/mcp_bridge"
	"github.com/wklken/apisix-go/pkg/plugin/mocking"
	"github.com/wklken/apisix-go/pkg/plugin/mqtt_proxy"
	"github.com/wklken/apisix-go/pkg/plugin/multi_auth"
	"github.com/wklken/apisix-go/pkg/plugin/node_status"
	"github.com/wklken/apisix-go/pkg/plugin/oas_validator"
	"github.com/wklken/apisix-go/pkg/plugin/opa"
	"github.com/wklken/apisix-go/pkg/plugin/openfunction"
	"github.com/wklken/apisix-go/pkg/plugin/openid_connect"
	"github.com/wklken/apisix-go/pkg/plugin/openwhisk"
	"github.com/wklken/apisix-go/pkg/plugin/otel"
	"github.com/wklken/apisix-go/pkg/plugin/prometheus"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_buffering"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_cache"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_control"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_mirror"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_rewrite"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/plugin/real_ip"
	"github.com/wklken/apisix-go/pkg/plugin/redirect"
	"github.com/wklken/apisix-go/pkg/plugin/referer_restriction"
	"github.com/wklken/apisix-go/pkg/plugin/request_context"
	"github.com/wklken/apisix-go/pkg/plugin/request_id"
	"github.com/wklken/apisix-go/pkg/plugin/request_validation"
	"github.com/wklken/apisix-go/pkg/plugin/response_rewrite"
	"github.com/wklken/apisix-go/pkg/plugin/rocketmq_logger"
	"github.com/wklken/apisix-go/pkg/plugin/saml_auth"
	"github.com/wklken/apisix-go/pkg/plugin/server_info"
	"github.com/wklken/apisix-go/pkg/plugin/serverless"
	"github.com/wklken/apisix-go/pkg/plugin/skywalking"
	"github.com/wklken/apisix-go/pkg/plugin/skywalking_logger"
	"github.com/wklken/apisix-go/pkg/plugin/sls_logger"
	"github.com/wklken/apisix-go/pkg/plugin/splunk_hec_logging"
	"github.com/wklken/apisix-go/pkg/plugin/syslog"
	"github.com/wklken/apisix-go/pkg/plugin/tcp_logger"
	"github.com/wklken/apisix-go/pkg/plugin/tencent_cloud_cls"
	"github.com/wklken/apisix-go/pkg/plugin/traffic_label"
	"github.com/wklken/apisix-go/pkg/plugin/traffic_split"
	"github.com/wklken/apisix-go/pkg/plugin/ua_restriction"
	"github.com/wklken/apisix-go/pkg/plugin/udp_logger"
	"github.com/wklken/apisix-go/pkg/plugin/uri_blocker"
	"github.com/wklken/apisix-go/pkg/plugin/wolf_rbac"
	"github.com/wklken/apisix-go/pkg/plugin/workflow"
	"github.com/wklken/apisix-go/pkg/plugin/zipkin"
)

var pluginRegistry = map[string]func() Plugin{
	"ai":                           func() Plugin { return &ai.Plugin{} },
	"ai-prompt-decorator":          func() Plugin { return &ai_prompt_decorator.Plugin{} },
	"ai-prompt-guard":              func() Plugin { return &ai_prompt_guard.Plugin{} },
	"ai-prompt-template":           func() Plugin { return &ai_prompt_template.Plugin{} },
	"ai-aliyun-content-moderation": func() Plugin { return &ai_aliyun_content_moderation.Plugin{} },
	"ai-aws-content-moderation":    func() Plugin { return &ai_aws_content_moderation.Plugin{} },
	"ai-proxy":                     func() Plugin { return &ai_proxy.Plugin{} },
	"ai-proxy-multi":               func() Plugin { return &ai_proxy_multi.Plugin{} },
	"ai-rag":                       func() Plugin { return &ai_rag.Plugin{} },
	"ai-rate-limiting":             func() Plugin { return &ai_rate_limiting.Plugin{} },
	"ai-request-rewrite":           func() Plugin { return &ai_request_rewrite.Plugin{} },
	"batch-requests":               func() Plugin { return &batch_requests.Plugin{} },
	"aws-lambda":                   func() Plugin { return &aws_lambda.Plugin{} },
	"azure-functions":              func() Plugin { return &azure_functions.Plugin{} },
	"attach-consumer-label":        func() Plugin { return &attach_consumer_label.Plugin{} },
	"brotli":                       func() Plugin { return &brotli.Plugin{} },
	"file-logger":                  func() Plugin { return &file_logger.Plugin{} },
	"echo":                         func() Plugin { return &echo.Plugin{} },
	"acl":                          func() Plugin { return &acl.Plugin{} },
	"authz-casbin":                 func() Plugin { return &authz_casbin.Plugin{} },
	"authz-casdoor":                func() Plugin { return &authz_casdoor.Plugin{} },
	"authz-keycloak":               func() Plugin { return &authz_keycloak.Plugin{} },
	"error-log-logger":             func() Plugin { return &error_log_logger.Plugin{} },
	"error-page":                   func() Plugin { return &error_page.Plugin{} },
	"exit-transformer":             func() Plugin { return &exit_transformer.Plugin{} },
	"example-plugin":               func() Plugin { return &example_plugin.Plugin{} },
	"feishu-auth":                  func() Plugin { return &feishu_auth.Plugin{} },
	"cas-auth":                     func() Plugin { return &cas_auth.Plugin{} },
	"chaitin-waf":                  func() Plugin { return &chaitin_waf.Plugin{} },
	"forward-auth":                 func() Plugin { return &forward_auth.Plugin{} },
	"gm":                           func() Plugin { return &gm.Plugin{} },
	"otel":                         func() Plugin { return &otel.Plugin{} },
	"opa":                          func() Plugin { return &opa.Plugin{} },
	"proxy-rewrite":                func() Plugin { return &proxy_rewrite.Plugin{} },
	"response-rewrite":             func() Plugin { return &response_rewrite.Plugin{} },
	"body-transformer":             func() Plugin { return &body_transformer.Plugin{} },
	"degraphql":                    func() Plugin { return &degraphql.Plugin{} },
	"dingtalk-auth":                func() Plugin { return &dingtalk_auth.Plugin{} },
	"dubbo-proxy":                  func() Plugin { return &dubbo_proxy.Plugin{} },
	"http-dubbo":                   func() Plugin { return &http_dubbo.Plugin{} },
	"graphql-limit-count":          func() Plugin { return &graphql_limit_count.Plugin{} },
	"graphql-proxy-cache":          func() Plugin { return &graphql_proxy_cache.Plugin{} },
	"grpc-transcode":               func() Plugin { return &grpc_transcode.Plugin{} },
	"grpc-web":                     func() Plugin { return &grpc_web.Plugin{} },
	"public-api":                   func() Plugin { return &public_api.Plugin{} },
	"proxy-mirror":                 func() Plugin { return &proxy_mirror.Plugin{} },
	"proxy-control":                func() Plugin { return &proxy_control.Plugin{} },
	"proxy-buffering":              func() Plugin { return &proxy_buffering.Plugin{} },
	"proxy-cache":                  func() Plugin { return &proxy_cache.Plugin{} },
	"mocking":                      func() Plugin { return &mocking.Plugin{} },
	"node-status":                  func() Plugin { return &node_status.Plugin{} },
	"openfunction":                 func() Plugin { return &openfunction.Plugin{} },
	"openwhisk":                    func() Plugin { return &openwhisk.Plugin{} },
	"openid-connect":               func() Plugin { return &openid_connect.Plugin{} },
	"oas-validator":                func() Plugin { return &oas_validator.Plugin{} },
	"server-info":                  func() Plugin { return &server_info.Plugin{} },
	"serverless-pre-function":      func() Plugin { return serverless.NewPreFunction() },
	"serverless-post-function":     func() Plugin { return serverless.NewPostFunction() },
	"opentelemetry":                func() Plugin { return &otel.Plugin{} },
	"prometheus":                   func() Plugin { return &prometheus.Plugin{} },
	"client-control":               func() Plugin { return &client_control.Plugin{} },
	"request-id":                   func() Plugin { return &request_id.Plugin{} },
	"uri-blocker":                  func() Plugin { return &uri_blocker.Plugin{} },
	"limit-req":                    func() Plugin { return &limit_req.Plugin{} },
	"limit-conn":                   func() Plugin { return &limit_conn.Plugin{} },
	"limit-count":                  func() Plugin { return &limit_count.Plugin{} },
	"multi-auth":                   func() Plugin { return &multi_auth.Plugin{} },
	"wolf-rbac":                    func() Plugin { return &wolf_rbac.Plugin{} },
	"traffic-split":                func() Plugin { return &traffic_split.Plugin{} },
	"traffic-label":                func() Plugin { return &traffic_label.Plugin{} },
	"workflow":                     func() Plugin { return &workflow.Plugin{} },
	"log-rotate":                   func() Plugin { return &log_rotate.Plugin{} },
	"loggly":                       func() Plugin { return &loggly.Plugin{} },
	"loki-logger":                  func() Plugin { return &loki_logger.Plugin{} },
	"mcp-bridge":                   func() Plugin { return &mcp_bridge.Plugin{} },
	"mqtt-proxy":                   func() Plugin { return &mqtt_proxy.Plugin{} },
	"splunk-hec-logging":           func() Plugin { return &splunk_hec_logging.Plugin{} },
	"clickhouse-logger":            func() Plugin { return &clickhouse_logger.Plugin{} },
	"skywalking-logger":            func() Plugin { return &skywalking_logger.Plugin{} },
	"sls-logger":                   func() Plugin { return &sls_logger.Plugin{} },
	"google-cloud-logging":         func() Plugin { return &google_cloud_logging.Plugin{} },
	"zipkin":                       func() Plugin { return &zipkin.Plugin{} },
	"datadog":                      func() Plugin { return &datadog.Plugin{} },
	"lago":                         func() Plugin { return &lago.Plugin{} },
	"skywalking":                   func() Plugin { return &skywalking.Plugin{} },
	"kafka-logger":                 func() Plugin { return &kafka_logger.Plugin{} },
	"kafka-proxy":                  func() Plugin { return &kafka_proxy.Plugin{} },
	"rocketmq-logger":              func() Plugin { return &rocketmq_logger.Plugin{} },
	"saml-auth":                    func() Plugin { return &saml_auth.Plugin{} },
	"tencent-cloud-cls":            func() Plugin { return &tencent_cloud_cls.Plugin{} },
	"api-breaker":                  func() Plugin { return &api_breaker.Plugin{} },
	"gzip":                         func() Plugin { return &gzip.Plugin{} },
	"referer-restriction":          func() Plugin { return &referer_restriction.Plugin{} },
	"ua-restriction":               func() Plugin { return &ua_restriction.Plugin{} },
	"real-ip":                      func() Plugin { return &real_ip.Plugin{} },
	"ip-restriction":               func() Plugin { return &ip_restriction.Plugin{} },
	"basic-auth":                   func() Plugin { return &basic_auth.Plugin{} },
	"jwe-decrypt":                  func() Plugin { return &jwe_decrypt.Plugin{} },
	"hmac-auth":                    func() Plugin { return &hmac_auth.Plugin{} },
	"jwt-auth":                     func() Plugin { return &jwt_auth.Plugin{} },
	"key-auth":                     func() Plugin { return &key_auth.Plugin{} },
	"ldap-auth":                    func() Plugin { return &ldap_auth.Plugin{} },
	"request-context":              func() Plugin { return &request_context.Plugin{} },
	"cors":                         func() Plugin { return &cors.Plugin{} },
	"request-validation":           func() Plugin { return &request_validation.Plugin{} },
	"fault-injection":              func() Plugin { return &fault_injection.Plugin{} },
	"redirect":                     func() Plugin { return &redirect.Plugin{} },
	"csrf":                         func() Plugin { return &csrf.Plugin{} },
	"data-mask":                    func() Plugin { return &data_mask.Plugin{} },
	"consumer-restriction":         func() Plugin { return &consumer_restriction.Plugin{} },
	"http-logger":                  func() Plugin { return &http_logger.Plugin{} },
	"udp-logger":                   func() Plugin { return &udp_logger.Plugin{} },
	"syslog":                       func() Plugin { return &syslog.Plugin{} },
	"tcp-logger":                   func() Plugin { return &tcp_logger.Plugin{} },
	"elasticsearch-logger":         func() Plugin { return &elasticsearch_logger.Plugin{} },
}

// New returns the plugin registered for name, or nil for unknown names.
func New(name string) Plugin {
	factory, ok := pluginRegistry[name]
	if !ok {
		return nil
	}
	return factory()
}

func BuildPluginChain(plugins ...Plugin) alice.Chain {
	// sort the plugin by priority
	slices.SortFunc(plugins, func(a, b Plugin) int {
		return cmp.Compare(b.GetPriority(), a.GetPriority())
	})

	transformCount := 0
	for _, plugin := range plugins {
		if isResponseTransformPlugin(plugin.GetName()) {
			transformCount++
		}
	}

	// build the alice chain
	chain := alice.New(base.WithTransformPipeline(transformCount))
	// chain = chain.Append(Recoverer)
	for _, plugin := range plugins {
		chain = chain.Append(plugin.Handler)
	}

	return chain
}

func isResponseTransformPlugin(name string) bool {
	switch name {
	case "proxy-cache", "echo", "response-rewrite", "serverless-pre-function", "serverless-post-function",
		"brotli", "ai-rate-limiting", "grpc-transcode", "exit-transformer", "body-transformer",
		"error-page", "graphql-proxy-cache":
		return true
	default:
		return false
	}
}

// func Recoverer(next http.Handler) http.Handler {
// 	fn := func(w http.ResponseWriter, r *http.Request) {
// 		defer func() {
// 			fmt.Println("calling recover")
// 			if rvr := recover(); rvr != nil {
// 				fmt.Println("recover:", rvr)
// 				var err error
// 				switch x := rvr.(type) {
// 				case string:
// 					err = errors.New(x)
// 				case error:
// 					err = x
// 				default:
// 					panic(rvr)
// 					// Fallback err (per specs, error strings should be lowercase w/o punctuation
// 					// err = errors.New("unknown panic")
// 				}

// 				if err.Error() == "http: request body too large" {
// 					w.WriteHeader(http.StatusRequestEntityTooLarge)
// 				} else {
// 					panic(rvr)
// 				}
// 			}
// 		}()

// 		next.ServeHTTP(w, r)
// 	}

// 	return http.HandlerFunc(fn)
// }
