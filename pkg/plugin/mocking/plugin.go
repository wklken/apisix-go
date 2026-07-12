package mocking

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"math"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	// version  = "0.1"
	priority = 10900
	name     = "mocking"

	defaultContentType = "application/json;charset=utf8"
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
	Delay           int             `json:"delay"`
	ResponseStatus  int             `json:"response_status"`
	ContentType     string          `json:"content_type"`
	ResponseExample *string         `json:"response_example,omitempty"`
	ResponseSchema  *map[string]any `json:"response_schema,omitempty"`
	WithMockHeader  *bool           `json:"with_mock_header"`
	ResponseHeaders map[string]any  `json:"response_headers"`
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
	if p.config.ContentType == "" {
		p.config.ContentType = defaultContentType
	}

	if p.config.WithMockHeader == nil {
		defaultValue := true
		p.config.WithMockHeader = &defaultValue
	}

	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		// Delay response if needed
		if p.config.Delay > 0 {
			time.Sleep(time.Second * time.Duration(p.config.Delay))
		}

		// Set content type
		w.Header().Set("Content-Type", p.config.ContentType)

		// Set response headers
		for key, value := range p.config.ResponseHeaders {
			w.Header().Add(key, resolveValue(r, fmt.Sprint(value)))
		}

		// mock header
		if *p.config.WithMockHeader {
			// FIXME: change 0.0.1 to real version
			w.Header().Add("x-mock-by", "APISIX-GO/0.0.1")
		}

		w.WriteHeader(p.config.ResponseStatus)

		responseContent := ""
		if p.config.ResponseExample != nil {
			responseContent = *p.config.ResponseExample
		} else if p.config.ResponseSchema != nil {
			body, err := responseBodyFromSchema(p.config.ContentType, *p.config.ResponseSchema)
			if err != nil {
				http.Error(w, "failed to generate mocking response", http.StatusInternalServerError)
				return
			}
			responseContent = string(body)
		}
		_, _ = w.Write([]byte(resolveValue(r, responseContent)))

		// return without calling next.ServeHTTP
		return
		// next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func responseBodyFromSchema(contentType string, responseSchema map[string]any) ([]byte, error) {
	output := generateByProperty(responseSchema)
	switch parseContentType(contentType) {
	case "application/xml", "text/xml":
		return objectToXML(output, "data"), nil
	default:
		return json.Marshal(output)
	}
}

func generateByProperty(property map[string]any) any {
	switch strings.ToLower(stringValue(property["type"])) {
	case "array":
		items, ok := mapValue(property["items"])
		if !ok {
			return nil
		}
		return []any{generateByProperty(items)}
	case "object", "":
		return generateObject(property)
	case "string":
		if example, ok := property["example"].(string); ok {
			return example
		}
		return ""
	case "number":
		if example, ok := numberValue(property["example"]); ok {
			return example
		}
		return float64(0)
	case "integer":
		if example, ok := numberValue(property["example"]); ok {
			return math.Floor(example)
		}
		return float64(0)
	case "boolean":
		if example, ok := property["example"].(bool); ok {
			return example
		}
		return false
	default:
		return nil
	}
}

func generateObject(property map[string]any) map[string]any {
	output := map[string]any{}
	properties, ok := mapValue(property["properties"])
	if !ok {
		return output
	}
	for key, raw := range properties {
		child, ok := mapValue(raw)
		if !ok {
			continue
		}
		output[key] = generateByProperty(child)
	}
	return output
}

func parseContentType(contentType string) string {
	if before, _, ok := strings.Cut(contentType, ";"); ok {
		return strings.TrimSpace(before)
	}
	return strings.TrimSpace(contentType)
}

func objectToXML(value any, root string) []byte {
	var buf bytes.Buffer
	writeXMLValue(&buf, root, value)
	return buf.Bytes()
}

func writeXMLValue(buf *bytes.Buffer, name string, value any) {
	buf.WriteByte('<')
	buf.WriteString(name)
	buf.WriteByte('>')
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			writeXMLValue(buf, key, typed[key])
		}
	case []any:
		for _, item := range typed {
			writeXMLValue(buf, "item", item)
		}
	default:
		_ = xml.EscapeText(buf, fmt.Append(nil, typed))
	}
	buf.WriteString("</")
	buf.WriteString(name)
	buf.WriteByte('>')
}

func mapValue(value any) (map[string]any, bool) {
	typed, ok := value.(map[string]any)
	return typed, ok
}

func stringValue(value any) string {
	typed, _ := value.(string)
	return typed
}

func numberValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case float64:
		return typed, true
	case json.Number:
		number, err := strconv.ParseFloat(string(typed), 64)
		return number, err == nil
	default:
		return 0, false
	}
}

var variablePattern = regexp.MustCompile(`\$[A-Za-z0-9_]+`)

func resolveValue(r *http.Request, value string) string {
	return variablePattern.ReplaceAllStringFunc(value, func(variable string) string {
		return requestVar(r, strings.TrimPrefix(variable, "$"))
	})
}

func requestVar(r *http.Request, name string) string {
	switch {
	case name == "remote_addr":
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			return host
		}
		return r.RemoteAddr
	case name == "request_uri":
		return r.URL.RequestURI()
	case name == "uri":
		return r.URL.Path
	case name == "method", name == "request_method":
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
	case strings.HasPrefix(name, "arg_"):
		return r.URL.Query().Get(strings.TrimPrefix(name, "arg_"))
	case strings.HasPrefix(name, "http_"):
		header := strings.ReplaceAll(strings.TrimPrefix(name, "http_"), "_", "-")
		return r.Header.Get(header)
	default:
		return ""
	}
}
