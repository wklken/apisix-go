package ai_prompt_guard

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
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
	priority = 1072
	name     = "ai-prompt-guard"
)

const schema = `
{
  "type": "object",
  "properties": {
    "match_all_roles": {
      "type": "boolean",
      "default": false
    },
    "match_all_conversation_history": {
      "type": "boolean",
      "default": false
    },
    "allow_patterns": {
      "type": "array",
      "items": {
        "type": "string"
      },
      "default": []
    },
    "deny_patterns": {
      "type": "array",
      "items": {
        "type": "string"
      },
      "default": []
    }
  }
}
`

type Config struct {
	MatchAllRoles               bool     `json:"match_all_roles,omitempty"`
	MatchAllConversationHistory bool     `json:"match_all_conversation_history,omitempty"`
	AllowPatterns               []string `json:"allow_patterns,omitempty"`
	DenyPatterns                []string `json:"deny_patterns,omitempty"`

	allowPatterns []*regexp.Regexp
	denyPatterns  []*regexp.Regexp
}

type Message struct {
	Role    string
	Content string
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
	var err error
	p.config.allowPatterns, err = compilePatterns("allow_pattern", p.config.AllowPatterns)
	if err != nil {
		return err
	}
	p.config.denyPatterns, err = compilePatterns("deny_pattern", p.config.DenyPatterns)
	if err != nil {
		return err
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody(r)
		if err != nil {
			writeJSONMessage(w, http.StatusBadRequest, "Empty request body")
			return
		}
		if len(bytes.TrimSpace(body)) == 0 {
			writeJSONMessage(w, http.StatusBadRequest, "Empty request body")
			return
		}

		var bodyTab map[string]any
		if err := json.Unmarshal(body, &bodyTab); err != nil {
			writeJSONMessage(w, http.StatusBadRequest, err.Error())
			return
		}

		messages := messagesForRequest(r, bodyTab)
		if !isOpenAIResponses(r, bodyTab) && !p.config.MatchAllConversationHistory {
			messages = lastMessage(messages)
		}
		if !p.config.MatchAllRoles {
			messages = userMessages(messages)
		}
		if len(messages) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		content := joinContent(messages)
		if len(p.config.allowPatterns) > 0 && !matchesAny(p.config.allowPatterns, content) {
			writeJSONMessage(w, http.StatusBadRequest, "Request doesn't match allow patterns")
			return
		}
		if matchesAny(p.config.denyPatterns, content) {
			writeJSONMessage(w, http.StatusBadRequest, "Request contains prohibited content")
			return
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func compilePatterns(kind string, patterns []string) ([]*regexp.Regexp, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid %s: %s", kind, pattern)
		}
		compiled = append(compiled, re)
	}
	return compiled, nil
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

func messagesForRequest(r *http.Request, body map[string]any) []Message {
	if isOpenAIResponses(r, body) {
		return responseMessages(body)
	}
	return chatMessages(body)
}

func isOpenAIResponses(r *http.Request, body map[string]any) bool {
	if _, ok := body["input"]; !ok {
		return false
	}
	return strings.HasSuffix(r.URL.Path, "/v1/responses")
}

func chatMessages(body map[string]any) []Message {
	rawMessages, ok := body["messages"].([]any)
	if !ok {
		return nil
	}

	messages := make([]Message, 0, len(rawMessages))
	for _, raw := range rawMessages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		content := contentString(msg["content"])
		if role != "" && content != "" {
			messages = append(messages, Message{Role: role, Content: content})
		}
	}
	return messages
}

func responseMessages(body map[string]any) []Message {
	messages := []Message{}
	if instructions, ok := body["instructions"].(string); ok && instructions != "" {
		messages = append(messages, Message{Role: "system", Content: instructions})
	}

	switch input := body["input"].(type) {
	case string:
		if input != "" {
			messages = append(messages, Message{Role: "user", Content: input})
		}
	case []any:
		for _, item := range input {
			switch typed := item.(type) {
			case string:
				if typed != "" {
					messages = append(messages, Message{Role: "user", Content: typed})
				}
			case map[string]any:
				role, _ := typed["role"].(string)
				if role == "" {
					role = "user"
				}
				content := contentString(firstNonNil(typed["content"], typed["text"]))
				if content != "" {
					messages = append(messages, Message{Role: role, Content: content})
				}
			}
		}
	}
	return messages
}

func contentString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, part := range typed {
			block, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := block["text"].(string); ok {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func lastMessage(messages []Message) []Message {
	if len(messages) == 0 {
		return nil
	}
	return []Message{messages[len(messages)-1]}
}

func userMessages(messages []Message) []Message {
	filtered := make([]Message, 0, len(messages))
	for _, msg := range messages {
		if msg.Role == "user" {
			filtered = append(filtered, msg)
		}
	}
	return filtered
}

func joinContent(messages []Message) string {
	parts := make([]string, 0, len(messages))
	for _, msg := range messages {
		if msg.Content != "" {
			parts = append(parts, msg.Content)
		}
	}
	return strings.Join(parts, " ")
}

func matchesAny(patterns []*regexp.Regexp, content string) bool {
	for _, pattern := range patterns {
		if pattern.MatchString(content) {
			return true
		}
	}
	return false
}

func writeJSONMessage(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"message":%q}`, message)
}
