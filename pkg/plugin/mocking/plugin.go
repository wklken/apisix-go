package mocking

import (
	"fmt"
	"net/http"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

// TODO:
// 1. not support response_schema yet
// 2. no unittest

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	// version  = "0.1"
	priority = 10900
	name     = "mocking"
)

const schema = `
{
	"$schema": "http://json-schema.org/draft-04/schema#",
	"type": "object",
	"properties": {
	  "delay": {
		"type": "integer",
		"default": 0
	  },
	  "response_status": {
		"type": "integer",
		"default": 200,
		"minimum": 100
	  },
	  "content_type": {
		"type": "string",
		"default": "application/json;charset=utf8"
	  },
	  "response_example": {
		"type": "string"
	  },
	  "response_schema": {
		"type": "object"
	  },
	  "with_mock_header": {
		"type": "boolean",
		"default": true
	  },
	  "response_headers": {
		"type": "object",
		"minProperties": 1,
		"patternProperties": {
		  "^[^:]+$": {
			"oneOf": [
			  {
				"type": "string"
			  },
			  {
				"type": "number"
			  }
			]
		  }
		}
	  }
	},
	"anyOf": [
	  {
		"required": ["response_example"]
	  },
	  {
		"required": ["response_schema"]
	  }
	]
}`

type Config struct {
	Delay           int               `json:"delay"`
	ResponseStatus  int               `json:"response_status"`
	ContentType     string            `json:"content_type"`
	ResponseExample *string           `json:"response_example,omitempty"`
	ResponseSchema  *map[string]any   `json:"response_schema,omitempty"`
	WithMockHeader  *bool             `json:"with_mock_header"`
	ResponseHeaders map[string]string `json:"response_headers"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.ResponseStatus == 0 {
		p.config.ResponseStatus = 200
	}
	if p.config.ContentType != "" {
		p.config.ContentType = "application/json"
	}

	if p.config.WithMockHeader == nil {
		defaultValue := true
		p.config.WithMockHeader = &defaultValue
	}

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("mocking config: %+v\n", p.config)
		// Delay response if needed
		if p.config.Delay > 0 {
			time.Sleep(time.Second * time.Duration(p.config.Delay))
		}

		// Set content type
		w.Header().Set("Content-Type", p.config.ContentType)

		// Set response headers
		for key, value := range p.config.ResponseHeaders {
			w.Header().Add(key, value)
		}

		w.WriteHeader(p.config.ResponseStatus)

		// NOTE: not support response_schema yet
		if p.config.ResponseExample != nil {
			w.Write([]byte(*p.config.ResponseExample))
		} else {
			w.Write([]byte{})
		}

		// mock header
		if *p.config.WithMockHeader {
			// FIXME: change 0.0.1 to real version
			w.Header().Add("x-mock-by", "APISIX-GO/0.0.1")
		}

		// return without calling next.ServeHTTP
		return
		// next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}
