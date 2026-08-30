package plugin

import (
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type dependencyReceiver interface {
	SetDependencies(base.Dependencies)
}

// New returns the plugin registered for name, or nil for unknown names.
func New(name string, deps base.Dependencies) Plugin {
	registered, ok := pluginRegistry[name]
	if !ok {
		return nil
	}
	p := registered.create()
	receiver, ok := p.(dependencyReceiver)
	if !ok {
		panic("registered plugin does not embed base.BasePlugin")
	}
	receiver.SetDependencies(deps)
	return p
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
