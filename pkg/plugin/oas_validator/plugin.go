package oas_validator

import (
	"bytes"
	"crypto/tls"
	"encoding/xml"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
	"go.yaml.in/yaml/v3"
)

type Plugin struct {
	base.BasePlugin
	config     Config
	metadata   Metadata
	mu         sync.Mutex
	compiledAt time.Time
	now        func() time.Time
}

const (
	priority          = 512
	name              = "oas-validator"
	defaultSpecURLTTL = 3600
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

type Metadata struct {
	SpecURLTTL int `json:"spec_url_ttl,omitempty"`
}

type openAPISpec struct {
	Paths      map[string]pathItem `json:"paths"`
	Components components          `json:"components"`
}

type components struct {
	Schemas map[string]map[string]any `json:"schemas,omitempty"`
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
	Name     string               `json:"name"`
	In       string               `json:"in"`
	Required bool                 `json:"required,omitempty"`
	Style    string               `json:"style,omitempty"`
	Explode  *bool                `json:"explode,omitempty"`
	Schema   map[string]any       `json:"schema,omitempty"`
	Content  map[string]mediaType `json:"content,omitempty"`
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

const maxExternalRefs = 32

type specResolver struct {
	client    *http.Client
	headers   map[string]string
	documents map[string]any
	refCount  int
}

type compiledOperation struct {
	method     string
	template   string
	segments   []string
	parameters []parameter
	body       *requestBody
}

func (p *Plugin) Config() any {
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
	p.metadata = loadMetadata()
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
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.config.compiled != nil {
		if p.config.Spec != "" || p.currentTime().Before(p.compiledAt.Add(p.specURLTTL())) {
			return p.config.compiled, nil
		}
	}

	spec := p.config.Spec
	if spec == "" {
		fetched, err := p.fetchSpec()
		if err != nil {
			return nil, err
		}
		spec = fetched
	}

	var (
		baseURL *url.URL
		err     error
	)
	if p.config.SpecURL != "" {
		baseURL, err = url.Parse(p.config.SpecURL)
		if err != nil {
			return nil, err
		}
	}
	resolver := &specResolver{
		client:    p.httpClient(),
		headers:   p.config.SpecURLRequestHeaders,
		documents: make(map[string]any),
	}
	compiled, err := compileSpecWithResolver(spec, resolver, baseURL)
	if err != nil {
		return nil, err
	}
	p.config.compiled = compiled
	p.compiledAt = p.currentTime()
	return compiled, nil
}

func (p *Plugin) currentTime() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *Plugin) specURLTTL() time.Duration {
	if p.metadata.SpecURLTTL > 0 {
		return time.Duration(p.metadata.SpecURLTTL) * time.Second
	}
	return defaultSpecURLTTL * time.Second
}

func loadMetadata() (metadata Metadata) {
	defer func() {
		if recover() != nil {
			metadata = Metadata{}
		}
	}()
	if err := store.GetPluginMetadata(name, &metadata); err != nil {
		return Metadata{}
	}
	return metadata
}

func (p *Plugin) fetchSpec() (string, error) {
	req, err := http.NewRequest(http.MethodGet, p.config.SpecURL, nil)
	if err != nil {
		return "", err
	}
	for name, value := range p.config.SpecURLRequestHeaders {
		req.Header.Set(name, value)
	}

	res, err := p.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("spec URL returned status %d", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (p *Plugin) httpClient() *http.Client {
	client := &http.Client{Timeout: time.Duration(p.config.Timeout) * time.Millisecond}
	if !p.config.SSLVerify {
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	return client
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
		if err := validateParams(op.parameters, "cookie", nil, r); err != nil {
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

func compileSpecWithResolver(spec string, resolver *specResolver, baseURL *url.URL) (*compiledSpec, error) {
	var raw any
	if err := json.Unmarshal([]byte(spec), &raw); err != nil {
		return nil, err
	}
	if resolver != nil {
		resolved, err := resolver.resolve(raw, raw, baseURL, true, map[string]bool{}, 0)
		if err != nil {
			return nil, err
		}
		raw = resolved
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}

	var parsed openAPISpec
	if err := json.Unmarshal(encoded, &parsed); err != nil {
		return nil, err
	}

	compiled := &compiledSpec{}
	for path, item := range parsed.Paths {
		compiled.addOperation(path, http.MethodGet, item.Get, item.Parameters, parsed.Components.Schemas)
		compiled.addOperation(path, http.MethodPost, item.Post, item.Parameters, parsed.Components.Schemas)
		compiled.addOperation(path, http.MethodPut, item.Put, item.Parameters, parsed.Components.Schemas)
		compiled.addOperation(path, http.MethodDelete, item.Delete, item.Parameters, parsed.Components.Schemas)
		compiled.addOperation(path, http.MethodPatch, item.Patch, item.Parameters, parsed.Components.Schemas)
		compiled.addOperation(path, http.MethodHead, item.Head, item.Parameters, parsed.Components.Schemas)
		compiled.addOperation(path, http.MethodOptions, item.Options, item.Parameters, parsed.Components.Schemas)
	}
	return compiled, nil
}

func (r *specResolver) resolve(
	value any,
	root any,
	baseURL *url.URL,
	resolveLocal bool,
	stack map[string]bool,
	depth int,
) (any, error) {
	if depth > maxExternalRefs {
		return nil, fmt.Errorf("external $ref nesting exceeds %d", maxExternalRefs)
	}

	switch typed := value.(type) {
	case map[string]any:
		if ref, ok := typed["$ref"].(string); ok {
			if strings.HasPrefix(ref, "#") {
				if !resolveLocal {
					return value, nil
				}
				target, err := jsonPointer(root, ref)
				if err != nil {
					return nil, err
				}
				key := documentKey(baseURL) + ref
				if stack[key] {
					return nil, fmt.Errorf("cyclic external $ref %q", ref)
				}
				stack[key] = true
				resolved, err := r.resolve(target, root, baseURL, true, stack, depth+1)
				delete(stack, key)
				return resolved, err
			}

			refURL, err := resolveRefURL(baseURL, ref)
			if err != nil {
				return nil, err
			}
			r.refCount++
			if r.refCount > maxExternalRefs {
				return nil, fmt.Errorf("external $ref count exceeds %d", maxExternalRefs)
			}
			key := refURL.String()
			if stack[key] {
				return nil, fmt.Errorf("cyclic external $ref %q", ref)
			}
			document, documentURL, err := r.fetchDocument(refURL)
			if err != nil {
				return nil, err
			}
			fragment := "#"
			if refURL.Fragment != "" {
				fragment = "#" + refURL.Fragment
			}
			target, err := jsonPointer(document, fragment)
			if err != nil {
				return nil, fmt.Errorf("resolve external $ref %q: %w", ref, err)
			}
			stack[key] = true
			resolved, err := r.resolve(target, document, documentURL, true, stack, depth+1)
			delete(stack, key)
			return resolved, err
		}

		resolved := make(map[string]any, len(typed))
		for key, item := range typed {
			item, err := r.resolve(item, root, baseURL, resolveLocal, stack, depth)
			if err != nil {
				return nil, err
			}
			resolved[key] = item
		}
		return resolved, nil
	case []any:
		resolved := make([]any, len(typed))
		for index, item := range typed {
			item, err := r.resolve(item, root, baseURL, resolveLocal, stack, depth)
			if err != nil {
				return nil, err
			}
			resolved[index] = item
		}
		return resolved, nil
	default:
		return value, nil
	}
}

func (r *specResolver) fetchDocument(refURL *url.URL) (any, *url.URL, error) {
	documentURL := *refURL
	documentURL.Fragment = ""
	key := documentURL.String()
	if document, ok := r.documents[key]; ok {
		return document, &documentURL, nil
	}

	req, err := http.NewRequest(http.MethodGet, key, nil)
	if err != nil {
		return nil, nil, err
	}
	for name, value := range r.headers {
		req.Header.Set(name, value)
	}
	res, err := r.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("external $ref URL returned status %d", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, nil, err
	}
	var document any
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, nil, fmt.Errorf("external $ref URL returned invalid JSON: %w", err)
	}
	r.documents[key] = document
	return document, &documentURL, nil
}

func resolveRefURL(baseURL *url.URL, ref string) (*url.URL, error) {
	refURL, err := url.Parse(ref)
	if err != nil {
		return nil, fmt.Errorf("invalid external $ref %q: %w", ref, err)
	}
	if refURL.Scheme == "" {
		if baseURL == nil {
			return nil, fmt.Errorf("relative external $ref %q requires a spec URL", ref)
		}
		refURL = baseURL.ResolveReference(refURL)
	}
	if refURL.Scheme != "http" && refURL.Scheme != "https" {
		return nil, fmt.Errorf("external $ref %q must use http or https", ref)
	}
	return refURL, nil
}

func jsonPointer(document any, fragment string) (any, error) {
	if fragment == "" || fragment == "#" {
		return document, nil
	}
	if !strings.HasPrefix(fragment, "#/") {
		return nil, fmt.Errorf("unsupported JSON pointer %q", fragment)
	}
	current := document
	for part := range strings.SplitSeq(strings.TrimPrefix(fragment, "#/"), "/") {
		part, err := url.PathUnescape(part)
		if err != nil {
			return nil, fmt.Errorf("invalid JSON pointer %q: %w", fragment, err)
		}
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		switch typed := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = typed[part]
			if !ok {
				return nil, fmt.Errorf("JSON pointer %q does not exist", fragment)
			}
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(typed) {
				return nil, fmt.Errorf("JSON pointer %q has invalid array index", fragment)
			}
			current = typed[index]
		default:
			return nil, fmt.Errorf("JSON pointer %q traverses a non-container", fragment)
		}
	}
	return current, nil
}

func documentKey(documentURL *url.URL) string {
	if documentURL == nil {
		return "inline"
	}
	copyURL := *documentURL
	copyURL.Fragment = ""
	return copyURL.String()
}

func (s *compiledSpec) addOperation(
	path string,
	method string,
	op *operation,
	pathParams []parameter,
	schemas map[string]map[string]any,
) {
	if op == nil {
		return
	}
	params := make([]parameter, 0, len(pathParams)+len(op.Parameters))
	params = append(params, pathParams...)
	params = append(params, op.Parameters...)
	for i := range params {
		params[i].Schema = resolveSchemaRefs(params[i].Schema, schemas)
		for contentType, media := range params[i].Content {
			media.Schema = resolveSchemaRefs(media.Schema, schemas)
			params[i].Content[contentType] = media
		}
	}
	s.operations = append(s.operations, compiledOperation{
		method:     method,
		template:   path,
		segments:   splitPath(path),
		parameters: params,
		body:       resolveRequestBodyRefs(op.RequestBody, schemas),
	})
}

func resolveRequestBodyRefs(body *requestBody, schemas map[string]map[string]any) *requestBody {
	if body == nil {
		return nil
	}

	out := &requestBody{
		Required: body.Required,
		Content:  make(map[string]mediaType, len(body.Content)),
	}
	for contentType, media := range body.Content {
		out.Content[contentType] = mediaType{Schema: resolveSchemaRefs(media.Schema, schemas)}
	}
	return out
}

func resolveSchemaRefs(schema map[string]any, schemas map[string]map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	resolved, ok := resolveSchemaValue(schema, schemas, map[string]bool{}).(map[string]any)
	if !ok {
		return schema
	}
	return resolved
}

func resolveSchemaValue(value any, schemas map[string]map[string]any, seen map[string]bool) any {
	switch v := value.(type) {
	case map[string]any:
		if ref, ok := v["$ref"].(string); ok {
			name, ok := strings.CutPrefix(ref, "#/components/schemas/")
			if ok && !seen[name] {
				if target, exists := schemas[name]; exists {
					seen[name] = true
					resolved := resolveSchemaValue(target, schemas, seen)
					delete(seen, name)
					return resolved
				}
			}
		}

		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = resolveSchemaValue(item, schemas, seen)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = resolveSchemaValue(item, schemas, seen)
		}
		return out
	default:
		return value
	}
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
		value, ok, err := paramValue(param, location, pathParams, r)
		if err != nil {
			return fmt.Errorf("invalid %s parameter %q: %w", location, param.Name, err)
		}
		if !ok || emptyParamValue(value) {
			if param.Required {
				return fmt.Errorf("missing required %s parameter %q", location, param.Name)
			}
			continue
		}
		schema := parameterSchema(param)
		if !ok || emptyParamValue(value) || schema == nil {
			continue
		}
		if err := validateValue(value, schema); err != nil {
			return fmt.Errorf("invalid %s parameter %q: %w", location, param.Name, err)
		}
	}
	return nil
}

func emptyParamValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return typed == ""
	case []any:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	default:
		return false
	}
}

func paramValue(param parameter, location string, pathParams map[string]string, r *http.Request) (any, bool, error) {
	if len(param.Content) > 0 {
		return contentParameterValue(param, location, pathParams, r)
	}
	if err := validateParameterStyle(param, location); err != nil {
		return nil, false, err
	}

	switch location {
	case "path":
		value, ok := pathParams[param.Name]
		if !ok {
			return "", false, nil
		}
		parsed, err := parsePathParameter(value, param)
		return parsed, true, err
	case "query":
		return queryParamValue(param, r.URL.Query())
	case "header":
		values := r.Header.Values(param.Name)
		if len(values) == 0 {
			return "", false, nil
		}
		if len(values) > 1 {
			items := make([]any, len(values))
			for index, value := range values {
				items[index] = value
			}
			return coerceParameterValue(items, param.Schema), true, nil
		}
		parsed, err := parseSimpleParameter(values[0], param)
		return parsed, true, err
	case "cookie":
		return cookieParamValue(param, r)
	default:
		return "", false, nil
	}
}

func validateParameterStyle(param parameter, location string) error {
	if len(param.Content) > 0 {
		if param.Schema != nil {
			return fmt.Errorf("parameter %q cannot define both content and schema", param.Name)
		}
		if len(param.Content) != 1 {
			return fmt.Errorf("parameter %q content must define exactly one media type", param.Name)
		}
		for mediaType, media := range param.Content {
			if media.Schema == nil {
				return fmt.Errorf("parameter %q content media type %q has no schema", param.Name, mediaType)
			}
		}
		return nil
	}
	style, _ := parameterStyle(param, location)
	allowed := false
	switch location {
	case "path":
		allowed = style == "matrix" || style == "label" || style == "simple"
	case "query":
		allowed = style == "form" || style == "spaceDelimited" || style == "pipeDelimited" || style == "deepObject"
	case "header":
		allowed = style == "simple"
	case "cookie":
		allowed = style == "form"
	default:
		return fmt.Errorf("unsupported parameter location %q", location)
	}
	if !allowed {
		return fmt.Errorf("unsupported %s parameter style %q", location, style)
	}

	schemaType, _ := param.Schema["type"].(string)
	switch style {
	case "deepObject":
		if schemaType != "object" {
			return fmt.Errorf("unsupported %s parameter style %q for schema type %q", location, style, schemaType)
		}
		if param.Explode != nil && !*param.Explode {
			return fmt.Errorf("deepObject parameter style requires explode=true")
		}
	case "spaceDelimited", "pipeDelimited":
		if schemaType != "array" && schemaType != "object" {
			return fmt.Errorf("unsupported %s parameter style %q for schema type %q", location, style, schemaType)
		}
	}
	return nil
}

func contentParameterValue(
	param parameter,
	location string,
	pathParams map[string]string,
	r *http.Request,
) (any, bool, error) {
	if err := validateParameterStyle(param, location); err != nil {
		return nil, false, err
	}
	mediaType, media, ok := singleParameterContent(param.Content)
	if !ok {
		return nil, false, fmt.Errorf("parameter %q content must define exactly one media type", param.Name)
	}
	raw, present, err := rawContentParameterValue(param.Name, location, pathParams, r)
	if err != nil || !present {
		return nil, present, err
	}

	normalizedMediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(mediaType, ";", 2)[0]))
	schema := media.Schema
	switch {
	case normalizedMediaType == "application/json" || strings.HasSuffix(normalizedMediaType, "+json"):
		var value any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			return nil, true, fmt.Errorf("content media type %q contains invalid JSON: %w", mediaType, err)
		}
		return value, true, nil
	case normalizedMediaType == "text/plain":
		return coerceParameterValue(raw, schema), true, nil
	default:
		return nil, true, fmt.Errorf("unsupported parameter content media type %q", mediaType)
	}
}

func singleParameterContent(content map[string]mediaType) (string, mediaType, bool) {
	if len(content) != 1 {
		return "", mediaType{}, false
	}
	for mediaType, media := range content {
		return mediaType, media, true
	}
	return "", mediaType{}, false
}

func rawContentParameterValue(
	name string,
	location string,
	pathParams map[string]string,
	r *http.Request,
) (string, bool, error) {
	switch location {
	case "path":
		value, ok := pathParams[name]
		return value, ok, nil
	case "query":
		values := r.URL.Query()[name]
		if len(values) > 1 {
			return "", true, fmt.Errorf("content query parameter %q appears more than once", name)
		}
		if len(values) == 0 {
			return "", false, nil
		}
		return values[0], true, nil
	case "header":
		values := r.Header.Values(name)
		if len(values) > 1 {
			return "", true, fmt.Errorf("content header parameter %q appears more than once", name)
		}
		if len(values) == 0 {
			return "", false, nil
		}
		return values[0], true, nil
	case "cookie":
		values := make([]string, 0, 1)
		for _, cookie := range r.Cookies() {
			if cookie.Name == name {
				values = append(values, cookie.Value)
			}
		}
		if len(values) > 1 {
			return "", true, fmt.Errorf("content cookie parameter %q appears more than once", name)
		}
		if len(values) == 0 {
			return "", false, nil
		}
		return values[0], true, nil
	default:
		return "", false, fmt.Errorf("unsupported parameter location %q", location)
	}
}

func parameterSchema(param parameter) map[string]any {
	if param.Schema != nil {
		return param.Schema
	}
	_, media, ok := singleParameterContent(param.Content)
	if !ok {
		return nil
	}
	return media.Schema
}

func cookieParamValue(param parameter, r *http.Request) (any, bool, error) {
	style, explode := parameterStyle(param, "cookie")
	if style != "form" {
		return nil, false, fmt.Errorf("unsupported cookie parameter style %q", style)
	}

	schemaType, _ := param.Schema["type"].(string)
	cookies := r.Cookies()
	if schemaType == "object" && explode {
		result := map[string]any{}
		properties, _ := param.Schema["properties"].(map[string]any)
		for name, rawSchema := range properties {
			propertySchema, _ := rawSchema.(map[string]any)
			values := make([]string, 0, 1)
			for _, cookie := range cookies {
				if cookie.Name == name {
					values = append(values, cookie.Value)
				}
			}
			if len(values) == 0 {
				continue
			}
			if propertyType, _ := propertySchema["type"].(string); propertyType == "array" {
				items := make([]any, len(values))
				for index, value := range values {
					items[index] = value
				}
				result[name] = coerceParameterValue(items, propertySchema)
				continue
			}
			if len(values) > 1 {
				return nil, true, fmt.Errorf("cookie object property %q appears more than once", name)
			}
			result[name] = coerceParameterValue(values[0], propertySchema)
		}
		return coerceParameterValue(result, param.Schema), len(result) > 0, nil
	}

	values := make([]string, 0, 1)
	for _, cookie := range cookies {
		if cookie.Name == param.Name {
			values = append(values, cookie.Value)
		}
	}
	if len(values) == 0 {
		return "", false, nil
	}
	if schemaType == "array" && explode {
		items := make([]any, len(values))
		for index, value := range values {
			items[index] = value
		}
		return coerceParameterValue(items, param.Schema), true, nil
	}
	if len(values) > 1 {
		return nil, true, fmt.Errorf("cookie parameter %q appears more than once", param.Name)
	}
	if schemaType == "object" && !explode {
		parsed, err := parseAlternatingObject(values[0], ",", param.Schema)
		return parsed, true, err
	}
	return parseDelimitedValue(
		values[0],
		parameter{Style: style, Explode: &explode, Schema: param.Schema},
	), true, nil
}

func queryParamValue(param parameter, values url.Values) (any, bool, error) {
	schemaType, _ := param.Schema["type"].(string)
	style, explode := parameterStyle(param, "query")

	if style == "deepObject" && schemaType == "object" {
		result := map[string]any{}
		prefix := param.Name + "["
		for key, items := range values {
			if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, "]") || len(items) == 0 {
				continue
			}
			propertyName := strings.TrimSuffix(strings.TrimPrefix(key, prefix), "]")
			propertySchema := schemaProperty(param.Schema, propertyName)
			propertyType, _ := propertySchema["type"].(string)
			if propertyType == "array" {
				array := make([]any, len(items))
				for index, item := range items {
					array[index] = item
				}
				result[propertyName] = coerceParameterValue(array, propertySchema)
				continue
			}
			if len(items) > 1 {
				return nil, true, fmt.Errorf(
					"deepObject property %q appears more than once",
					param.Name+"."+propertyName,
				)
			}
			result[propertyName] = items[0]
		}
		return coerceParameterValue(result, param.Schema), len(result) > 0, nil
	}
	if schemaType == "object" && style == "form" && explode {
		result := map[string]any{}
		properties, _ := param.Schema["properties"].(map[string]any)
		additionalSchema, additionalAllowed := additionalPropertySchema(param.Schema)
		for name, propertyValues := range values {
			if len(propertyValues) == 0 {
				continue
			}
			propertySchema, known := properties[name].(map[string]any)
			if !known {
				if !additionalAllowed {
					continue
				}
				propertySchema = additionalSchema
			}
			propertyType, _ := propertySchema["type"].(string)
			if propertyType == "array" {
				array := make([]any, len(propertyValues))
				for index, item := range propertyValues {
					array[index] = item
				}
				result[name] = coerceParameterValue(array, propertySchema)
				continue
			}
			if len(propertyValues) > 1 {
				return nil, true, fmt.Errorf("form object property %q appears more than once", name)
			}
			result[name] = coerceParameterValue(propertyValues[0], propertySchema)
		}
		return coerceParameterValue(result, param.Schema), len(result) > 0, nil
	}

	items, ok := values[param.Name]
	if !ok || len(items) == 0 {
		return "", false, nil
	}
	if schemaType == "array" && style == "form" && !explode && len(items) > 1 {
		return nil, true, fmt.Errorf("form array parameter %q with explode=false must appear once", param.Name)
	}
	if len(items) == 1 {
		if schemaType == "array" && style == "form" && explode {
			return coerceParameterValue([]any{items[0]}, param.Schema), true, nil
		}
		if schemaType == "object" && style == "form" && !explode {
			parsed, err := parseAlternatingObject(items[0], ",", param.Schema)
			return parsed, true, err
		}
		if schemaType == "object" && (style == "spaceDelimited" || style == "pipeDelimited") {
			delimiter := " "
			if style == "pipeDelimited" {
				delimiter = "|"
			}
			parsed, err := parseAlternatingObject(items[0], delimiter, param.Schema)
			return parsed, true, err
		}
		return parseDelimitedValue(
			items[0],
			parameter{Style: style, Explode: &explode, Schema: param.Schema},
		), true, nil
	}
	array := make([]any, len(items))
	if schemaType == "array" && (style == "spaceDelimited" || style == "pipeDelimited") {
		return parseRepeatedDelimitedArray(items, style, param.Schema), true, nil
	}
	for index, item := range items {
		array[index] = item
	}
	return coerceParameterValue(array, param.Schema), true, nil
}

func parseRepeatedDelimitedArray(values []string, style string, schema map[string]any) []any {
	delimiter := " "
	if style == "pipeDelimited" {
		delimiter = "|"
	}
	items := make([]any, 0, len(values))
	for _, value := range values {
		for item := range strings.SplitSeq(value, delimiter) {
			items = append(items, item)
		}
	}
	return coerceParameterValue(items, schema).([]any)
}

func parameterStyle(param parameter, location string) (string, bool) {
	style := param.Style
	if style == "" {
		if location == "query" || location == "cookie" {
			style = "form"
		} else {
			style = "simple"
		}
	}
	explode := style == "form" || style == "deepObject"
	if param.Explode != nil {
		explode = *param.Explode
	}
	return style, explode
}

func parsePathParameter(raw string, param parameter) (any, error) {
	style, explode := parameterStyle(param, "path")
	switch style {
	case "matrix":
		return parseMatrixParameter(raw, param, explode)
	case "label":
		return parseLabelParameter(raw, param, explode)
	case "simple":
		return parseSimpleParameterWithExplode(raw, param, explode)
	default:
		return coerceParameterValue(raw, param.Schema), nil
	}
}

func parseSimpleParameter(raw string, param parameter) (any, error) {
	_, explode := parameterStyle(param, "header")
	return parseSimpleParameterWithExplode(raw, param, explode)
}

func parseSimpleParameterWithExplode(raw string, param parameter, explode bool) (any, error) {
	schemaType, _ := param.Schema["type"].(string)
	switch schemaType {
	case "array":
		return parseArrayParameter(raw, ",", param.Schema), nil
	case "object":
		if explode {
			return parseKeyValueObject(raw, ",", param.Schema)
		}
		return parseAlternatingObject(raw, ",", param.Schema)
	default:
		return coerceParameterValue(raw, param.Schema), nil
	}
}

func parseLabelParameter(raw string, param parameter, explode bool) (any, error) {
	if !strings.HasPrefix(raw, ".") {
		return nil, fmt.Errorf("label parameter %q must start with '.'", param.Name)
	}
	raw = strings.TrimPrefix(raw, ".")
	schemaType, _ := param.Schema["type"].(string)
	switch schemaType {
	case "array":
		delimiter := ","
		if explode {
			delimiter = "."
		}
		return parseArrayParameter(raw, delimiter, param.Schema), nil
	case "object":
		if explode {
			return parseKeyValueObject(raw, ".", param.Schema)
		}
		return parseAlternatingObject(raw, ",", param.Schema)
	default:
		return coerceParameterValue(raw, param.Schema), nil
	}
}

func parseMatrixParameter(raw string, param parameter, explode bool) (any, error) {
	if !strings.HasPrefix(raw, ";") {
		return nil, fmt.Errorf("matrix parameter %q must start with ';'", param.Name)
	}
	fields := strings.Split(strings.TrimPrefix(raw, ";"), ";")
	schemaType, _ := param.Schema["type"].(string)
	switch schemaType {
	case "array":
		if explode {
			values := make([]any, 0, len(fields))
			for _, field := range fields {
				key, value, ok := strings.Cut(field, "=")
				if !ok || key != param.Name {
					return nil, fmt.Errorf("matrix array parameter %q has invalid field %q", param.Name, field)
				}
				values = append(values, value)
			}
			return coerceParameterValue(values, param.Schema), nil
		}
		if len(fields) != 1 {
			return nil, fmt.Errorf("matrix parameter %q must contain one field when explode is false", param.Name)
		}
		key, value, ok := strings.Cut(fields[0], "=")
		if !ok || key != param.Name {
			return nil, fmt.Errorf("matrix array parameter %q has invalid field %q", param.Name, fields[0])
		}
		return parseArrayParameter(value, ",", param.Schema), nil
	case "object":
		if explode {
			return parseKeyValueObject(strings.Join(fields, ";"), ";", param.Schema)
		}
		if len(fields) != 1 {
			return nil, fmt.Errorf(
				"matrix object parameter %q must contain one field when explode is false",
				param.Name,
			)
		}
		key, value, ok := strings.Cut(fields[0], "=")
		if !ok || key != param.Name {
			return nil, fmt.Errorf("matrix object parameter %q has invalid field %q", param.Name, fields[0])
		}
		return parseAlternatingObject(value, ",", param.Schema)
	default:
		if len(fields) != 1 {
			return nil, fmt.Errorf("matrix parameter %q must contain one field", param.Name)
		}
		key, value, ok := strings.Cut(fields[0], "=")
		if !ok || key != param.Name {
			return nil, fmt.Errorf("matrix parameter %q has invalid field %q", param.Name, fields[0])
		}
		return coerceParameterValue(value, param.Schema), nil
	}
}

func parseArrayParameter(raw, delimiter string, schema map[string]any) any {
	parts := strings.Split(raw, delimiter)
	items := make([]any, len(parts))
	for index, part := range parts {
		items[index] = part
	}
	return coerceParameterValue(items, schema)
}

func parseKeyValueObject(raw, delimiter string, schema map[string]any) (map[string]any, error) {
	parts := strings.Split(raw, delimiter)
	result := make(map[string]any, len(parts))
	for _, part := range parts {
		key, value, ok := strings.Cut(part, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("object parameter contains invalid field %q", part)
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("object parameter contains duplicate field %q", key)
		}
		result[key] = coerceParameterValue(value, schemaProperty(schema, key))
	}
	return result, nil
}

func parseAlternatingObject(raw, delimiter string, schema map[string]any) (map[string]any, error) {
	parts := strings.Split(raw, delimiter)
	if len(parts) == 0 || len(parts)%2 != 0 {
		return nil, fmt.Errorf("object parameter must contain alternating names and values")
	}

	result := make(map[string]any, len(parts)/2)
	for index := 0; index < len(parts); index += 2 {
		key := parts[index]
		if key == "" {
			return nil, fmt.Errorf("object parameter contains an empty field name")
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("object parameter contains duplicate field %q", key)
		}
		result[key] = coerceParameterValue(parts[index+1], schemaProperty(schema, key))
	}
	return result, nil
}

func parseDelimitedValue(value any, param parameter) any {
	schemaType, _ := param.Schema["type"].(string)
	if schemaType != "array" {
		return coerceParameterValue(value, param.Schema)
	}
	var raw string
	switch typed := value.(type) {
	case string:
		raw = typed
	case []any:
		return coerceParameterValue(typed, param.Schema)
	default:
		return value
	}
	delimiter := ","
	switch param.Style {
	case "spaceDelimited":
		delimiter = " "
	case "pipeDelimited":
		delimiter = "|"
	}
	parts := strings.Split(raw, delimiter)
	items := make([]any, len(parts))
	for index, part := range parts {
		items[index] = part
	}
	return coerceParameterValue(items, param.Schema)
}

func coerceParameterValue(value any, schema map[string]any) any {
	if schema == nil {
		return value
	}
	switch typed := value.(type) {
	case string:
		return coerceValue(typed, schema)
	case []any:
		itemSchema, _ := schema["items"].(map[string]any)
		for index, item := range typed {
			typed[index] = coerceParameterValue(item, itemSchema)
		}
		return typed
	case map[string]any:
		for name, item := range typed {
			typed[name] = coerceParameterValue(item, schemaProperty(schema, name))
		}
		return typed
	default:
		return value
	}
}

type xmlBodyNode struct {
	name     string
	attrs    map[string]string
	text     string
	children []*xmlBodyNode
}

func parseXMLBody(body []byte) (*xmlBodyNode, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	var root *xmlBodyNode
	stack := make([]*xmlBodyNode, 0, 8)
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		switch typed := token.(type) {
		case xml.StartElement:
			node := &xmlBodyNode{
				name:  typed.Name.Local,
				attrs: make(map[string]string, len(typed.Attr)),
			}
			for _, attr := range typed.Attr {
				node.attrs[attr.Name.Local] = attr.Value
			}
			if len(stack) == 0 {
				if root != nil {
					return nil, fmt.Errorf("XML body contains multiple root elements")
				}
				root = node
			} else {
				stack[len(stack)-1].children = append(stack[len(stack)-1].children, node)
			}
			stack = append(stack, node)
		case xml.EndElement:
			if len(stack) == 0 || stack[len(stack)-1].name != typed.Name.Local {
				return nil, fmt.Errorf("XML body contains an unmatched closing element %q", typed.Name.Local)
			}
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].text += string(typed)
			}
		}
	}
	if root == nil {
		return nil, fmt.Errorf("XML body is empty")
	}
	if len(stack) != 0 {
		return nil, fmt.Errorf("XML body contains an unclosed element")
	}
	return root, nil
}

func xmlBodyValue(node *xmlBodyNode, schema map[string]any) any {
	if node == nil {
		return nil
	}
	schemaType, _ := schema["type"].(string)
	switch schemaType {
	case "object":
		return xmlObjectValue(node, schema)
	case "array":
		itemSchema, _ := schema["items"].(map[string]any)
		items := make([]any, 0, len(node.children))
		for _, child := range node.children {
			items = append(items, xmlBodyValue(child, itemSchema))
		}
		if len(items) == 0 && strings.TrimSpace(node.text) != "" {
			items = append(items, strings.TrimSpace(node.text))
		}
		return items
	default:
		if len(node.children) > 0 {
			return xmlObjectValue(node, schema)
		}
		return strings.TrimSpace(node.text)
	}
}

func xmlObjectValue(node *xmlBodyNode, schema map[string]any) map[string]any {
	result := map[string]any{}
	properties, _ := schema["properties"].(map[string]any)
	for jsonName, rawSchema := range properties {
		propertySchema, _ := rawSchema.(map[string]any)
		elementName, attribute, _ := xmlPropertyMetadata(jsonName, propertySchema)
		if !attribute {
			continue
		}
		if value, ok := node.attrs[elementName]; ok {
			result[jsonName] = value
		}
	}

	groups := make(map[string][]*xmlBodyNode, len(node.children))
	order := make([]string, 0, len(node.children))
	groupSchemas := make(map[string]map[string]any, len(node.children))
	for _, child := range node.children {
		jsonName, propertySchema := xmlPropertyForElement(schema, child.name)
		if _, ok := groups[jsonName]; !ok {
			order = append(order, jsonName)
		}
		groups[jsonName] = append(groups[jsonName], child)
		groupSchemas[jsonName] = propertySchema
	}

	for _, jsonName := range order {
		children := groups[jsonName]
		propertySchema := groupSchemas[jsonName]
		propertyType, _ := propertySchema["type"].(string)
		if propertyType == "array" {
			_, _, wrapped := xmlPropertyMetadata(jsonName, propertySchema)
			if wrapped {
				result[jsonName] = xmlBodyValue(children[0], propertySchema)
				continue
			}
			itemSchema, _ := propertySchema["items"].(map[string]any)
			items := make([]any, 0, len(children))
			for _, child := range children {
				items = append(items, xmlBodyValue(child, itemSchema))
			}
			result[jsonName] = items
			continue
		}
		result[jsonName] = xmlBodyValue(children[0], propertySchema)
	}
	return result
}

func xmlPropertyForElement(schema map[string]any, elementName string) (string, map[string]any) {
	properties, _ := schema["properties"].(map[string]any)
	for jsonName, rawSchema := range properties {
		propertySchema, _ := rawSchema.(map[string]any)
		name, _, _ := xmlPropertyMetadata(jsonName, propertySchema)
		if name == elementName {
			return jsonName, propertySchema
		}
	}
	return elementName, nil
}

func xmlPropertyMetadata(jsonName string, schema map[string]any) (string, bool, bool) {
	name := jsonName
	attribute := false
	wrapped := false
	metadata, _ := schema["xml"].(map[string]any)
	if value, ok := metadata["name"].(string); ok && value != "" {
		name = value
	}
	if value, ok := metadata["attribute"].(bool); ok {
		attribute = value
	}
	if value, ok := metadata["wrapped"].(bool); ok {
		wrapped = value
	}
	return name, attribute, wrapped
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
	mediaParams := map[string]string{}
	if parsedType, params, parseErr := mime.ParseMediaType(r.Header.Get("Content-Type")); parseErr == nil {
		contentType = strings.ToLower(parsedType)
		mediaParams = params
	}
	media, ok := selectMediaType(body.Content, contentType)
	if !ok || media.Schema == nil {
		if len(body.Content) > 0 {
			return fmt.Errorf("unsupported request body content type %q", contentType)
		}
		return nil
	}

	var data any
	switch contentType {
	case "application/x-www-form-urlencoded":
		values, err := url.ParseQuery(string(rawBody))
		if err != nil {
			return fmt.Errorf("invalid request body form: %w", err)
		}
		data = formBodyValue(values, media.Schema)
	case "multipart/form-data":
		data, err = multipartBodyValue(rawBody, mediaParams["boundary"], media.Schema)
		if err != nil {
			return fmt.Errorf("invalid request body multipart: %w", err)
		}
	case "application/xml", "text/xml":
		node, parseErr := parseXMLBody(rawBody)
		if parseErr != nil {
			return fmt.Errorf("invalid request body XML: %w", parseErr)
		}
		data = xmlBodyValue(node, media.Schema)
	case "application/yaml", "text/yaml", "application/x-yaml":
		data, err = parseYAMLBody(rawBody)
		if err != nil {
			return fmt.Errorf("invalid request body YAML: %w", err)
		}
	case "text/plain", "application/octet-stream":
		data = string(rawBody)
	default:
		if strings.HasSuffix(contentType, "+xml") {
			node, parseErr := parseXMLBody(rawBody)
			if parseErr != nil {
				return fmt.Errorf("invalid request body XML: %w", parseErr)
			}
			data = xmlBodyValue(node, media.Schema)
			break
		}
		if strings.HasSuffix(contentType, "+yaml") {
			data, err = parseYAMLBody(rawBody)
			if err != nil {
				return fmt.Errorf("invalid request body YAML: %w", err)
			}
			break
		}
		if strings.HasSuffix(contentType, "+json") || contentType == "application/json" {
			if err := json.Unmarshal(rawBody, &data); err != nil {
				return fmt.Errorf("invalid request body JSON: %w", err)
			}
			break
		}
		if schemaType, _ := media.Schema["type"].(string); schemaType == "string" {
			// OpenAPI permits arbitrary media-type keys. Without a local codec,
			// preserve scalar string bodies as opaque bytes instead of guessing JSON.
			data = string(rawBody)
			break
		}
		return fmt.Errorf("unsupported request body content type %q", contentType)
	}
	data = coerceBodyValue(data, media.Schema)
	if err := validateAny(data, media.Schema); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}

func selectMediaType(content map[string]mediaType, actual string) (mediaType, bool) {
	actual = normalizeMediaType(actual)
	bestKey := ""
	bestScore := -1
	var best mediaType
	for key, candidate := range content {
		score := mediaTypeMatchScore(normalizeMediaType(key), actual)
		if score < bestScore || (score == bestScore && (bestKey == "" || key >= bestKey)) {
			continue
		}
		bestKey = key
		bestScore = score
		best = candidate
	}
	return best, bestScore >= 0
}

func normalizeMediaType(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.SplitN(value, ";", 2)[0]))
}

func mediaTypeMatchScore(pattern, actual string) int {
	if pattern == "" || actual == "" {
		return -1
	}
	if pattern == actual {
		return 100
	}
	patternType, patternSubtype, patternOK := strings.Cut(pattern, "/")
	actualType, actualSubtype, actualOK := strings.Cut(actual, "/")
	if !patternOK || !actualOK || (patternType != "*" && patternType != actualType) {
		return -1
	}
	if patternType == "*" && patternSubtype == "*" {
		return 10
	}
	if patternSubtype == "*" {
		return 20
	}
	if strings.HasPrefix(patternSubtype, "*+") &&
		strings.HasSuffix(actualSubtype, patternSubtype[1:]) &&
		len(actualSubtype) > len(patternSubtype)-1 {
		return 40
	}
	if patternType == actualType &&
		strings.HasSuffix(actualSubtype, "+"+patternSubtype) &&
		(patternSubtype == "json" || patternSubtype == "xml" || patternSubtype == "yaml") {
		return 30
	}
	return -1
}

func parseYAMLBody(rawBody []byte) (any, error) {
	var value any
	if err := yaml.Unmarshal(rawBody, &value); err != nil {
		return nil, err
	}
	return normalizeYAMLValue(value)
}

func normalizeYAMLValue(value any) (any, error) {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			normalized, err := normalizeYAMLValue(item)
			if err != nil {
				return nil, err
			}
			result[key] = normalized
		}
		return result, nil
	case map[any]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			name, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("object key %v is not a string", key)
			}
			normalized, err := normalizeYAMLValue(item)
			if err != nil {
				return nil, err
			}
			result[name] = normalized
		}
		return result, nil
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			normalized, err := normalizeYAMLValue(item)
			if err != nil {
				return nil, err
			}
			result[index] = normalized
		}
		return result, nil
	default:
		return value, nil
	}
}

func multipartBodyValue(rawBody []byte, boundary string, schema map[string]any) (map[string]any, error) {
	if boundary == "" {
		return nil, fmt.Errorf("multipart boundary is missing")
	}
	reader := multipart.NewReader(bytes.NewReader(rawBody), boundary)
	values := url.Values{}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		name := part.FormName()
		if name == "" {
			continue
		}
		value, err := io.ReadAll(part)
		if err != nil {
			return nil, err
		}
		values.Add(name, string(value))
	}
	return formBodyValue(values, schema), nil
}

func formBodyValue(values url.Values, schema map[string]any) map[string]any {
	data := make(map[string]any, len(values))
	for name, items := range values {
		if len(items) == 1 {
			data[name] = coerceBodyValue(items[0], schemaProperty(schema, name))
			continue
		}
		array := make([]any, len(items))
		for index, item := range items {
			array[index] = item
		}
		data[name] = coerceBodyValue(array, schemaProperty(schema, name))
	}
	return data
}

func schemaProperty(schema map[string]any, name string) map[string]any {
	properties, _ := schema["properties"].(map[string]any)
	if property, ok := properties[name].(map[string]any); ok {
		return property
	}
	property, _ := additionalPropertySchema(schema)
	return property
}

func additionalPropertySchema(schema map[string]any) (map[string]any, bool) {
	value, ok := schema["additionalProperties"]
	if !ok {
		return nil, false
	}
	switch typed := value.(type) {
	case map[string]any:
		return typed, true
	case bool:
		return nil, typed
	default:
		return nil, false
	}
}

func coerceBodyValue(value any, schema map[string]any) any {
	if schema == nil {
		return value
	}
	switch typed := value.(type) {
	case map[string]any:
		for name, item := range typed {
			typed[name] = coerceBodyValue(item, schemaProperty(schema, name))
		}
		return typed
	case []any:
		itemSchema, _ := schema["items"].(map[string]any)
		for index, item := range typed {
			typed[index] = coerceBodyValue(item, itemSchema)
		}
		return typed
	case string:
		return coerceValue(typed, schema)
	default:
		return value
	}
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

func validateValue(value any, schema map[string]any) error {
	return validateAny(coerceParameterValue(value, schema), schema)
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
