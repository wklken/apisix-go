package request_id

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gofrs/uuid"
	"github.com/wklken/apisix-go/pkg/plugin/base"
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
	base.BasePlugin
	config Config
}

type Config struct {
	HeaderName    string `json:"header_name"`
	SetInResponse bool   `json:"set_in_response"`
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

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
