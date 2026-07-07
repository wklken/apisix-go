package body_transformer

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
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
          "enum": ["xml", "json", "encoded", "args", "plain", "multipart"]
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
          "enum": ["xml", "json", "encoded", "args", "plain", "multipart"]
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
	values map[string]string
	body   string
	req    *http.Request
}

type responseRecorder struct {
	header      http.Header
	body        bytes.Buffer
	statusCode  int
	wroteHeader bool
}

var templateExprPattern = regexp.MustCompile(`\{\{\s*([^{}]+?)\s*\}\}`)

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
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		var err error
		if p.config.Request != nil {
			r, err = p.transformRequest(r)
			if err != nil {
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
	body, err := readBody(r)
	if err != nil {
		return r, err
	}

	format := p.detectFormat(p.config.Request, r.Header.Get("Content-Type"), r.Method)
	ctx, err := p.buildTemplateContext(r, body, format, "request")
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
	ctx, err := p.buildTemplateContext(r, recorder.body.Bytes(), format, "response")
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
) (templateContext, error) {
	ctx := templateContext{
		values: map[string]string{},
		body:   string(body),
		req:    r,
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
	case "encoded":
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return ctx, fmt.Errorf("%s body decode: %w", phase, err)
		}
		for key, value := range values {
			if len(value) > 0 {
				ctx.values[key] = value[0]
			}
		}
	case "args":
		for key, value := range r.URL.Query() {
			if len(value) > 0 {
				ctx.values[key] = value[0]
			}
		}
	case "plain", "", "xml", "multipart":
	}
	return ctx, nil
}

func renderTemplate(transform *Transform, ctx templateContext) (string, error) {
	text := transform.Template
	if transform.TemplateIsBase64 {
		decoded, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			return "", err
		}
		text = string(decoded)
	}

	return templateExprPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := templateExprPattern.FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}
		return resolveExpression(strings.TrimSpace(parts[1]), ctx)
	}), nil
}

func resolveExpression(expr string, ctx templateContext) string {
	if strings.HasPrefix(expr, "_escape_json(") && strings.HasSuffix(expr, ")") {
		value := resolveExpression(strings.TrimSuffix(strings.TrimPrefix(expr, "_escape_json("), ")"), ctx)
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
	if expr == "_body" {
		return ctx.body
	}
	if strings.HasPrefix(expr, "_ctx.var.") {
		return requestVar(ctx.req, strings.TrimPrefix(expr, "_ctx.var."))
	}
	if value, ok := ctx.values[expr]; ok {
		return value
	}
	return ""
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
	switch {
	case name == "uri":
		return r.URL.Path
	case name == "request_uri":
		return r.URL.RequestURI()
	case name == "method":
		return r.Method
	case name == "host":
		return r.Host
	case name == "scheme":
		if scheme := r.Header.Get("X-Forwarded-Proto"); scheme != "" {
			return scheme
		}
		if r.TLS != nil {
			return "https"
		}
		return "http"
	case name == "remote_addr":
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			return host
		}
		return r.RemoteAddr
	case strings.HasPrefix(name, "arg_"):
		return r.URL.Query().Get(strings.TrimPrefix(name, "arg_"))
	case strings.HasPrefix(name, "http_"):
		header := strings.ReplaceAll(strings.TrimPrefix(name, "http_"), "_", "-")
		return r.Header.Get(header)
	default:
		return ""
	}
}
