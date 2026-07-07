package example_plugin

import (
	"fmt"
	"net/http"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/public_api"
	"github.com/wklken/apisix-go/pkg/plugin/traffic_split"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 0
	name     = "example-plugin"

	helloURI = "/v1/plugin/example-plugin/hello"
)

const schema = `
{
  "type": "object",
  "properties": {
    "i": {
      "type": "number",
      "minimum": 0
    },
    "s": {
      "type": "string"
    },
    "t": {
      "type": "array",
      "minItems": 1
    },
    "ip": {
      "type": "string"
    },
    "port": {
      "type": "integer"
    }
  },
  "required": ["i"]
}
`

type Config struct {
	I    float64 `json:"i"`
	S    string  `json:"s,omitempty"`
	T    []any   `json:"t,omitempty"`
	IP   string  `json:"ip,omitempty"`
	Port int     `json:"port,omitempty"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	public_api.Register(http.MethodGet, helloURI, http.HandlerFunc(hello))
	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.config.IP != "" {
			r = traffic_split.WithOverride(r, &traffic_split.Override{
				Scheme: "http",
				Host:   fmt.Sprintf("%s:%d", p.config.IP, p.config.Port),
			})
		}
		next.ServeHTTP(w, r)
	})
}

func hello(w http.ResponseWriter, r *http.Request) {
	if _, ok := r.URL.Query()["json"]; ok {
		body, err := json.Marshal(map[string]string{"msg": "world"})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("world\n"))
}
