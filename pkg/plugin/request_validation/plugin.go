package request_validation

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
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
	config          Config
	headerSensitive bool
	bodySensitive   bool
	headerSecrets   []schemaSecret
	bodySecrets     []schemaSecret
}

const (
	// version  = "0.1"
	priority = 2800
	name     = "request-validation"
)

var errSensitiveSchemaMismatch = errors.New("request does not match schema")

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

	headerSchema *util.CompiledSchema
	bodySchema   *util.CompiledSchema
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

	if p.config.HeaderSchema != nil && p.config.headerSchema == nil && !p.headerSensitive {
		compiled, err := compileRequestValidationSchema("header_schema", p.config.HeaderSchema)
		if err != nil {
			return err
		}
		p.config.headerSchema = compiled
	}
	if p.config.BodySchema != nil && p.config.bodySchema == nil && !p.bodySensitive {
		compiled, err := compileRequestValidationSchema("body_schema", p.config.BodySchema)
		if err != nil {
			return err
		}
		p.config.bodySchema = compiled
	}
	return nil
}

func compileRequestValidationSchema(
	field string, document map[string]any,
) (*util.CompiledSchema, error) {
	if document == nil {
		return nil, nil
	}
	normalized := normalizeAPISIXSchema(document)
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal %s: %w", field, err)
	}
	defer clear(encoded)
	compiled, err := util.CompileSchemaWithoutExternalReferences(util.BytesToString(encoded))
	if err != nil {
		return nil, fmt.Errorf("invalid %s: %w", field, err)
	}
	return compiled, nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if p.config.HeaderSchema != nil {
			if err := p.validateSchema(
				"header_schema", p.config.HeaderSchema, p.headerSecrets,
				p.config.headerSchema, requestHeaders(r),
			); err != nil {
				message := schemaValidationDiagnostic(err, p.headerSensitive)
				logger.Error("req schema validation failed: " + message)
				writeSchemaRejection(
					w,
					p.schemaRejectedMessage(err, p.headerSensitive),
					p.config.RejectedCode,
				)
				return
			}
		}

		if p.config.BodySchema != nil {
			body, err := ctx.ReadRequestBody(r)
			if err != nil {
				err = fmt.Errorf("failed to read request body: %w", err)
				logger.Error(err.Error())
				http.Error(w, p.rejectedMessage(err), p.config.RejectedCode)
				return
			}
			if len(bytes.TrimSpace(body)) == 0 {
				err = fmt.Errorf("request body is required")
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

			err = p.validateSchema(
				"body_schema", p.config.BodySchema, p.bodySecrets,
				p.config.bodySchema, bodyData,
			)
			if err != nil {
				message := schemaValidationDiagnostic(err, p.bodySensitive)
				logger.Error("req schema validation failed: " + message)
				writeSchemaRejection(
					w,
					p.schemaRejectedMessage(err, p.bodySensitive),
					p.config.RejectedCode,
				)
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

func (p *Plugin) validateSchema(
	field string,
	document map[string]any,
	secrets []schemaSecret,
	compiled *util.CompiledSchema,
	instance any,
) error {
	if len(secrets) == 0 {
		return compiled.Validate(instance)
	}
	resolved := cloneSchemaDocument(document)
	defer clearSchemaValue(resolved)
	return withResolvedSchemaSecrets(resolved, secrets, 0, func() error {
		ephemeral, err := compileRequestValidationSchema(field, resolved)
		if err != nil {
			return errSensitiveSchemaMismatch
		}
		if err := ephemeral.Validate(instance); err != nil {
			return errSensitiveSchemaMismatch
		}
		return nil
	})
}

func (p *Plugin) rejectedMessage(err error) string {
	if p.config.RejectedMsg != "" {
		return p.config.RejectedMsg
	}
	return schemaValidationMessage(err)
}

func (p *Plugin) schemaRejectedMessage(err error, sensitive bool) string {
	if p.config.RejectedMsg != "" {
		return p.config.RejectedMsg
	}
	return schemaValidationDiagnostic(err, sensitive)
}

func schemaValidationDiagnostic(err error, sensitive bool) string {
	if sensitive {
		return "request does not match schema"
	}
	return schemaValidationMessage(err)
}

func schemaValidationMessage(err error) string {
	var validationErr *jsonschema.ValidationError
	if !errors.As(err, &validationErr) {
		return err.Error()
	}

	leaf := validationErr
	for len(leaf.Causes) > 0 {
		leaf = leaf.Causes[0]
	}
	if strings.HasSuffix(leaf.KeywordLocation, "/required") {
		const missingPropertiesPrefix = "missing properties: "
		if missing, ok := strings.CutPrefix(leaf.Message, missingPropertiesPrefix); ok {
			if firstProperty, ok := strings.CutPrefix(missing, "'"); ok {
				if property, _, ok := strings.Cut(firstProperty, "'"); ok {
					return fmt.Sprintf("property %q is required", property)
				}
			}
		}
	}
	if leaf.Message != "" {
		return leaf.Message
	}
	return "request does not match schema"
}

func writeSchemaRejection(w http.ResponseWriter, message string, status int) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, message)
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
	if len(trimmedData) == 0 {
		return nil, fmt.Errorf("empty JSON data")
	}

	var result any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	return result, nil
}

func normalizeAPISIXSchema(schema map[string]any) map[string]any {
	normalized := maps.Clone(schema)
	if schemaType, ok := normalized["type"].(string); ok {
		switch schemaType {
		case "table":
			normalized["type"] = []any{"object", "array"}
		case "function":
			delete(normalized, "type")
			normalized["not"] = map[string]any{}
		}
	}

	for _, keyword := range []string{
		"additionalItems", "additionalProperties", "contains", "contentSchema",
		"else", "if", "items", "not", "propertyNames", "then",
		"unevaluatedItems", "unevaluatedProperties",
	} {
		if value, ok := normalized[keyword]; ok {
			normalized[keyword] = normalizeAPISIXSubschema(value)
		}
	}
	for _, keyword := range []string{"allOf", "anyOf", "oneOf", "prefixItems"} {
		if values, ok := normalized[keyword].([]any); ok {
			items := make([]any, len(values))
			for index, value := range values {
				items[index] = normalizeAPISIXSubschema(value)
			}
			normalized[keyword] = items
		}
	}
	for _, keyword := range []string{"$defs", "definitions", "dependencies", "dependentSchemas", "patternProperties", "properties"} {
		if values, ok := normalized[keyword].(map[string]any); ok {
			items := make(map[string]any, len(values))
			for name, value := range values {
				items[name] = normalizeAPISIXSubschema(value)
			}
			normalized[keyword] = items
		}
	}
	return normalized
}

func normalizeAPISIXSubschema(value any) any {
	if schema, ok := value.(map[string]any); ok {
		return normalizeAPISIXSchema(schema)
	}
	if schemas, ok := value.([]any); ok {
		normalized := make([]any, len(schemas))
		for index, schema := range schemas {
			normalized[index] = normalizeAPISIXSubschema(schema)
		}
		return normalized
	}
	return value
}
