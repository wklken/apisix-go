package plugin

import "net/http"

type Plugin interface {
	Init(config string) error
	Handler(next http.Handler) http.Handler
	Priority() int
}
