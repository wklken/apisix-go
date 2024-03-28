package plugin

import "net/http"

type Plugin interface {
	Init() error
	PostInit() error
	Handler(next http.Handler) http.Handler
	Config() interface{}
	GetSchema() string
	GetPriority() int
	GetName() string
}
