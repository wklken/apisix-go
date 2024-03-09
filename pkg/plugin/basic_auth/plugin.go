package basic_auth

import (
	"fmt"
	"net/http"

	plugin_config "github.com/wklken/apisix-go/pkg/plugin/config"
)

const (
	// version  = "0.1"
	priority = 102
	name     = "basic_auth"
)

type Plugin struct {
	config Config
}

// FIXME: use jsonschema to unmarshal the config dynamic

// 1. use json schema? => gojsonschema => 更通用
// 2. use go struct tag? => validator => go native
//
//	const schema = `{
//		"credentials": {
//			"type": "object",
//		},
//		"realm": {
//			"type": "string",
//			"required": false
//		}
//	}`
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

func (p *Plugin) Name() string {
	return name
}

func (p *Plugin) Priority() int {
	return priority
}

func (p *Plugin) Schema() string {
	return schema
}

func (p *Plugin) Init(pc interface{}) error {
	// j, err := json.Marshal(pc)
	// if err != nil {
	// 	return err
	// }

	// var c Config
	// err = json.Unmarshal(j, &c)
	// if err != nil {
	// 	return err
	// }
	var c Config
	plugin_config.Parse(pc, &c)

	p.config = c
	fmt.Printf("%s parsed config %+v\n", name, c)

	p.config = c

	return nil
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
