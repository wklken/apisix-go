package plugin

import (
	"cmp"
	"slices"

	"github.com/justinas/alice"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

// New returns the plugin registered for name, or nil for unknown names.
func New(name string) Plugin {
	factory, ok := pluginRegistry[name]
	if !ok {
		return nil
	}
	return factory()
}

func BuildPluginChain(plugins ...Plugin) alice.Chain {
	// Copy before sorting so the caller's backing array is not reordered.
	plugins = append([]Plugin(nil), plugins...)
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
