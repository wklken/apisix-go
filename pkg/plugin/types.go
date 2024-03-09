package plugin

import "net/http"

type Plugin interface {
	Init(pc interface{}) error
	Schema() string
	Handler(next http.Handler) http.Handler
	Priority() int
}
