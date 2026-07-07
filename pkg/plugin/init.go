package plugin

import (
	"fmt"
	"sort"

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

func New(name string) Plugin {
	// fmt.Println("plugin name:", name)
	// FIXME: auto detecting the plugins under dir `plugin`
	switch name {
	case "ai":
		return &ai.Plugin{}
	case "ai-prompt-decorator":
		return &ai_prompt_decorator.Plugin{}
	case "ai-prompt-guard":
		return &ai_prompt_guard.Plugin{}
	case "ai-prompt-template":
		return &ai_prompt_template.Plugin{}
	case "ai-aliyun-content-moderation":
		return &ai_aliyun_content_moderation.Plugin{}
	case "ai-aws-content-moderation":
		return &ai_aws_content_moderation.Plugin{}
	case "ai-proxy":
		return &ai_proxy.Plugin{}
	case "ai-proxy-multi":
		return &ai_proxy_multi.Plugin{}
	case "ai-rag":
		return &ai_rag.Plugin{}
	case "ai-rate-limiting":
		return &ai_rate_limiting.Plugin{}
	case "ai-request-rewrite":
		return &ai_request_rewrite.Plugin{}
	case "batch-requests":
		return &batch_requests.Plugin{}
	case "aws-lambda":
		return &aws_lambda.Plugin{}
	case "azure-functions":
		return &azure_functions.Plugin{}
	case "attach-consumer-label":
		return &attach_consumer_label.Plugin{}
	case "brotli":
		return &brotli.Plugin{}
	case "file-logger":
		return &file_logger.Plugin{}
	case "echo":
		return &echo.Plugin{}
	case "acl":
		return &acl.Plugin{}
	case "authz-casbin":
		return &authz_casbin.Plugin{}
	case "authz-casdoor":
		return &authz_casdoor.Plugin{}
	case "authz-keycloak":
		return &authz_keycloak.Plugin{}
	case "error-log-logger":
		return &error_log_logger.Plugin{}
	case "error-page":
		return &error_page.Plugin{}
	case "exit-transformer":
		return &exit_transformer.Plugin{}
	case "example-plugin":
		return &example_plugin.Plugin{}
	case "feishu-auth":
		return &feishu_auth.Plugin{}
	case "cas-auth":
		return &cas_auth.Plugin{}
	case "chaitin-waf":
		return &chaitin_waf.Plugin{}
	case "forward-auth":
		return &forward_auth.Plugin{}
	case "gm":
		return &gm.Plugin{}
	case "otel":
		return &otel.Plugin{}
	case "opa":
		return &opa.Plugin{}
	case "proxy-rewrite":
		return &proxy_rewrite.Plugin{}
	case "response-rewrite":
		return &response_rewrite.Plugin{}
	case "body-transformer":
		return &body_transformer.Plugin{}
	case "degraphql":
		return &degraphql.Plugin{}
	case "dingtalk-auth":
		return &dingtalk_auth.Plugin{}
	case "dubbo-proxy":
		return &dubbo_proxy.Plugin{}
	case "http-dubbo":
		return &http_dubbo.Plugin{}
	case "graphql-limit-count":
		return &graphql_limit_count.Plugin{}
	case "graphql-proxy-cache":
		return &graphql_proxy_cache.Plugin{}
	case "grpc-transcode":
		return &grpc_transcode.Plugin{}
	case "grpc-web":
		return &grpc_web.Plugin{}
	case "public-api":
		return &public_api.Plugin{}
	case "proxy-mirror":
		return &proxy_mirror.Plugin{}
	case "proxy-control":
		return &proxy_control.Plugin{}
	case "proxy-buffering":
		return &proxy_buffering.Plugin{}
	case "proxy-cache":
		return &proxy_cache.Plugin{}
	case "mocking":
		return &mocking.Plugin{}
	case "node-status":
		return &node_status.Plugin{}
	case "openfunction":
		return &openfunction.Plugin{}
	case "openwhisk":
		return &openwhisk.Plugin{}
	case "openid-connect":
		return &openid_connect.Plugin{}
	case "oas-validator":
		return &oas_validator.Plugin{}
	case "server-info":
		return &server_info.Plugin{}
	case "serverless-pre-function":
		return serverless.NewPreFunction()
	case "serverless-post-function":
		return serverless.NewPostFunction()
	case "opentelemetry":
		return &otel.Plugin{}
	case "prometheus":
		return &prometheus.Plugin{}
	case "client-control":
		return &client_control.Plugin{}
	case "request-id":
		return &request_id.Plugin{}
	case "uri-blocker":
		return &uri_blocker.Plugin{}
	case "limit-req":
		return &limit_req.Plugin{}
	case "limit-conn":
		return &limit_conn.Plugin{}
	case "limit-count":
		return &limit_count.Plugin{}
	case "multi-auth":
		return &multi_auth.Plugin{}
	case "wolf-rbac":
		return &wolf_rbac.Plugin{}
	case "traffic-split":
		return &traffic_split.Plugin{}
	case "traffic-label":
		return &traffic_label.Plugin{}
	case "workflow":
		return &workflow.Plugin{}
	case "log-rotate":
		return &log_rotate.Plugin{}
	case "loggly":
		return &loggly.Plugin{}
	case "loki-logger":
		return &loki_logger.Plugin{}
	case "mcp-bridge":
		return &mcp_bridge.Plugin{}
	case "mqtt-proxy":
		return &mqtt_proxy.Plugin{}
	case "splunk-hec-logging":
		return &splunk_hec_logging.Plugin{}
	case "clickhouse-logger":
		return &clickhouse_logger.Plugin{}
	case "skywalking-logger":
		return &skywalking_logger.Plugin{}
	case "sls-logger":
		return &sls_logger.Plugin{}
	case "google-cloud-logging":
		return &google_cloud_logging.Plugin{}
	case "zipkin":
		return &zipkin.Plugin{}
	case "datadog":
		return &datadog.Plugin{}
	case "lago":
		return &lago.Plugin{}
	case "skywalking":
		return &skywalking.Plugin{}
	case "kafka-logger":
		return &kafka_logger.Plugin{}
	case "kafka-proxy":
		return &kafka_proxy.Plugin{}
	case "rocketmq-logger":
		return &rocketmq_logger.Plugin{}
	case "saml-auth":
		return &saml_auth.Plugin{}
	case "tencent-cloud-cls":
		return &tencent_cloud_cls.Plugin{}
	case "api-breaker":
		return &api_breaker.Plugin{}
	case "gzip":
		return &gzip.Plugin{}
	case "referer-restriction":
		return &referer_restriction.Plugin{}
	case "ua-restriction":
		return &ua_restriction.Plugin{}
	case "real-ip":
		return &real_ip.Plugin{}
	case "ip-restriction":
		return &ip_restriction.Plugin{}
	case "basic-auth":
		return &basic_auth.Plugin{}
	case "jwe-decrypt":
		return &jwe_decrypt.Plugin{}
	case "hmac-auth":
		return &hmac_auth.Plugin{}
	case "jwt-auth":
		return &jwt_auth.Plugin{}
	case "key-auth":
		return &key_auth.Plugin{}
	case "ldap-auth":
		return &ldap_auth.Plugin{}
	case "request-context":
		return &request_context.Plugin{}
	case "cors":
		return &cors.Plugin{}
	case "request-validation":
		return &request_validation.Plugin{}
	case "fault-injection":
		return &fault_injection.Plugin{}
	case "redirect":
		return &redirect.Plugin{}
	case "csrf":
		return &csrf.Plugin{}
	case "data-mask":
		return &data_mask.Plugin{}
	case "consumer-restriction":
		return &consumer_restriction.Plugin{}
	case "http-logger":
		return &http_logger.Plugin{}
	case "udp-logger":
		return &udp_logger.Plugin{}
	case "syslog":
		return &syslog.Plugin{}
	case "tcp-logger":
		return &tcp_logger.Plugin{}
	case "elasticsearch-logger":
		return &elasticsearch_logger.Plugin{}
	}
	return nil
}

func BuildPluginChain(plugins ...Plugin) alice.Chain {
	// sort the plugin by priority
	sort.Slice(plugins, func(i, j int) bool {
		return plugins[i].GetPriority() > plugins[j].GetPriority()
	})

	// build the alice chain
	chain := alice.New()
	// chain = chain.Append(Recoverer)
	for _, plugin := range plugins {
		fmt.Println("plugin name:", plugin.GetName(), "priority:", plugin.GetPriority())
		chain = chain.Append(plugin.Handler)
	}

	return chain
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
