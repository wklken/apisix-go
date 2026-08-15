package oas_validator

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/wklken/apisix-go/pkg/json"
	"go.yaml.in/yaml/v3"
)

// compiledSpec is the kin-openapi view of a compiled OpenAPI document.
type compiledSpec struct {
	document *openapi3.T
	router   routers.Router
}

// compileSpec loads a document once into a router, extends structured-suffix
// media types (application/problem+json matches application/json) and
// registers body decoders for every declared content type.
func compileSpec(
	ctx context.Context,
	raw []byte,
	origin *url.URL,
	client *http.Client,
	headers map[string]string,
) (*compiledSpec, error) {
	loader := openapi3.NewLoader()
	loader.Context = ctx
	loader.IsExternalRefsAllowed = true
	documents, err := preloadDocumentGraph(
		ctx,
		raw,
		origin,
		func(ctx context.Context, refURL *url.URL) ([]byte, error) {
			return fetchDocument(ctx, client, refURL, headers, origin)
		},
	)
	if err != nil {
		return nil, err
	}
	loader.ReadFromURIFunc = func(_ *openapi3.Loader, refURL *url.URL) ([]byte, error) {
		document, ok := documents[documentURL(refURL)]
		if !ok {
			return nil, fmt.Errorf("external openapi document %q was not preloaded", refURL)
		}
		return append([]byte(nil), document...), nil
	}

	doc, err := loader.LoadFromDataWithPath(raw, refOrigin(origin))
	if err != nil {
		return nil, err
	}
	if err := doc.Validate(ctx); err != nil {
		return nil, err
	}
	normalizeSpecContentKeys(doc)
	registerSpecBodyDecoders(doc)

	router, err := legacy.NewRouter(doc)
	if err != nil {
		return nil, err
	}
	return &compiledSpec{document: doc, router: router}, nil
}

// refOrigin returns a usable document path for relative $ref resolution.
// Inline specs have no URL; any relative external $ref against the synthetic
// origin fails when fetched, matching the previous "requires a spec URL"
// rejection.
func refOrigin(origin *url.URL) *url.URL {
	if origin != nil {
		return origin
	}
	return &url.URL{Scheme: "http", Host: "openapi.invalid"}
}

// validateRequest matches the request against the compiled router and
// validates parameters and body, honoring the configured skip flags.
func validateRequest(ctx context.Context, req *http.Request, spec *compiledSpec, cfg Config) error {
	route, pathParams, err := spec.router.FindRoute(req)
	if err != nil {
		return mapRouteError(err, req)
	}
	normalizeBodyContentType(req, route.Operation)
	route = cloneRouteWithSkippedParameters(route, cfg)
	input := &openapi3filter.RequestValidationInput{
		Request:    req,
		Route:      route,
		PathParams: pathParams,
		Options: &openapi3filter.Options{
			ExcludeRequestBody:        cfg.SkipRequestBodyValidation,
			ExcludeRequestQueryParams: cfg.SkipQueryParamValidation,
			MultiError:                cfg.VerboseErrors,
			AuthenticationFunc:        openapi3filter.NoopAuthenticationFunc,
		},
	}
	return openapi3filter.ValidateRequest(ctx, input)
}

func mapRouteError(err error, req *http.Request) error {
	if errors.Is(err, routers.ErrPathNotFound) || errors.Is(err, routers.ErrMethodNotAllowed) {
		return fmt.Errorf("no matching operation for %s %s", req.Method, req.URL.Path)
	}
	return err
}

// cloneRouteWithSkippedParameters shallow-clones the matched operation and
// path item and removes header/cookie and path parameters whose skip flags
// are enabled. The shared document is never mutated.
func cloneRouteWithSkippedParameters(route *routers.Route, cfg Config) *routers.Route {
	if !cfg.SkipRequestHeaderValidation && !cfg.SkipPathParamsValidation {
		return route
	}
	operation := *route.Operation
	operation.Parameters = skipParameters(route.Operation.Parameters, cfg)
	cloned := *route
	cloned.Operation = &operation
	if route.PathItem != nil {
		pathItem := *route.PathItem
		pathItem.Parameters = skipParameters(route.PathItem.Parameters, cfg)
		cloned.PathItem = &pathItem
	}
	return &cloned
}

func skipParameters(parameters openapi3.Parameters, cfg Config) openapi3.Parameters {
	kept := make(openapi3.Parameters, 0, len(parameters))
	for _, parameter := range parameters {
		location := parameter.Value.In
		if cfg.SkipRequestHeaderValidation &&
			(location == openapi3.ParameterInHeader || location == openapi3.ParameterInCookie) {
			continue
		}
		if cfg.SkipPathParamsValidation && location == openapi3.ParameterInPath {
			continue
		}
		kept = append(kept, parameter)
	}
	return kept
}

// normalizeBodyContentType rewrites structured-suffix request content types
// (application/problem+json) to their base form so kin-openapi's exact and
// wildcard matching accepts them, mirroring the previous suffix scoring.
func normalizeBodyContentType(req *http.Request, operation *openapi3.Operation) {
	contentType := normalizeMediaType(req.Header.Get("Content-Type"))
	if contentType == "" || operation == nil || operation.RequestBody == nil || operation.RequestBody.Value == nil {
		return
	}
	content := operation.RequestBody.Value.Content
	if content.Get(contentType) != nil {
		return
	}
	if base := suffixBase(contentType); base != "" && content.Get(base) != nil {
		req.Header.Set("Content-Type", base)
	}
}

// normalizeSpecContentKeys registers the base form of every declared
// structured-suffix content key (application/problem+json and
// application/*+json both gain application/json) so the request-time
// normalization has an entry to match.
func normalizeSpecContentKeys(doc *openapi3.T) {
	for _, pathItem := range doc.Paths.Map() {
		for _, operation := range pathItem.Operations() {
			if operation.RequestBody == nil || operation.RequestBody.Value == nil {
				continue
			}
			content := operation.RequestBody.Value.Content
			additions := make(map[string]*openapi3.MediaType)
			for key, mediaType := range content {
				if base := suffixBase(key); base != "" && content[base] == nil {
					additions[base] = mediaType
				}
			}
			maps.Copy(content, additions)
		}
	}
}

// suffixBase returns the base media type of a structured-suffix content key
// (application/problem+json -> application/json) or "" when it has none.
func suffixBase(mediaType string) string {
	mediaType = normalizeMediaType(mediaType)
	mediaType, subtype, found := strings.Cut(mediaType, "/")
	if !found {
		return ""
	}
	_, suffix, found := strings.Cut(subtype, "+")
	if !found {
		return ""
	}
	switch suffix {
	case "json", "xml", "yaml":
		return mediaType + "/" + suffix
	default:
		return ""
	}
}

func normalizeMediaType(value string) string {
	return strings.ToLower(strings.TrimSpace(strings.SplitN(value, ";", 2)[0]))
}

// registerSpecBodyDecoders registers a family-based decoder for every content
// type a compiled document declares, plus the common JSON/XML/YAML/form/
// multipart/text/octet-stream keys.
func registerSpecBodyDecoders(doc *openapi3.T) {
	keys := map[string]struct{}{
		"application/json":                  {},
		"application/xml":                   {},
		"application/x-yaml":                {},
		"application/yaml":                  {},
		"application/x-www-form-urlencoded": {},
		"multipart/form-data":               {},
		"text/plain":                        {},
		"application/octet-stream":          {},
	}
	for _, pathItem := range doc.Paths.Map() {
		for _, operation := range pathItem.Operations() {
			if operation.RequestBody == nil || operation.RequestBody.Value == nil {
				continue
			}
			for key := range operation.RequestBody.Value.Content {
				keys[key] = struct{}{}
				if base := suffixBase(key); base != "" {
					keys[base] = struct{}{}
				}
			}
		}
	}
	for key := range keys {
		family := mediaTypeFamily(key)
		if openapi3filter.RegisteredBodyDecoder(key) != nil {
			continue
		}
		openapi3filter.RegisterBodyDecoder(key, bodyDecoderForFamily(family))
	}
}

func mediaTypeFamily(mediaType string) string {
	switch mediaType {
	case "application/json", "text/json":
		return "json"
	case "application/xml", "text/xml":
		return "xml"
	case "application/x-yaml", "application/yaml":
		return "yaml"
	case "application/x-www-form-urlencoded":
		return "form"
	case "multipart/form-data":
		return "multipart"
	case "text/plain":
		return "text"
	case "application/octet-stream":
		return "octet"
	default:
		if strings.HasSuffix(mediaType, "+json") {
			return "json"
		}
		if strings.HasSuffix(mediaType, "+xml") {
			return "xml"
		}
		if strings.HasSuffix(mediaType, "+yaml") {
			return "yaml"
		}
		return "string"
	}
}

func bodyDecoderForFamily(family string) openapi3filter.BodyDecoder {
	switch family {
	case "xml":
		return xmlBodyDecoder
	case "yaml":
		return yamlBodyDecoder
	case "form":
		return formBodyDecoder
	case "multipart":
		return multipartBodyDecoder
	case "text", "octet", "string":
		return stringBodyDecoder
	default:
		return jsonBodyDecoder
	}
}

func jsonBodyDecoder(body io.Reader, _ http.Header, _ *openapi3.SchemaRef, _ openapi3filter.EncodingFn) (any, error) {
	var value any
	if err := json.NewDecoder(body).Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func xmlBodyDecoder(
	body io.Reader,
	_ http.Header,
	schema *openapi3.SchemaRef,
	_ openapi3filter.EncodingFn,
) (any, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	node, err := parseXMLBody(raw)
	if err != nil {
		return nil, err
	}
	schemaMap := schemaRefToMap(schema)
	return coerceBodyValue(xmlBodyValue(node, schemaMap), schemaMap), nil
}

func yamlBodyDecoder(
	body io.Reader,
	_ http.Header,
	schema *openapi3.SchemaRef,
	_ openapi3filter.EncodingFn,
) (any, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	value, err := parseYAMLBody(raw)
	if err != nil {
		return nil, err
	}
	return coerceBodyValue(value, schemaRefToMap(schema)), nil
}

func formBodyDecoder(
	body io.Reader,
	_ http.Header,
	schema *openapi3.SchemaRef,
	_ openapi3filter.EncodingFn,
) (any, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	values, err := url.ParseQuery(string(raw))
	if err != nil {
		return nil, fmt.Errorf("invalid request body form: %w", err)
	}
	return formBodyValue(values, schemaRefToMap(schema)), nil
}

func multipartBodyDecoder(
	body io.Reader,
	header http.Header,
	schema *openapi3.SchemaRef,
	_ openapi3filter.EncodingFn,
) (any, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	boundary := ""
	if parsed, params, parseErr := mime.ParseMediaType(
		header.Get("Content-Type"),
	); parseErr == nil &&
		parsed == "multipart/form-data" {
		boundary = params["boundary"]
	}
	return multipartBodyValue(raw, boundary, schemaRefToMap(schema))
}

func stringBodyDecoder(body io.Reader, _ http.Header, _ *openapi3.SchemaRef, _ openapi3filter.EncodingFn) (any, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	return string(raw), nil
}

// schemaRefToMap converts a loaded schema back to the plain-map shape used by
// the body value converters.
func schemaRefToMap(schema *openapi3.SchemaRef) map[string]any {
	if schema == nil || schema.Value == nil {
		return nil
	}
	encoded, err := json.Marshal(schema.Value)
	if err != nil {
		return nil
	}
	var value map[string]any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return nil
	}
	return value
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
