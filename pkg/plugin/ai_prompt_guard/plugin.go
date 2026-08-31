package ai_prompt_guard

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/samber/lo"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
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
		body, err := base.ReadRequestBody(r)
		if err != nil {
			writeAPISIXMessage(w, http.StatusBadRequest, "Empty request body")
			return
		}
		if len(body) == 0 {
			writeAPISIXMessage(w, http.StatusBadRequest, "Empty request body")
			return
		}

		var bodyTab map[string]any
		if err := json.Unmarshal(body, &bodyTab); err != nil {
			writeAPISIXMessage(w, http.StatusBadRequest, err.Error())
			return
		}

		protocol, err := ai_protocols.Detect(r.URL.Path, bodyTab)
		if err != nil || protocol == ai_protocols.Passthrough {
			writeEmptyResponse(w)
			return
		}

		messages := ai_protocols.ExtractMessages(protocol, bodyTab)
		if protocol == ai_protocols.OpenAIChat {
			if chatMessages, ok := chatMessagesPreservingEmptyContent(bodyTab); ok {
				messages = chatMessages
			}
		}
		if protocol != ai_protocols.OpenAIResponses && !p.config.MatchAllConversationHistory {
			messages = lastMessage(messages)
		}
		if !p.config.MatchAllRoles {
			messages = userMessages(messages)
		}
		if len(messages) == 0 {
			writeEmptyResponse(w)
			return
		}
		content := joinContent(messages)
		if len(p.config.allowPatterns) > 0 && !matchesAny(p.config.allowPatterns, content) {
			writeAPISIXMessage(w, http.StatusBadRequest, "Request doesn't match allow patterns")
			return
		}
		if matchesAny(p.config.denyPatterns, content) {
			writeAPISIXMessage(w, http.StatusBadRequest, "Request contains prohibited content")
			return
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func writeAPISIXMessage(w http.ResponseWriter, status int, message string) {
	body, _ := json.Marshal(map[string]string{"message": message})
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
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

func lastMessage(messages []ai_protocols.Message) []ai_protocols.Message {
	if len(messages) == 0 {
		return nil
	}
	return []ai_protocols.Message{messages[len(messages)-1]}
}

func userMessages(messages []ai_protocols.Message) []ai_protocols.Message {
	return lo.Filter(messages, func(msg ai_protocols.Message, _ int) bool {
		return msg.Role == "user"
	})
}

func chatMessagesPreservingEmptyContent(body map[string]any) ([]ai_protocols.Message, bool) {
	rawMessages, ok := body["messages"].([]any)
	if !ok {
		return nil, false
	}

	messages := make([]ai_protocols.Message, 0, len(rawMessages))
	for _, rawMessage := range rawMessages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		if content, ok := message["content"].(string); ok {
			role, _ := message["role"].(string)
			messages = append(messages, ai_protocols.Message{Role: role, Content: content})
			continue
		}
		messages = append(messages, ai_protocols.ExtractMessages(
			ai_protocols.OpenAIChat,
			map[string]any{"messages": []any{message}},
		)...)
	}
	return messages, true
}

func joinContent(messages []ai_protocols.Message) string {
	return strings.Join(lo.Map(messages, func(msg ai_protocols.Message, _ int) string {
		return msg.Content
	}), " ")
}

func writeEmptyResponse(w http.ResponseWriter) {
	w.WriteHeader(http.StatusOK)
}

func matchesAny(patterns []*regexp.Regexp, content string) bool {
	for _, pattern := range patterns {
		if pattern.MatchString(content) {
			return true
		}
	}
	return false
}
