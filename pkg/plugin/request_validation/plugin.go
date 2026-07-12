package request_validation

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"

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
	HeaderSchema map[string]any `json:"header_schema,omitempty"`
	BodySchema   map[string]any `json:"body_schema,omitempty"`
	RejectedCode int            `json:"rejected_code"`
	RejectedMsg  string         `json:"rejected_msg"`

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
		if err := compileNestedSchema("header_schema", headerSchemaStr); err != nil {
			return err
		}
		p.config.headerSchemaStr = util.BytesToString(headerSchemaStr)
	}

	if p.config.BodySchema != nil {
		bodySchemaStr, err := json.Marshal(p.config.BodySchema)
		if err != nil {
			return fmt.Errorf("failed to marshal body schema: %w", err)
		}
		if err := compileNestedSchema("body_schema", bodySchemaStr); err != nil {
			return err
		}
		p.config.bodySchemaStr = util.BytesToString(bodySchemaStr)
	}

	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if p.config.headerSchemaStr != "" {
			if err := util.Validate(requestHeaders(r), p.config.headerSchemaStr); err != nil {
				logger.Error("req schema validation failed: " + err.Error())
				http.Error(w, p.rejectedMessage(err), p.config.RejectedCode)
				return
			}
		}

		if p.config.bodySchemaStr != "" {
			body, err := ctx.ReadRequestBody(r)
			if err != nil {
				err = fmt.Errorf("failed to read request body: %w", err)
				logger.Error(err.Error())
				http.Error(w, p.rejectedMessage(err), p.config.RejectedCode)
				return
			}

			bodyData, bodyIsJSON, err := parseRequestBody(r, body)
			if err != nil {
				err = fmt.Errorf("failed to parse request body: %w", err)
				logger.Error(err.Error())
				http.Error(w, p.rejectedMessage(err), p.config.RejectedCode)
				return
			}

			err = util.Validate(bodyData, p.config.bodySchemaStr)
			if err != nil {
				http.Error(w, p.rejectedMessage(err), p.config.RejectedCode)
				return
			}
			if bodyIsJSON {
				if err := normalizeJSONBody(r, bodyData); err != nil {
					err = fmt.Errorf("failed to normalize request body: %w", err)
					logger.Error(err.Error())
					http.Error(w, p.rejectedMessage(err), p.config.RejectedCode)
					return
				}
			}

		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) rejectedMessage(err error) string {
	if p.config.RejectedMsg != "" {
		return p.config.RejectedMsg
	}
	return err.Error()
}

func requestHeaders(r *http.Request) map[string]any {
	headers := make(map[string]any, len(r.Header)*2+2)
	for key := range r.Header {
		values := r.Header.Values(key)
		if len(values) == 0 {
			continue
		}
		var value any = values[0]
		if len(values) > 1 {
			items := make([]any, len(values))
			for i, item := range values {
				items[i] = item
			}
			value = items
		}
		headers[key] = value
		headers[strings.ToLower(key)] = value
	}
	if r.Host != "" {
		headers["Host"] = r.Host
		headers["host"] = r.Host
	}

	return headers
}

func compileNestedSchema(name string, schema []byte) error {
	if _, err := jsonschema.CompileString(name+".json", util.BytesToString(schema)); err != nil {
		return fmt.Errorf("invalid %s: %w", name, err)
	}
	return nil
}

func parseRequestBody(r *http.Request, body []byte) (any, bool, error) {
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.HasPrefix(contentType, "application/x-www-form-urlencoded") {
		data, err := parseURLEncodedForm(body)
		return data, false, err
	}

	data, err := parseJSON(body)
	return data, true, err
}

func normalizeJSONBody(r *http.Request, data any) error {
	body, err := json.Marshal(data)
	if err != nil {
		return err
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	r.ContentLength = int64(len(body))
	if ctx.GetRequestVars(r) != nil {
		ctx.RegisterRequestVar(r, ctx.RequestBodyKey, body)
	}
	return nil
}

func parseURLEncodedForm(data []byte) (map[string]any, error) {
	values, err := url.ParseQuery(util.BytesToString(data))
	if err != nil {
		return nil, err
	}

	result := make(map[string]any, len(values))
	for key, vals := range values {
		if len(vals) == 1 {
			result[key] = vals[0]
			continue
		}

		items := make([]any, len(vals))
		for i, val := range vals {
			items[i] = val
		}
		result[key] = items
	}

	return result, nil
}

// FIXME: if this func show in another plugin, should be refactor, only do it once
func parseJSON(data []byte) (any, error) {
	trimmedData := strings.TrimSpace(string(data))
	// Check the first character of the JSON data to determine its type
	if len(trimmedData) > 0 {
		switch trimmedData[0] {
		case '{':
			var resultMap map[string]any
			err := json.Unmarshal(data, &resultMap)
			if err != nil {
				return nil, err
			}
			return resultMap, nil

		case '[':
			var resultList []any
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
