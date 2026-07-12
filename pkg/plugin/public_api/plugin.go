package public_api

import (
	"net/http"
	"sync"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 501
	name     = "public-api"
)

const schema = `
{
  "type": "object",
  "properties": {
    "uri": {
      "type": "string"
    }
  }
}
`

type Config struct {
	URI string `json:"uri,omitempty"`
}

type registryKey struct {
	method string
	uri    string
}

var publicAPIs = struct {
	sync.RWMutex
	handlers map[registryKey]http.Handler
}{
	handlers: map[registryKey]http.Handler{},
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) PostInit() error {
	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri := p.config.URI
		if uri == "" {
			uri = r.URL.Path
		}
		handler := Lookup(r.Method, uri)
		if handler == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		req := r.Clone(r.Context())
		req.URL.Path = uri
		req.URL.RawPath = ""
		handler.ServeHTTP(w, req)
	})
}

func Register(method string, uri string, handler http.Handler) {
	publicAPIs.Lock()
	defer publicAPIs.Unlock()
	publicAPIs.handlers[registryKey{method: method, uri: uri}] = handler
}

func Lookup(method string, uri string) http.Handler {
	publicAPIs.RLock()
	defer publicAPIs.RUnlock()
	return publicAPIs.handlers[registryKey{method: method, uri: uri}]
}

func ResetRegistryForTest() {
	publicAPIs.Lock()
	defer publicAPIs.Unlock()
	publicAPIs.handlers = map[registryKey]http.Handler{}
}
