package basic_auth

import (
	"fmt"
	"net/http"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	// version  = "0.1"
	priority = 102
	name     = "basic_auth"
)

const schema = `
{
	"$schema": "http://json-schema.org/draft-04/schema#",
	"type": "object",
	"properties": {
	  "credentials": {
		"type": "object"
	  },
	  "realm": {
		"type": "string"
	  }
	},
	"required": [
	  "credentials"
	]
  }
`

type Config struct {
	Credentials map[string]string `json:"credentials"`
	Realm       string            `json:"realm"`
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

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok {
			basicAuthFailed(w, p.config.Realm)
			return
		}

		credPass, credUserOk := p.config.Credentials[user]
		if !credUserOk || pass != credPass {
			basicAuthFailed(w, p.config.Realm)
			return
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func basicAuthFailed(w http.ResponseWriter, realm string) {
	w.Header().Add("WWW-Authenticate", fmt.Sprintf(`Basic realm="%s"`, realm))
	w.WriteHeader(http.StatusUnauthorized)
}
