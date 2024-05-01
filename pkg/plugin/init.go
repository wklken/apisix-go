package plugin

import (
	"fmt"
	"sort"

	"github.com/justinas/alice"
	"github.com/wklken/apisix-go/pkg/plugin/api_breaker"
	"github.com/wklken/apisix-go/pkg/plugin/basic_auth"
	"github.com/wklken/apisix-go/pkg/plugin/client_control"
	"github.com/wklken/apisix-go/pkg/plugin/consumer_restriction"
	"github.com/wklken/apisix-go/pkg/plugin/cors"
	"github.com/wklken/apisix-go/pkg/plugin/csrf"
	"github.com/wklken/apisix-go/pkg/plugin/fault_injection"
	"github.com/wklken/apisix-go/pkg/plugin/file_logger"
	"github.com/wklken/apisix-go/pkg/plugin/gzip"
	"github.com/wklken/apisix-go/pkg/plugin/http_logger"
	"github.com/wklken/apisix-go/pkg/plugin/ip_restriction"
	"github.com/wklken/apisix-go/pkg/plugin/key_auth"
	"github.com/wklken/apisix-go/pkg/plugin/limit_count"
	"github.com/wklken/apisix-go/pkg/plugin/mocking"
	"github.com/wklken/apisix-go/pkg/plugin/otel"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_rewrite"
	"github.com/wklken/apisix-go/pkg/plugin/real_ip"
	"github.com/wklken/apisix-go/pkg/plugin/redirect"
	"github.com/wklken/apisix-go/pkg/plugin/referer_restriction"
	"github.com/wklken/apisix-go/pkg/plugin/request_context"
	"github.com/wklken/apisix-go/pkg/plugin/request_id"
	"github.com/wklken/apisix-go/pkg/plugin/request_validation"
	"github.com/wklken/apisix-go/pkg/plugin/syslog"
	"github.com/wklken/apisix-go/pkg/plugin/ua_restriction"
	"github.com/wklken/apisix-go/pkg/plugin/udp_logger"
	"github.com/wklken/apisix-go/pkg/plugin/uri_blocker"
)

func New(name string) Plugin {
	// fmt.Println("plugin name:", name)
	// FIXME: auto detecting the plugins under dir `plugin`
	switch name {
	case "file-logger":
		return &file_logger.Plugin{}
	case "otel":
		return &otel.Plugin{}
	case "proxy-rewrite":
		return &proxy_rewrite.Plugin{}
	case "mocking":
		return &mocking.Plugin{}
	case "client-control":
		return &client_control.Plugin{}
	case "request-id":
		return &request_id.Plugin{}
	case "uri-blocker":
		return &uri_blocker.Plugin{}
	case "limit-count":
		return &limit_count.Plugin{}
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
	case "key-auth":
		return &key_auth.Plugin{}
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
	case "consumer-restriction":
		return &consumer_restriction.Plugin{}
	case "http-logger":
		return &http_logger.Plugin{}
	case "udp-logger":
		return &udp_logger.Plugin{}
	case "syslog":
		return &syslog.Plugin{}
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
