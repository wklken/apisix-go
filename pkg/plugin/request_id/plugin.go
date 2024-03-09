package request_id

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gofrs/uuid"
	plugin_config "github.com/wklken/apisix-go/pkg/plugin/config"
)

const (
	// version  = "0.1"
	priority = 100
	name     = "request_id"
)

const schema = `
{
	"$schema": "http://json-schema.org/draft-04/schema#",
	"type": "object",
	"properties": {
	  "header_name": {
		"type": "string"
	  },
	  "set_in_response": {
		"type": "boolean"
	  }
	},
	"required": [
	  "header_name"
	]
  }
`

type Plugin struct {
	config Config
}

// FIXME: use jsonschema to unmarshal the config dynamic

type Config struct {
	HeaderName    string `json:"header_name"`
	SetInResponse bool   `json:"set_in_response"`
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
	err := plugin_config.Parse(pc, &c)
	fmt.Printf("%s config before parse %+v, err=%+v\n", name, pc, err)

	p.config = c
	fmt.Printf("%s parsed config %+v\n", name, c)

	p.config = c

	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		fmt.Println("call the Handler")

		requestID := r.Header.Get(p.config.HeaderName)
		if requestID == "" {
			requestID = uuid.Must(uuid.NewV4()).String()
		}

		fmt.Println("requestID: ", requestID)

		r.Header.Set(p.config.HeaderName, requestID)

		if p.config.SetInResponse {
			fmt.Println(
				"set the header in response",
			)
			w.Header().Set(p.config.HeaderName, requestID)
		}

		// ctx = plugin_ctx.WithPluginVar(ctx, name, "request_id", requestID)
		ctx = context.WithValue(ctx, "request_id", requestID)

		next.ServeHTTP(w, r.WithContext(ctx))
	}
	return http.HandlerFunc(fn)
}
