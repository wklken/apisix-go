package basic_auth

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/santhosh-tekuri/jsonschema/v5"
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

func (p *Plugin) Init(config string) error {
	fmt.Println("init the basic_auth plugin", config)
	// v := viper.New()
	// v.SetConfigType("json")

	sch, err := jsonschema.CompileString("schema.json", schema)
	if err != nil {
		log.Fatalf("--- compile json schema fail: %#v", err)
	}

	var v interface{}
	if err := json.Unmarshal([]byte(config), &v); err != nil {
		log.Fatalf("--- unmarshal string fail: %#v", err)
	}
	fmt.Printf("the config %+v\n", v)
	if err = sch.Validate(v); err != nil {
		log.Fatalf("--- validate fail: %#v", err)
	}

	var c Config
	if err := json.Unmarshal([]byte(config), &c); err != nil {
		log.Fatalf("--- unmarshal config string fail: %#v", err)
	}

	// TODO: how to make the default value
	// v.SetDefault("header_name", "X-Request-ID")
	// v.SetDefault("set_in_response", true)

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
