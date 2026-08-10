package ai_prompt_guard

import (
	"bytes"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/samber/lo"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/plugin/ai_common"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config   Config
	failMode ai_common.SafetyFailMode
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
    },
    "fail_mode": {
      "type": "string",
      "enum": ["skip", "warn", "error"],
      "default": "error"
    }
  }
}
`

type Config struct {
	MatchAllRoles               bool     `json:"match_all_roles,omitempty"`
	MatchAllConversationHistory bool     `json:"match_all_conversation_history,omitempty"`
	AllowPatterns               []string `json:"allow_patterns,omitempty"`
	DenyPatterns                []string `json:"deny_patterns,omitempty"`
	FailMode                    string   `json:"fail_mode,omitempty"`

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
	failMode, err := ai_common.ParseSafetyFailMode(p.config.FailMode)
	if err != nil {
		return fmt.Errorf("invalid fail_mode: %s", p.config.FailMode)
	}
	p.failMode = failMode
	p.config.FailMode = string(failMode)

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
			base.WriteJSONMessage(w, http.StatusBadRequest, "Empty request body")
			return
		}
		if len(bytes.TrimSpace(body)) == 0 {
			base.WriteJSONMessage(w, http.StatusBadRequest, "Empty request body")
			return
		}

		var bodyTab map[string]any
		if err := json.Unmarshal(body, &bodyTab); err != nil {
			p.handleSafetyFailure(
				w,
				r,
				next,
				ai_common.SafetyInvalidPayload,
				"Request body is not valid JSON",
			)
			return
		}

		protocol, err := ai_protocols.Detect(r.URL.Path, bodyTab)
		if err != nil || protocol == ai_protocols.Passthrough {
			p.handleSafetyFailure(
				w,
				r,
				next,
				ai_common.SafetyUnknownProtocol,
				"Request format not recognized by ai-prompt-guard",
			)
			return
		}

		messages := extractMessages(protocol, bodyTab)
		if protocol != ai_protocols.OpenAIResponses && !p.config.MatchAllConversationHistory {
			messages = lastMessage(messages)
		}
		if !p.config.MatchAllRoles {
			messages = userMessages(messages)
		}
		if len(messages) == 0 {
			p.handleSafetyFailure(
				w,
				r,
				next,
				ai_common.SafetyEmptyContent,
				"No inspectable AI prompt content",
			)
			return
		}

		content := joinContent(messages)
		if strings.TrimSpace(content) == "" {
			p.handleSafetyFailure(
				w,
				r,
				next,
				ai_common.SafetyEmptyContent,
				"No inspectable AI prompt content",
			)
			return
		}
		if len(p.config.allowPatterns) > 0 && !matchesAny(p.config.allowPatterns, content) {
			metrics.RecordAISafetyOutcome(
				name,
				string(ai_common.SafetyPhaseRequest),
				string(ai_common.SafetyOutcomeDeny),
				string(ai_common.SafetyReasonAllowPatternMiss),
			)
			base.WriteJSONMessage(w, http.StatusBadRequest, "Request doesn't match allow patterns")
			return
		}
		if matchesAny(p.config.denyPatterns, content) {
			metrics.RecordAISafetyOutcome(
				name,
				string(ai_common.SafetyPhaseRequest),
				string(ai_common.SafetyOutcomeDeny),
				string(ai_common.SafetyReasonDenyPatternMatch),
			)
			base.WriteJSONMessage(w, http.StatusBadRequest, "Request contains prohibited content")
			return
		}

		metrics.RecordAISafetyOutcome(
			name,
			string(ai_common.SafetyPhaseRequest),
			string(ai_common.SafetyOutcomeAllow),
			string(ai_common.SafetyReasonClean),
		)
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) handleSafetyFailure(
	w http.ResponseWriter,
	r *http.Request,
	next http.Handler,
	class ai_common.SafetyFailureClass,
	publicMessage string,
) {
	decision := ai_common.DecideSafetyFailure(p.failMode, class)
	metrics.RecordAISafetyOutcome(
		name,
		string(ai_common.SafetyPhaseRequest),
		string(decision.Outcome),
		string(class),
	)
	if decision.Action == ai_common.SafetyReject {
		base.WriteJSONMessage(w, decision.Status, publicMessage)
		return
	}

	ai_common.LogSafetyDegradation(r, name, p.failMode, ai_common.SafetyPhaseRequest, class)
	next.ServeHTTP(w, r)
}

func extractMessages(protocol ai_protocols.Protocol, body map[string]any) []ai_protocols.Message {
	messages := ai_protocols.ExtractMessages(protocol, body)
	if protocol != ai_protocols.OpenAIResponses {
		return messages
	}

	for _, item := range responseInputItems(body["input"]) {
		content, ok := item["content"].([]any)
		if !ok {
			continue
		}
		parts := make([]string, 0, len(content))
		for _, rawPart := range content {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			if part["type"] != "input_text" && part["type"] != "text" {
				continue
			}
			if text, ok := part["text"].(string); ok && text != "" {
				parts = append(parts, text)
			}
		}
		if len(parts) == 0 {
			continue
		}
		role, _ := item["role"].(string)
		if role == "" {
			role = "user"
		}
		messages = append(messages, ai_protocols.Message{Role: role, Content: strings.Join(parts, " ")})
	}
	return messages
}

func responseInputItems(input any) []map[string]any {
	values, ok := input.([]any)
	if !ok {
		return nil
	}
	items := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if item, ok := value.(map[string]any); ok {
			items = append(items, item)
		}
	}
	return items
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

func joinContent(messages []ai_protocols.Message) string {
	return strings.Join(lo.FilterMap(messages, func(msg ai_protocols.Message, _ int) (string, bool) {
		return msg.Content, msg.Content != ""
	}), " ")
}

func matchesAny(patterns []*regexp.Regexp, content string) bool {
	for _, pattern := range patterns {
		if pattern.MatchString(content) {
			return true
		}
	}
	return false
}
