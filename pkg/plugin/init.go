package plugin

import (
	"fmt"
	"sort"

	"github.com/justinas/alice"
	"github.com/wklken/apisix-go/pkg/plugin/basic_auth"
	"github.com/wklken/apisix-go/pkg/plugin/client_control"
	"github.com/wklken/apisix-go/pkg/plugin/file_logger"
	"github.com/wklken/apisix-go/pkg/plugin/mocking"
	"github.com/wklken/apisix-go/pkg/plugin/otel"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_rewrite"
	"github.com/wklken/apisix-go/pkg/plugin/request_id"
)

func New(name string) Plugin {
	fmt.Println("plugin name:", name)
	// FIXME: use the plugin name to do the register
	switch name {
	case "basic_auth":
		return &basic_auth.Plugin{}
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
		// TODO: proxy-rewrite
	}
	return nil
}

func BuildPluginChain(plugins ...Plugin) alice.Chain {
	// sort the plugin by priority
	sort.Slice(plugins, func(i, j int) bool {
		return plugins[i].GetPriority() < plugins[j].GetPriority()
	})

	// build the alice chain
	chain := alice.New()
	// chain = chain.Append(Recoverer)
	for _, plugin := range plugins {
		fmt.Println("plugin name:", plugin.GetName())
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
