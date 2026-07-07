package ai_prompt_decorator

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 1070
	name     = "ai-prompt-decorator"
)

const schema = `
{
  "type": "object",
  "properties": {
    "prepend": {
      "type": "array",
      "items": {
        "$ref": "#/$defs/prompt"
      }
    },
    "append": {
      "type": "array",
      "items": {
        "$ref": "#/$defs/prompt"
      }
    }
  },
  "anyOf": [
    {
      "required": ["prepend"]
    },
    {
      "required": ["append"]
    },
    {
      "required": ["append", "prepend"]
    }
  ],
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
	Prepend []Message `json:"prepend,omitempty"`
	Append  []Message `json:"append,omitempty"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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

		if isOpenAIResponses(r, bodyTab) {
			decorateResponses(bodyTab, p.config.Prepend, p.config.Append)
		} else {
			decorateChat(bodyTab, p.config.Prepend, p.config.Append)
		}

		rewritten, err := json.Marshal(bodyTab)
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

func decorateChat(body map[string]any, prepend []Message, appendMessages []Message) {
	original := asMessages(body["messages"])
	messages := make([]any, 0, len(prepend)+len(original)+len(appendMessages))
	for _, msg := range prepend {
		messages = append(messages, messageMap(msg))
	}
	messages = append(messages, original...)
	for _, msg := range appendMessages {
		messages = append(messages, messageMap(msg))
	}
	body["messages"] = messages
}

func decorateResponses(body map[string]any, prepend []Message, appendMessages []Message) {
	if len(prepend) > 0 {
		prependText := joinMessageContent(prepend)
		if existing, ok := body["instructions"].(string); ok && existing != "" {
			body["instructions"] = prependText + "\n" + existing
		} else {
			body["instructions"] = prependText
		}
	}

	if len(appendMessages) == 0 {
		return
	}
	appendText := joinMessageContent(appendMessages)
	switch input := body["input"].(type) {
	case string:
		body["input"] = input + "\n" + appendText
	case []any:
		body["input"] = append(input, map[string]any{
			"type":    "message",
			"role":    "user",
			"content": appendText,
		})
	default:
		body["input"] = appendText
	}
}

func isOpenAIResponses(r *http.Request, body map[string]any) bool {
	if _, ok := body["input"]; !ok {
		return false
	}
	return strings.HasSuffix(r.URL.Path, "/v1/responses")
}

func asMessages(value any) []any {
	messages, ok := value.([]any)
	if !ok {
		return nil
	}
	return messages
}

func messageMap(msg Message) map[string]any {
	return map[string]any{
		"role":    msg.Role,
		"content": msg.Content,
	}
}

func joinMessageContent(messages []Message) string {
	parts := make([]string, 0, len(messages))
	for _, msg := range messages {
		parts = append(parts, msg.Content)
	}
	return strings.Join(parts, "\n")
}

func writeJSONMessage(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"message":%q}`, message)
}
