package oas_validator

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 512
	name     = "oas-validator"
)

const schema = `
{
  "type": "object",
  "properties": {
    "spec": {
      "type": "string",
      "minLength": 1
    },
    "spec_url": {
      "type": "string",
      "pattern": "^https?://"
    },
    "spec_url_request_headers": {
      "type": "object",
      "additionalProperties": {
        "type": "string"
      }
    },
    "ssl_verify": {
      "type": "boolean",
      "default": false
    },
    "timeout": {
      "type": "integer",
      "minimum": 1000,
      "maximum": 60000,
      "default": 10000
    },
    "verbose_errors": {
      "type": "boolean",
      "default": false
    },
    "skip_request_body_validation": {
      "type": "boolean",
      "default": false
    },
    "skip_request_header_validation": {
      "type": "boolean",
      "default": false
    },
    "skip_query_param_validation": {
      "type": "boolean",
      "default": false
    },
    "skip_path_params_validation": {
      "type": "boolean",
      "default": false
    },
    "reject_if_not_match": {
      "type": "boolean",
      "default": true
    },
    "rejection_status_code": {
      "type": "integer",
      "minimum": 400,
      "maximum": 599,
      "default": 400
    }
  },
  "oneOf": [
    {
      "required": ["spec"]
    },
    {
      "required": ["spec_url"]
    }
  ]
}
`

type Config struct {
	Spec                        string            `json:"spec,omitempty"`
	SpecURL                     string            `json:"spec_url,omitempty"`
	SpecURLRequestHeaders       map[string]string `json:"spec_url_request_headers,omitempty"`
	SSLVerify                   bool              `json:"ssl_verify,omitempty"`
	Timeout                     int               `json:"timeout,omitempty"`
	VerboseErrors               bool              `json:"verbose_errors,omitempty"`
	SkipRequestBodyValidation   bool              `json:"skip_request_body_validation,omitempty"`
	SkipRequestHeaderValidation bool              `json:"skip_request_header_validation,omitempty"`
	SkipQueryParamValidation    bool              `json:"skip_query_param_validation,omitempty"`
	SkipPathParamsValidation    bool              `json:"skip_path_params_validation,omitempty"`
	RejectIfNotMatch            *bool             `json:"reject_if_not_match,omitempty"`
	RejectionStatusCode         int               `json:"rejection_status_code,omitempty"`

	compiled *compiledSpec
}

type openAPISpec struct {
	Paths map[string]pathItem `json:"paths"`
}

type pathItem struct {
	Parameters []parameter `json:"parameters,omitempty"`
	Get        *operation  `json:"get,omitempty"`
	Post       *operation  `json:"post,omitempty"`
	Put        *operation  `json:"put,omitempty"`
	Delete     *operation  `json:"delete,omitempty"`
	Patch      *operation  `json:"patch,omitempty"`
	Head       *operation  `json:"head,omitempty"`
	Options    *operation  `json:"options,omitempty"`
}

type operation struct {
	Parameters  []parameter  `json:"parameters,omitempty"`
	RequestBody *requestBody `json:"requestBody,omitempty"`
}

type parameter struct {
	Name     string         `json:"name"`
	In       string         `json:"in"`
	Required bool           `json:"required,omitempty"`
	Schema   map[string]any `json:"schema,omitempty"`
}

type requestBody struct {
	Required bool                 `json:"required,omitempty"`
	Content  map[string]mediaType `json:"content,omitempty"`
}

type mediaType struct {
	Schema map[string]any `json:"schema,omitempty"`
}

type compiledSpec struct {
	operations []compiledOperation
}

type compiledOperation struct {
	method     string
	template   string
	segments   []string
	parameters []parameter
	body       *requestBody
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

func (p *Plugin) PostInit() error {
	if p.config.Timeout == 0 {
		p.config.Timeout = 10000
	}
	if p.config.RejectionStatusCode == 0 {
		p.config.RejectionStatusCode = http.StatusBadRequest
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		validator, err := p.validator()
		if err != nil {
			writeJSONMessage(w, http.StatusInternalServerError, "failed to parse openapi spec")
			return
		}

		if err := p.validateRequest(validator, r); err != nil {
			if p.rejectIfNotMatch() {
				msg := "failed to validate request. "
				if p.config.VerboseErrors {
					msg += err.Error()
				}
				writeJSONMessage(w, p.config.RejectionStatusCode, msg)
				return
			}
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) validator() (*compiledSpec, error) {
	if p.config.compiled != nil {
		return p.config.compiled, nil
	}

	spec := p.config.Spec
	if spec == "" {
		fetched, err := p.fetchSpec()
		if err != nil {
			return nil, err
		}
		spec = fetched
	}

	compiled, err := compileSpec(spec)
	if err != nil {
		return nil, err
	}
	p.config.compiled = compiled
	return compiled, nil
}

func (p *Plugin) fetchSpec() (string, error) {
	req, err := http.NewRequest(http.MethodGet, p.config.SpecURL, nil)
	if err != nil {
		return "", err
	}
	for name, value := range p.config.SpecURLRequestHeaders {
		req.Header.Set(name, value)
	}

	client := &http.Client{Timeout: time.Duration(p.config.Timeout) * time.Millisecond}
	if !p.config.SSLVerify {
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("spec URL returned status %d", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (p *Plugin) rejectIfNotMatch() bool {
	return p.config.RejectIfNotMatch == nil || *p.config.RejectIfNotMatch
}

func (p *Plugin) validateRequest(spec *compiledSpec, r *http.Request) error {
	op, pathParams := spec.match(r.Method, r.URL.Path)
	if op == nil {
		return fmt.Errorf("no matching operation for %s %s", r.Method, r.URL.Path)
	}

	if !p.config.SkipPathParamsValidation {
		if err := validateParams(op.parameters, "path", pathParams, nil); err != nil {
			return err
		}
	}
	if !p.config.SkipQueryParamValidation {
		if err := validateParams(op.parameters, "query", nil, r); err != nil {
			return err
		}
	}
	if !p.config.SkipRequestHeaderValidation {
		if err := validateParams(op.parameters, "header", nil, r); err != nil {
			return err
		}
	}
	if !p.config.SkipRequestBodyValidation {
		if err := validateBody(op.body, r); err != nil {
			return err
		}
	}

	return nil
}

func compileSpec(spec string) (*compiledSpec, error) {
	var parsed openAPISpec
	if err := json.Unmarshal([]byte(spec), &parsed); err != nil {
		return nil, err
	}

	compiled := &compiledSpec{}
	for path, item := range parsed.Paths {
		compiled.addOperation(path, http.MethodGet, item.Get, item.Parameters)
		compiled.addOperation(path, http.MethodPost, item.Post, item.Parameters)
		compiled.addOperation(path, http.MethodPut, item.Put, item.Parameters)
		compiled.addOperation(path, http.MethodDelete, item.Delete, item.Parameters)
		compiled.addOperation(path, http.MethodPatch, item.Patch, item.Parameters)
		compiled.addOperation(path, http.MethodHead, item.Head, item.Parameters)
		compiled.addOperation(path, http.MethodOptions, item.Options, item.Parameters)
	}
	return compiled, nil
}

func (s *compiledSpec) addOperation(path string, method string, op *operation, pathParams []parameter) {
	if op == nil {
		return
	}
	params := make([]parameter, 0, len(pathParams)+len(op.Parameters))
	params = append(params, pathParams...)
	params = append(params, op.Parameters...)
	s.operations = append(s.operations, compiledOperation{
		method:     method,
		template:   path,
		segments:   splitPath(path),
		parameters: params,
		body:       op.RequestBody,
	})
}

func (s *compiledSpec) match(method string, path string) (*compiledOperation, map[string]string) {
	segments := splitPath(path)
	for i := range s.operations {
		op := &s.operations[i]
		if op.method != method || len(op.segments) != len(segments) {
			continue
		}

		params := map[string]string{}
		matched := true
		for idx, segment := range op.segments {
			if isPathParam(segment) {
				params[strings.TrimSuffix(strings.TrimPrefix(segment, "{"), "}")] = segments[idx]
				continue
			}
			if segment != segments[idx] {
				matched = false
				break
			}
		}
		if matched {
			return op, params
		}
	}
	return nil, nil
}

func splitPath(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	return strings.Split(path, "/")
}

func isPathParam(segment string) bool {
	return strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") && len(segment) > 2
}

func validateParams(params []parameter, location string, pathParams map[string]string, r *http.Request) error {
	for _, param := range params {
		if param.In != location {
			continue
		}
		value, ok := paramValue(param, location, pathParams, r)
		if (!ok || value == "") && param.Required {
			return fmt.Errorf("missing required %s parameter %q", location, param.Name)
		}
		if !ok || value == "" || param.Schema == nil {
			continue
		}
		if err := validateValue(value, param.Schema); err != nil {
			return fmt.Errorf("invalid %s parameter %q: %w", location, param.Name, err)
		}
	}
	return nil
}

func paramValue(param parameter, location string, pathParams map[string]string, r *http.Request) (string, bool) {
	switch location {
	case "path":
		value, ok := pathParams[param.Name]
		return value, ok
	case "query":
		values, ok := r.URL.Query()[param.Name]
		if !ok || len(values) == 0 {
			return "", false
		}
		return values[0], true
	case "header":
		value := r.Header.Get(param.Name)
		return value, value != ""
	default:
		return "", false
	}
}

func validateBody(body *requestBody, r *http.Request) error {
	if body == nil {
		return nil
	}

	rawBody, err := readBody(r)
	if err != nil {
		return fmt.Errorf("error reading the request body. err: %w", err)
	}
	if len(bytes.TrimSpace(rawBody)) == 0 {
		if body.Required {
			return fmt.Errorf("missing required request body")
		}
		return nil
	}

	contentType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	media, ok := body.Content[contentType]
	if !ok && contentType == "application/json" {
		media, ok = body.Content["application/json"]
	}
	if !ok || media.Schema == nil {
		if len(body.Content) > 0 {
			return fmt.Errorf("unsupported request body content type %q", contentType)
		}
		return nil
	}

	var data any
	if err := json.Unmarshal(rawBody, &data); err != nil {
		return fmt.Errorf("invalid request body JSON: %w", err)
	}
	if err := validateAny(data, media.Schema); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	if closeErr := r.Body.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, err
}

func validateValue(value string, schema map[string]any) error {
	return validateAny(coerceValue(value, schema), schema)
}

func validateAny(value any, schema map[string]any) error {
	encoded, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	compiled, err := jsonschema.CompileString("schema.json", string(encoded))
	if err != nil {
		return err
	}
	return compiled.Validate(value)
}

func coerceValue(value string, schema map[string]any) any {
	schemaType, _ := schema["type"].(string)
	switch schemaType {
	case "integer":
		if out, err := strconv.ParseInt(value, 10, 64); err == nil {
			return out
		}
	case "number":
		if out, err := strconv.ParseFloat(value, 64); err == nil {
			return out
		}
	case "boolean":
		if out, err := strconv.ParseBool(value); err == nil {
			return out
		}
	}
	return value
}

func writeJSONMessage(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"message":%q}`, message)
}
