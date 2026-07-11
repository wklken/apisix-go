package ai_prompt_template

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 1071
	name     = "ai-prompt-template"
)

const schema = `
{
  "type": "object",
  "properties": {
    "templates": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "name": {
            "type": "string",
            "minLength": 1
          },
          "template": {
            "type": "object",
            "properties": {
              "model": {
                "type": "string",
                "minLength": 1
              },
              "messages": {
                "type": "array",
                "minItems": 1,
                "items": {
                  "$ref": "#/$defs/prompt"
                }
              }
            }
          }
        },
        "required": ["name", "template"]
      }
    }
  },
  "required": ["templates"],
  "$defs": {
    "prompt": {
      "type": "object",
      "properties": {
        "role": {
          "type": "string",
          "enum": ["system", "user", "assistant"]
        },
        "content": {
          "type": "string",
          "minLength": 1
        }
      },
      "required": ["role", "content"]
    }
  }
}
`

type Config struct {
	Templates []NamedTemplate `json:"templates"`
}

type NamedTemplate struct {
	Name     string   `json:"name"`
	Template Template `json:"template"`
}

type Template struct {
	Model    string    `json:"model,omitempty"`
	Messages []Message `json:"messages,omitempty"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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
		body, err := readBody(r)
		if err != nil {
			writeJSONMessage(w, http.StatusBadRequest, "could not get body: "+err.Error())
			return
		}
		if len(bytes.TrimSpace(body)) == 0 {
			writeJSONMessage(w, http.StatusBadRequest, "could not get body: request body is empty")
			return
		}

		var bodyTab map[string]any
		if err := json.Unmarshal(body, &bodyTab); err != nil {
			writeJSONMessage(w, http.StatusBadRequest, "could not parse JSON request body: "+err.Error())
			return
		}

		templateName, _ := bodyTab["template_name"].(string)
		if templateName == "" {
			writeJSONMessage(w, http.StatusBadRequest, "template name is missing in request.")
			return
		}

		template, ok := p.findTemplate(templateName)
		if !ok {
			writeJSONMessage(w, http.StatusBadRequest, "template: "+templateName+" not configured.")
			return
		}

		rendered := renderTemplate(template, bodyTab)
		rewritten, err := json.Marshal(rendered)
		if err != nil {
			writeJSONMessage(
				w,
				http.StatusInternalServerError,
				"failed to parse modified JSON request body: "+err.Error(),
			)
			return
		}

		r.Body = io.NopCloser(bytes.NewReader(rewritten))
		r.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(rewritten)), nil
		}
		r.ContentLength = int64(len(rewritten))
		r.Header.Set("Content-Length", fmt.Sprint(len(rewritten)))

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) findTemplate(name string) (Template, bool) {
	for _, template := range p.config.Templates {
		if template.Name == name {
			return template.Template, true
		}
	}
	return Template{}, false
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

func renderTemplate(template Template, values map[string]any) Template {
	rendered := Template{
		Model:    renderString(template.Model, values),
		Messages: make([]Message, 0, len(template.Messages)),
	}
	for _, msg := range template.Messages {
		rendered.Messages = append(rendered.Messages, Message{
			Role:    msg.Role,
			Content: renderString(msg.Content, values),
		})
	}
	return rendered
}

func renderString(text string, values map[string]any) string {
	return templateExprPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := templateExprPattern.FindStringSubmatch(match)
		if len(parts) != 2 {
			return match
		}
		key := strings.TrimSpace(parts[1])
		if value, ok := lookupValue(values, key); ok {
			return fmt.Sprint(value)
		}
		return ""
	})
}

func lookupValue(values map[string]any, expression string) (any, bool) {
	var current any = values
	for _, segment := range strings.Split(expression, ".") {
		key, indexes, ok := parseSegment(segment)
		if !ok {
			return nil, false
		}
		if key != "" {
			object, ok := current.(map[string]any)
			if !ok {
				return nil, false
			}
			current, ok = object[key]
			if !ok {
				return nil, false
			}
		}
		for _, index := range indexes {
			array, ok := current.([]any)
			if !ok || index >= len(array) {
				return nil, false
			}
			current = array[index]
		}
	}
	return current, true
}

func parseSegment(segment string) (string, []int, bool) {
	if segment == "" {
		return "", nil, false
	}
	keyEnd := strings.IndexByte(segment, '[')
	if keyEnd == -1 {
		return segment, nil, true
	}
	key := segment[:keyEnd]
	indexes := make([]int, 0)
	for rest := segment[keyEnd:]; rest != ""; {
		if rest[0] != '[' {
			return "", nil, false
		}
		end := strings.IndexByte(rest, ']')
		if end <= 1 {
			return "", nil, false
		}
		index, err := strconv.Atoi(rest[1:end])
		if err != nil || index < 0 {
			return "", nil, false
		}
		indexes = append(indexes, index)
		rest = rest[end+1:]
	}
	return key, indexes, true
}

func writeJSONMessage(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"message":%q}`, message)
}
