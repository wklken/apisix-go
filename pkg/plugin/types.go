package plugin

import "net/http"

type Plugin interface {
	Init() error
	Handler(next http.Handler) http.Handler
	Config() interface{}
	GetSchema() string
	GetPriority() int
	GetName() string
}
