package request_validation

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	// version  = "0.1"
	priority = 2800
	name     = "request-validation"
)

const schema = `
{
	"type": "object",
	"properties": {
	  "header_schema": {
		"type": "object"
	  },
	  "body_schema": {
		"type": "object"
	  },
	  "rejected_code": {
		"type": "integer",
		"minimum": 200,
		"maximum": 599,
		"default": 400
	  },
	  "rejected_msg": {
		"type": "string",
		"minLength": 1,
		"maxLength": 256
	  }
	},
	"anyOf": [
	  {
		"required": ["header_schema"]
	  },
	  {
		"required": ["body_schema"]
	  }
	]
}`

type Config struct {
	// HeaderSchema *string `json:"header_schema,omitempty"`
	// BodySchema   *string `json:"body_schema,omitempty"`
	HeaderSchema map[string]interface{} `json:"header_schema,omitempty"`
	BodySchema   map[string]interface{} `json:"body_schema,omitempty"`
	RejectedCode int                    `json:"rejected_code"`
	RejectedMsg  string                 `json:"rejected_msg"`

	bodySchemaStr   string
	headerSchemaStr string
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.RejectedCode == 0 {
		p.config.RejectedCode = 400
	}

	if p.config.HeaderSchema != nil {
		headerSchemaStr, err := json.Marshal(p.config.HeaderSchema)
		if err != nil {
			return fmt.Errorf("failed to marshal header schema: %w", err)
		}
		p.config.headerSchemaStr = util.BytesToString(headerSchemaStr)
	}

	if p.config.BodySchema != nil {
		bodySchemaStr, err := json.Marshal(p.config.BodySchema)
		if err != nil {
			return fmt.Errorf("failed to marshal body schema: %w", err)
		}
		p.config.bodySchemaStr = util.BytesToString(bodySchemaStr)
	}

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if p.config.bodySchemaStr != "" {
			body, err := ctx.ReadRequestBody(r)
			if err != nil {
				err = fmt.Errorf("failed to read request body: %w", err)
				logger.Error(err.Error())
				http.Error(w, err.Error(), p.config.RejectedCode)
				return
			}

			// the body is []byte, want to unmarshal to map[string]interface{} or []interface{}, how to?
			bodyData, err := parseJSON(body)
			if err != nil {
				err = fmt.Errorf("failed to parse request body: %w", err)
				logger.Error(err.Error())
				http.Error(w, err.Error(), p.config.RejectedCode)
				return
			}

			err = util.Validate(bodyData, p.config.bodySchemaStr)
			if err != nil {
				msg := err.Error()
				if p.config.RejectedMsg != "" {
					msg = p.config.RejectedMsg
				}
				http.Error(w, msg, p.config.RejectedCode)
				return
			}

		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

// FIXME: if this func show in another plugin, should be refactor, only do it once
func parseJSON(data []byte) (interface{}, error) {
	trimmedData := strings.TrimSpace(string(data))
	// Check the first character of the JSON data to determine its type
	if len(trimmedData) > 0 {
		switch trimmedData[0] {
		case '{':
			var resultMap map[string]interface{}
			err := json.Unmarshal(data, &resultMap)
			if err != nil {
				return nil, err
			}
			return resultMap, nil

		case '[':
			var resultList []interface{}
			err := json.Unmarshal(data, &resultList)
			if err != nil {
				return nil, err
			}
			return resultList, nil

		default:
			return nil, fmt.Errorf("invalid JSON data")
		}
	}
	return nil, fmt.Errorf("empty JSON data")
}
