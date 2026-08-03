package body_transformer

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	pluginexpr "github.com/wklken/apisix-go/pkg/plugin/expr"
	"go.yaml.in/yaml/v3"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 1080
	name     = "body-transformer"
)

const schema = `
{
  "type": "object",
  "properties": {
    "request": {
      "type": "object",
      "properties": {
        "input_format": {
          "type": "string",
          "enum": ["xml", "json", "yaml", "encoded", "args", "plain", "multipart"]
        },
        "template": {
          "type": "string"
        },
        "template_is_base64": {
          "type": "boolean"
        }
      },
      "required": ["template"]
    },
    "response": {
      "type": "object",
      "properties": {
        "input_format": {
          "type": "string",
          "enum": ["xml", "json", "yaml", "encoded", "args", "plain", "multipart"]
        },
        "template": {
          "type": "string"
        },
        "template_is_base64": {
          "type": "boolean"
        }
      },
      "required": ["template"]
    }
  },
  "anyOf": [
    {
      "required": ["request"]
    },
    {
      "required": ["response"]
    }
  ]
}
`

type Config struct {
	Request  *Transform `json:"request,omitempty"`
	Response *Transform `json:"response,omitempty"`
}

type Transform struct {
	InputFormat      string `json:"input_format,omitempty"`
	Template         string `json:"template"`
	TemplateIsBase64 bool   `json:"template_is_base64,omitempty"`
}

type templateContext struct {
	values     map[string]string
	structured map[string]any
	body       string
	req        *http.Request
	format     string
}

type responseRecorder struct {
	header      http.Header
	body        bytes.Buffer
	statusCode  int
	wroteHeader bool
}

var (
	templateExprPattern    = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)
	templateRawExprPattern = regexp.MustCompile(`\{\*\s*([^{}]+?)\s*\*\}`)
	templateCallPattern    = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_.]*)\s*\(`)
)

var reservedTemplateValues = [...]string{"_ctx", "_body", "_escape_json", "_escape_xml", "_multipart"}

type templateRenderingError struct {
	err error
}

func (e *templateRenderingError) Error() string {
	return e.err.Error()
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
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		var err error
		if p.config.Request != nil {
			r, err = p.transformRequest(r)
			if err != nil {
				var renderingError *templateRenderingError
				if errors.As(err, &renderingError) {
					logger.Errorf("transform(): request template rendering: %s", renderingError)
					http.Error(w, renderingError.Error(), http.StatusServiceUnavailable)
					return
				}
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}

		if p.config.Response == nil {
			next.ServeHTTP(w, r)
			return
		}

		recorder := newResponseRecorder()
		next.ServeHTTP(recorder, r)
		if err := p.transformResponse(r, recorder); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		recorder.writeTo(w)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) transformRequest(r *http.Request) (*http.Request, error) {
	body, err := base.ReadRequestBody(r)
	if err != nil {
		return r, err
	}

	format := p.detectFormat(p.config.Request, r.Header.Get("Content-Type"), r.Method)
	ctx, err := p.buildTemplateContext(r, body, format, "request", r.Header.Get("Content-Type"))
	if err != nil {
		return r, err
	}
	out, err := renderTemplate(p.config.Request, ctx)
	if err != nil {
		return r, err
	}

	bodyReader := bytes.NewReader([]byte(out))
	r.Body = io.NopCloser(bodyReader)
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader([]byte(out))), nil
	}
	r.ContentLength = int64(len(out))
	r.Header.Set("Content-Length", fmt.Sprint(len(out)))
	return r, nil
}

func (p *Plugin) transformResponse(r *http.Request, recorder *responseRecorder) error {
	format := p.detectFormat(p.config.Response, recorder.header.Get("Content-Type"), "")
	ctx, err := p.buildTemplateContext(
		r,
		recorder.body.Bytes(),
		format,
		"response",
		recorder.header.Get("Content-Type"),
	)
	if err != nil {
		return err
	}
	out, err := renderTemplate(p.config.Response, ctx)
	if err != nil {
		return err
	}

	recorder.body.Reset()
	_, _ = recorder.body.WriteString(out)
	recorder.header.Del("Content-Length")
	return nil
}

func (p *Plugin) detectFormat(transform *Transform, contentType string, method string) string {
	if method == http.MethodGet {
		return "args"
	}
	if transform.InputFormat != "" {
		return transform.InputFormat
	}

	contentType = strings.ToLower(contentType)
	switch {
	case strings.Contains(contentType, "application/json"):
		return "json"
	case strings.Contains(contentType, "application/x-www-form-urlencoded"):
		return "encoded"
	case strings.Contains(contentType, "text/xml"):
		return "xml"
	case strings.Contains(contentType, "multipart/"):
		return "multipart"
	default:
		return "plain"
	}
}

func (p *Plugin) buildTemplateContext(
	r *http.Request,
	body []byte,
	format string,
	phase string,
	contentType string,
) (templateContext, error) {
	ctx := templateContext{
		values:     map[string]string{},
		structured: map[string]any{},
		body:       string(body),
		req:        r,
		format:     format,
	}

	switch format {
	case "json":
		if len(bytes.TrimSpace(body)) == 0 {
			return ctx, nil
		}
		var data any
		if err := json.Unmarshal(body, &data); err != nil {
			return ctx, fmt.Errorf("%s body decode: %w", phase, err)
		}
		flattenValues("", data, ctx.values)
	case "yaml":
		if len(bytes.TrimSpace(body)) == 0 {
			return ctx, nil
		}
		var data any
		if err := yaml.Unmarshal(body, &data); err != nil {
			return ctx, fmt.Errorf("%s body decode: %w", phase, err)
		}
		flattenValues("", data, ctx.values)
	case "encoded":
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return ctx, fmt.Errorf("%s body decode: %w", phase, err)
		}
		for key, value := range values {
			setRepeatedValues(ctx.values, key, value)
		}
	case "args":
		for key, value := range r.URL.Query() {
			setRepeatedValues(ctx.values, key, value)
		}
	case "xml":
		if len(bytes.TrimSpace(body)) == 0 {
			return ctx, nil
		}
		if err := flattenXMLValues(body, ctx.values, ctx.structured); err != nil {
			logger.Errorf("Error Parsing XML: %s", err)
			return ctx, fmt.Errorf("%s body decode: %w", phase, err)
		}
	case "multipart":
		if err := flattenMultipartValues(body, contentType, ctx.values); err != nil {
			return ctx, fmt.Errorf("%s body decode: %w", phase, err)
		}
	case "plain", "":
	}
	for _, reserved := range reservedTemplateValues {
		delete(ctx.values, reserved)
	}
	return ctx, nil
}

func renderTemplate(transform *Transform, ctx templateContext) (string, error) {
	text := transform.Template
	if transform.TemplateIsBase64 || (ctx.format != "" && ctx.format != "encoded" && ctx.format != "args") {
		if decoded, err := base64.StdEncoding.DecodeString(text); err == nil {
			text = string(decoded)
		}
	}
	var err error
	if text, err = renderTemplateBlocks(text, ctx); err != nil {
		return "", err
	}
	if err := validateTemplate(text); err != nil {
		return "", err
	}
	if err := validateTemplateFunctionCalls(text); err != nil {
		return "", &templateRenderingError{err: err}
	}

	text = templateRawExprPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := templateRawExprPattern.FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}
		return resolveExpression(strings.TrimSpace(parts[1]), ctx)
	})
	return templateExprPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := templateExprPattern.FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}
		return escapeTemplateHTML(resolveExpression(strings.TrimSpace(parts[1]), ctx))
	}), nil
}

func escapeTemplateHTML(value string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&#34;",
		"'", "&#39;",
		"/", "&#47;",
	).Replace(value)
}

func validateTemplateFunctionCalls(text string) error {
	for _, pattern := range []*regexp.Regexp{templateRawExprPattern, templateExprPattern} {
		for _, expression := range pattern.FindAllStringSubmatch(text, -1) {
			if len(expression) != 2 {
				continue
			}
			for _, call := range templateCallPattern.FindAllStringSubmatch(expression[1], -1) {
				if len(call) != 2 {
					continue
				}
				switch call[1] {
				case "_escape_json", "_escape_xml", "string.gsub":
					continue
				}
				name := strings.Split(call[1], ".")[0]
				return fmt.Errorf("attempt to call global '%s' (a string value)", name)
			}
		}
	}
	return nil
}

func validateTemplate(text string) error {
	if err := validateTemplateDelimiter(text, "{*", "*}", "raw expression"); err != nil {
		return err
	}
	withoutRawExpressions := templateRawExprPattern.ReplaceAllString(text, "")
	return validateTemplateDelimiter(withoutRawExpressions, "{{", "}}", "expression")
}

func validateTemplateDelimiter(text, openDelimiter, closeDelimiter, kind string) error {
	position := 0
	for position < len(text) {
		open := strings.Index(text[position:], openDelimiter)
		close := strings.Index(text[position:], closeDelimiter)
		if close >= 0 && (open < 0 || close < open) {
			return fmt.Errorf("template contains an unmatched closing delimiter for %s", kind)
		}
		if open < 0 {
			return nil
		}
		open += position
		close = strings.Index(text[open+len(openDelimiter):], closeDelimiter)
		if close < 0 {
			return fmt.Errorf("template contains an unmatched opening delimiter for %s", kind)
		}
		close += open + len(openDelimiter)
		if strings.TrimSpace(text[open+len(openDelimiter):close]) == "" {
			return fmt.Errorf("template %s is empty", kind)
		}
		position = close + len(closeDelimiter)
	}
	return nil
}

type templateIfBranch struct {
	condition string
	body      string
}

func renderTemplateBlocks(text string, ctx templateContext) (string, error) {
	for {
		start := strings.Index(text, "{%")
		if start < 0 {
			return text, nil
		}
		directiveEnd := strings.Index(text[start+2:], "%}")
		if directiveEnd < 0 {
			return "", errors.New("template contains an unmatched opening block delimiter")
		}
		directiveEnd += start + 2
		directive := strings.TrimSpace(text[start+2 : directiveEnd])
		if !strings.HasPrefix(directive, "if ") {
			return "", fmt.Errorf("unsupported template directive %q", directive)
		}

		condition, err := parseTemplateConditionDirective(directive, "if")
		if err != nil {
			return "", err
		}
		branches, elseBody, after, err := findTemplateIfBlock(text, condition, directiveEnd+2)
		if err != nil {
			return "", err
		}
		selected := elseBody
		for _, branch := range branches {
			if evaluateTemplateCondition(branch.condition, ctx) {
				selected = branch.body
				break
			}
		}
		rendered, err := renderTemplateBlocks(selected, ctx)
		if err != nil {
			return "", err
		}
		text = text[:start] + rendered + text[after:]
	}
}

func parseTemplateConditionDirective(directive, keyword string) (string, error) {
	prefix := keyword + " "
	if !strings.HasPrefix(directive, prefix) {
		return "", fmt.Errorf("unsupported template directive %q", directive)
	}
	condition := strings.TrimSpace(strings.TrimPrefix(directive, prefix))
	if !strings.HasSuffix(condition, "then") {
		return "", fmt.Errorf("template %s directive must end with then", keyword)
	}
	condition = strings.TrimSpace(strings.TrimSuffix(condition, "then"))
	if condition == "" {
		return "", fmt.Errorf("template %s directive condition is empty", keyword)
	}
	return condition, nil
}

func findTemplateIfBlock(
	text string,
	initialCondition string,
	contentStart int,
) (branches []templateIfBranch, elseBody string, after int, err error) {
	depth := 1
	hasElse := false
	elseBodyStart := -1
	currentBodyStart := contentStart
	branches = []templateIfBranch{{condition: initialCondition}}
	position := contentStart
	for position < len(text) {
		start := strings.Index(text[position:], "{%")
		if start < 0 {
			return nil, "", 0, errors.New("template if directive is missing end")
		}
		start += position
		end := strings.Index(text[start+2:], "%}")
		if end < 0 {
			return nil, "", 0, errors.New("template contains an unmatched block delimiter")
		}
		end += start + 2
		directive := strings.TrimSpace(text[start+2 : end])
		switch {
		case strings.HasPrefix(directive, "if "):
			depth++
		case strings.HasPrefix(directive, "elseif "):
			if depth != 1 || hasElse {
				return nil, "", 0, errors.New("template contains an invalid elseif directive")
			}
			branches[len(branches)-1].body = text[currentBodyStart:start]
			condition, conditionErr := parseTemplateConditionDirective(directive, "elseif")
			if conditionErr != nil {
				return nil, "", 0, conditionErr
			}
			branches = append(branches, templateIfBranch{condition: condition})
			currentBodyStart = end + 2
		case directive == "else":
			if depth != 1 || hasElse {
				return nil, "", 0, errors.New("template contains an invalid else directive")
			}
			branches[len(branches)-1].body = text[currentBodyStart:start]
			hasElse = true
			elseBodyStart = end + 2
		case directive == "end":
			depth--
			if depth == 0 {
				if hasElse {
					elseBody = text[elseBodyStart:start]
				} else {
					branches[len(branches)-1].body = text[currentBodyStart:start]
				}
				return branches, elseBody, end + 2, nil
			}
		default:
			return nil, "", 0, fmt.Errorf("unsupported template directive %q", directive)
		}
		position = end + 2
	}
	return nil, "", 0, errors.New("template if directive is missing end")
}

func evaluateTemplateCondition(expr string, ctx templateContext) bool {
	if parts := splitTemplateKeyword(expr, "or"); len(parts) > 1 {
		for _, part := range parts {
			if evaluateTemplateCondition(part, ctx) {
				return true
			}
		}
		return false
	}
	if parts := splitTemplateKeyword(expr, "and"); len(parts) > 1 {
		for _, part := range parts {
			if !evaluateTemplateCondition(part, ctx) {
				return false
			}
		}
		return true
	}
	if after, ok := strings.CutPrefix(strings.TrimSpace(expr), "not "); ok {
		return !evaluateTemplateCondition(strings.TrimSpace(after), ctx)
	}
	for _, operator := range []string{"~=", "==", ">=", "<=", ">", "<"} {
		parts := splitTemplateOperator(expr, operator)
		if len(parts) != 2 {
			continue
		}
		left, leftOK := templateConditionOperand(parts[0], ctx)
		right, rightOK := templateConditionOperand(parts[1], ctx)
		switch operator {
		case "==":
			return leftOK == rightOK && left == right
		case "~=":
			return leftOK != rightOK || left != right
		default:
			if !leftOK || !rightOK {
				return false
			}
			leftNumber, leftErr := strconv.ParseFloat(left, 64)
			rightNumber, rightErr := strconv.ParseFloat(right, 64)
			if leftErr != nil || rightErr != nil {
				return false
			}
			switch operator {
			case ">=":
				return leftNumber >= rightNumber
			case "<=":
				return leftNumber <= rightNumber
			case ">":
				return leftNumber > rightNumber
			case "<":
				return leftNumber < rightNumber
			}
		}
	}
	value, ok := templateConditionOperand(expr, ctx)
	return ok && value != "" && value != "false"
}

func templateConditionOperand(expr string, ctx templateContext) (string, bool) {
	expr = strings.TrimSpace(expr)
	if expr == "nil" {
		return "", false
	}
	if expr == "true" {
		return "true", true
	}
	if expr == "false" {
		return "false", true
	}
	if literal, ok := templateStringLiteral(expr); ok {
		return literal, true
	}
	if _, err := strconv.ParseFloat(expr, 64); err == nil {
		return expr, true
	}
	if expr == "_body" {
		return ctx.body, true
	}
	if strings.HasPrefix(expr, "_ctx.var.") {
		if ctx.req == nil {
			return "", false
		}
		return requestVar(ctx.req, strings.TrimPrefix(expr, "_ctx.var.")), true
	}
	if value, ok := ctx.values[expr]; ok {
		if value == "null" {
			return "", false
		}
		return value, true
	}
	if normalized := normalizeTemplatePath(expr); normalized != expr {
		if value, ok := ctx.values[normalized]; ok {
			if value == "null" {
				return "", false
			}
			return value, true
		}
	}
	return "", false
}

func splitTemplateKeyword(expr, keyword string) []string {
	parts := make([]string, 0, 2)
	start := 0
	depth := 0
	var quote byte
	for index := 0; index < len(expr); index++ {
		char := expr[index]
		if quote != 0 {
			if char == quote && (index == 0 || expr[index-1] != '\\') {
				quote = 0
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			continue
		}
		switch char {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		}
		if depth != 0 || !strings.HasPrefix(expr[index:], keyword) {
			continue
		}
		beforeBoundary := index == 0 || expr[index-1] == ' ' || expr[index-1] == '\t'
		after := index + len(keyword)
		afterBoundary := after == len(expr) || expr[after] == ' ' || expr[after] == '\t'
		if !beforeBoundary || !afterBoundary {
			continue
		}
		parts = append(parts, expr[start:index])
		start = after
		index = after - 1
	}
	if len(parts) == 0 {
		return nil
	}
	parts = append(parts, expr[start:])
	return parts
}

func resolveExpression(expr string, ctx templateContext) string {
	expr = strings.TrimSpace(expr)
	if strings.HasPrefix(expr, "string.gsub(") && strings.HasSuffix(expr, ")") {
		arguments := splitTemplateArguments(strings.TrimSuffix(strings.TrimPrefix(expr, "string.gsub("), ")"))
		if len(arguments) != 3 {
			return ""
		}
		return strings.ReplaceAll(
			resolveExpression(arguments[0], ctx),
			resolveExpression(arguments[1], ctx),
			resolveExpression(arguments[2], ctx),
		)
	}
	if strings.HasPrefix(expr, "_escape_json(") && strings.HasSuffix(expr, ")") {
		argument := strings.TrimSuffix(strings.TrimPrefix(expr, "_escape_json("), ")")
		if value, ok := structuredTemplateValue(argument, ctx); ok {
			encoded, err := json.Marshal(value)
			if err != nil {
				return ""
			}
			return string(encoded)
		}
		value := resolveExpression(argument, ctx)
		encoded, err := json.Marshal(value)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
	if strings.HasPrefix(expr, "_escape_xml(") && strings.HasSuffix(expr, ")") {
		value := resolveExpression(strings.TrimSuffix(strings.TrimPrefix(expr, "_escape_xml("), ")"), ctx)
		return html.EscapeString(value)
	}
	if parts := splitTemplateOperator(expr, ".."); len(parts) > 1 {
		var out strings.Builder
		for _, part := range parts {
			out.WriteString(resolveExpression(part, ctx))
		}
		return out.String()
	}
	if parts := splitTemplateOperator(expr, "+"); len(parts) > 1 {
		var sum float64
		for _, part := range parts {
			value, err := strconv.ParseFloat(resolveExpression(part, ctx), 64)
			if err != nil {
				return ""
			}
			sum += value
		}
		return strconv.FormatFloat(sum, 'f', -1, 64)
	}
	if literal, ok := templateStringLiteral(expr); ok {
		return literal
	}
	if _, err := strconv.ParseFloat(expr, 64); err == nil {
		return expr
	}
	if expr == "_body" {
		return ctx.body
	}
	if after, ok := strings.CutPrefix(expr, "_ctx.var."); ok {
		return requestVar(ctx.req, after)
	}
	if value, ok := ctx.values[expr]; ok {
		return value
	}
	if normalized := normalizeTemplatePath(expr); normalized != expr {
		if value, ok := ctx.values[normalized]; ok {
			return value
		}
	}
	return ""
}

func structuredTemplateValue(expr string, ctx templateContext) (any, bool) {
	expr = strings.TrimSpace(expr)
	if value, ok := ctx.structured[expr]; ok {
		return value, true
	}
	if normalized := normalizeTemplatePath(expr); normalized != expr {
		value, ok := ctx.structured[normalized]
		return value, ok
	}
	return nil, false
}

func splitTemplateArguments(expr string) []string {
	parts := make([]string, 0, 3)
	start := 0
	depth := 0
	var quote byte
	escaped := false
	for index := 0; index < len(expr); index++ {
		char := expr[index]
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if char == '\\' {
				escaped = true
				continue
			}
			if char == quote {
				quote = 0
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			continue
		}
		switch char {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(expr[start:index]))
				start = index + 1
			}
		}
	}
	parts = append(parts, strings.TrimSpace(expr[start:]))
	return parts
}

func splitTemplateOperator(expr, operator string) []string {
	parts := make([]string, 0, 2)
	start := 0
	depth := 0
	var quote byte
	escaped := false
	for index := 0; index < len(expr); index++ {
		char := expr[index]
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if char == '\\' {
				escaped = true
				continue
			}
			if char == quote {
				quote = 0
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			continue
		}
		switch char {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		}
		if depth == 0 && strings.HasPrefix(expr[index:], operator) {
			parts = append(parts, expr[start:index])
			index += len(operator) - 1
			start = index + 1
		}
	}
	if len(parts) == 0 {
		return nil
	}
	parts = append(parts, expr[start:])
	return parts
}

func templateStringLiteral(expr string) (string, bool) {
	if len(expr) < 2 || (expr[0] != '"' && expr[0] != '\'') || expr[len(expr)-1] != expr[0] {
		return "", false
	}
	if expr[0] == '\'' {
		var value strings.Builder
		for index := 1; index < len(expr)-1; index++ {
			if expr[index] != '\\' {
				value.WriteByte(expr[index])
				continue
			}
			if index+1 >= len(expr)-1 {
				return "", false
			}
			index++
			switch expr[index] {
			case 'a':
				value.WriteByte('\a')
			case 'b':
				value.WriteByte('\b')
			case 'f':
				value.WriteByte('\f')
			case 'n':
				value.WriteByte('\n')
			case 'r':
				value.WriteByte('\r')
			case 't':
				value.WriteByte('\t')
			case 'v':
				value.WriteByte('\v')
			case '\\', '\'', '"':
				value.WriteByte(expr[index])
			default:
				return "", false
			}
		}
		return value.String(), true
	}
	value, err := strconv.Unquote(expr)
	if err != nil {
		return "", false
	}
	return value, true
}

func normalizeTemplatePath(expr string) string {
	if !strings.Contains(expr, "[") {
		return expr
	}
	var out strings.Builder
	for index := 0; index < len(expr); index++ {
		if expr[index] != '[' {
			out.WriteByte(expr[index])
			continue
		}
		end := strings.IndexByte(expr[index+1:], ']')
		if end < 0 {
			return expr
		}
		end += index + 1
		part := strings.TrimSpace(expr[index+1 : end])
		if len(part) >= 2 && part[0] == '"' && part[len(part)-1] == '"' {
			part = part[1 : len(part)-1]
		}
		if part == "" {
			return expr
		}
		out.WriteByte('.')
		out.WriteString(part)
		index = end
	}
	return strings.TrimPrefix(out.String(), ".")
}

func flattenValues(prefix string, value any, out map[string]string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			nextKey := key
			if prefix != "" {
				nextKey = prefix + "." + key
			}
			flattenValues(nextKey, nested, out)
		}
	case []any:
		for i, nested := range typed {
			flattenValues(fmt.Sprintf("%s.%d", prefix, i), nested, out)
		}
	case string:
		out[prefix] = typed
	case float64, bool, nil:
		encoded, err := json.Marshal(typed)
		if err == nil {
			out[prefix] = string(encoded)
		}
	default:
		out[prefix] = fmt.Sprint(typed)
	}
}

func setRepeatedValues(out map[string]string, key string, values []string) {
	if len(values) == 0 {
		return
	}
	out[key] = values[0]
	for index, value := range values {
		out[fmt.Sprintf("%s.%d", key, index)] = value
	}
}

type xmlNode struct {
	name     string
	text     string
	attrs    map[string]string
	children []*xmlNode
}

func flattenXMLValues(body []byte, out map[string]string, structured map[string]any) error {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	root := &xmlNode{}
	stack := []*xmlNode{root}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		switch typed := token.(type) {
		case xml.StartElement:
			attrs := make(map[string]string, len(typed.Attr))
			for _, attr := range typed.Attr {
				attrs[attr.Name.Local] = attr.Value
			}
			node := &xmlNode{name: typed.Name.Local, attrs: attrs}
			stack[len(stack)-1].children = append(stack[len(stack)-1].children, node)
			stack = append(stack, node)
		case xml.EndElement:
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			text := strings.TrimSpace(string(typed))
			if text != "" && len(stack) > 1 {
				stack[len(stack)-1].text += text
			}
		}
	}
	if len(root.children) == 0 {
		return errors.New("XML document has no root element")
	}

	for _, node := range root.children {
		flattenXMLNode(node, node.name, out, structured)
	}
	return nil
}

func flattenXMLNode(node *xmlNode, prefix string, out map[string]string, structured map[string]any) {
	structured[prefix] = xmlNodeValue(node)
	for name, value := range node.attrs {
		out[fmt.Sprintf("%s._attr.%s", prefix, name)] = value
	}
	if node.text != "" {
		out[prefix] = node.text
	}
	if len(node.children) == 0 {
		return
	}

	groups := make(map[string][]*xmlNode, len(node.children))
	order := make([]string, 0, len(node.children))
	for _, child := range node.children {
		if _, ok := groups[child.name]; !ok {
			order = append(order, child.name)
		}
		groups[child.name] = append(groups[child.name], child)
	}
	for _, name := range order {
		children := groups[name]
		childPrefix := prefix + "." + name
		if len(children) == 1 {
			flattenXMLNode(children[0], childPrefix, out, structured)
			continue
		}
		for index, child := range children {
			flattenXMLNode(child, fmt.Sprintf("%s.%d", childPrefix, index), out, structured)
		}
	}
}

func xmlNodeValue(node *xmlNode) any {
	if len(node.children) == 0 && len(node.attrs) == 0 {
		return node.text
	}
	value := make(map[string]any, len(node.children)+1)
	if len(node.attrs) > 0 {
		attrs := make(map[string]any, len(node.attrs))
		for name, attrValue := range node.attrs {
			attrs[name] = attrValue
		}
		value["_attr"] = attrs
	}
	groups := make(map[string][]*xmlNode, len(node.children))
	for _, child := range node.children {
		groups[child.name] = append(groups[child.name], child)
	}
	for name, children := range groups {
		if len(children) == 1 {
			value[name] = xmlNodeValue(children[0])
			continue
		}
		items := make([]any, len(children))
		for index, child := range children {
			items[index] = xmlNodeValue(child)
		}
		value[name] = items
	}
	return value
}

func flattenMultipartValues(body []byte, contentType string, out map[string]string) error {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		return fmt.Errorf("content type %q is not multipart", contentType)
	}
	boundary := params["boundary"]
	if boundary == "" {
		return fmt.Errorf("multipart boundary is missing")
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	indices := make(map[string]int)
	partCount := 0
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			if partCount == 0 && len(bytes.TrimSpace(body)) > 0 {
				return fmt.Errorf("multipart body contains no parts")
			}
			return nil
		}
		if err != nil {
			return err
		}
		partCount++
		name := part.FormName()
		if name == "" {
			continue
		}
		value, err := io.ReadAll(part)
		if err != nil {
			return err
		}
		index := indices[name]
		if index == 0 {
			out[name] = string(value)
		}
		out[fmt.Sprintf("%s.%d", name, index)] = string(value)
		if filename := part.FileName(); filename != "" {
			out[fmt.Sprintf("%s.filename", name)] = filename
			out[fmt.Sprintf("%s.%d.filename", name, index)] = filename
		}
		indices[name] = index + 1
	}
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{
		header:     http.Header{},
		statusCode: http.StatusOK,
	}
}

func (r *responseRecorder) Header() http.Header {
	return r.header
}

func (r *responseRecorder) WriteHeader(statusCode int) {
	if r.wroteHeader {
		return
	}
	r.statusCode = statusCode
	r.wroteHeader = true
}

func (r *responseRecorder) Write(body []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.body.Write(body)
}

func (r *responseRecorder) writeTo(w http.ResponseWriter) {
	for field, values := range r.header {
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	w.WriteHeader(r.statusCode)
	_, _ = w.Write(r.body.Bytes())
}

func requestVar(r *http.Request, name string) string {
	return pluginexpr.String(pluginexpr.RequestValue(r, name))
}
