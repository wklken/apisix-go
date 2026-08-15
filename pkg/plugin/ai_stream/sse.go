package ai_stream

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
)

type Usage struct {
	Model            string
	Text             string
	Raw              map[string]any
	PromptTokens     int64
	CompletionTokens int64
	ToolCalls        int64
	HasToolCalls     bool

	textBuilder strings.Builder
}

// AppendText accumulates chunk text into Text without re-copying the
// accumulated prefix on every chunk.
func (u *Usage) AppendText(text string) {
	if text == "" {
		return
	}
	if u.textBuilder.Len() == 0 && u.Text != "" {
		u.textBuilder.WriteString(u.Text)
	}
	u.textBuilder.WriteString(text)
	u.Text = u.textBuilder.String()
}

func ForwardSSE(
	w http.ResponseWriter,
	body io.Reader,
	protocol ai_protocols.Protocol,
	maxBytes int64,
) (Usage, error) {
	usage := Usage{Raw: make(map[string]any), PromptTokens: -1, CompletionTokens: -1}
	reader := bufio.NewReader(body)
	var total int64
	var event strings.Builder
	writeEvent := func() error {
		if event.Len() == 0 {
			return nil
		}
		if _, err := io.WriteString(w, event.String()); err != nil {
			return fmt.Errorf("%w: %v", ErrClientDisconnected, err)
		}
		event.Reset()
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return nil
	}
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			total += int64(len(line))
			if maxBytes > 0 && total > maxBytes {
				return usage, fmt.Errorf("max_response_bytes exceeded")
			}
			if mergeErr := mergeSSEUsage(&usage, protocol, line); mergeErr != nil {
				return usage, mergeErr
			}
			event.WriteString(line)
			if strings.TrimRight(line, "\r\n") == "" {
				if writeErr := writeEvent(); writeErr != nil {
					return usage, writeErr
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				if writeErr := writeEvent(); writeErr != nil {
					return usage, writeErr
				}
				break
			}
			return usage, err
		}
	}
	if usage.PromptTokens < 0 {
		usage.PromptTokens = ai_protocols.NumericUsage(usage.Raw["prompt_tokens"], false)
	}
	if usage.CompletionTokens < 0 {
		usage.CompletionTokens = ai_protocols.NumericUsage(usage.Raw["completion_tokens"], false)
	}
	return usage, nil
}

func mergeSSEUsage(usage *Usage, protocol ai_protocols.Protocol, line string) error {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return nil
	}
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "" || data == "[DONE]" {
		return nil
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return fmt.Errorf("invalid SSE data: %w", err)
	}
	if model, ok := event["model"].(string); ok {
		usage.Model = model
	}
	usage.AppendText(ai_protocols.ExtractStreamEventText(protocol, event))
	switch protocol {
	case ai_protocols.OpenAIResponses:
		response, _ := event["response"].(map[string]any)
		if model, ok := response["model"].(string); ok {
			usage.Model = model
		}
		if output, ok := response["output"].([]any); ok {
			for _, rawItem := range output {
				item, _ := rawItem.(map[string]any)
				switch item["type"] {
				case "function_call":
					usage.HasToolCalls = true
				case "message":
					if content, ok := item["content"].([]any); ok {
						for _, rawPart := range content {
							part, _ := rawPart.(map[string]any)
							if part["type"] == "function_call" {
								usage.HasToolCalls = true
							}
						}
					}
				}
			}
		}
		mergeOpenAIUsage(usage, response["usage"], true)
	case ai_protocols.AnthropicMessages:
		if message, ok := event["message"].(map[string]any); ok {
			if model, ok := message["model"].(string); ok {
				usage.Model = model
			}
			mergeAnthropicUsage(usage, message["usage"])
		}
		mergeAnthropicUsage(usage, event["usage"])
		if contentBlocks, ok := event["content_block"].(map[string]any); ok &&
			contentBlocks["type"] == "tool_use" {
			usage.HasToolCalls = true
		}
	default:
		if choices, ok := event["choices"].([]any); ok {
			for _, rawChoice := range choices {
				choice, _ := rawChoice.(map[string]any)
				delta, _ := choice["delta"].(map[string]any)
				if toolCalls, ok := delta["tool_calls"].([]any); ok && len(toolCalls) > 0 {
					usage.HasToolCalls = true
				}
			}
		}
		mergeOpenAIUsage(usage, event["usage"], false)
	}
	return nil
}

func mergeOpenAIUsage(usage *Usage, value any, responses bool) {
	raw, ok := value.(map[string]any)
	if !ok {
		return
	}
	mergeRaw(usage.Raw, raw)
	for _, key := range []string{"prompt_tokens_details", "completion_tokens_details", "input_tokens_details", "output_tokens_details"} {
		if details, ok := raw[key].(map[string]any); ok {
			usage.Raw[key] = details
		}
	}
	if responses {
		usage.PromptTokens = ai_protocols.NumericUsage(raw["input_tokens"], false)
		usage.CompletionTokens = ai_protocols.NumericUsage(raw["output_tokens"], false)
		return
	}
	usage.PromptTokens = ai_protocols.NumericUsage(raw["prompt_tokens"], false)
	usage.CompletionTokens = ai_protocols.NumericUsage(raw["completion_tokens"], false)
}

func mergeAnthropicUsage(usage *Usage, value any) {
	raw, ok := value.(map[string]any)
	if !ok {
		return
	}
	mergeRaw(usage.Raw, raw)
	if value := ai_protocols.NumericUsage(raw["input_tokens"], false); value >= 0 {
		usage.PromptTokens = value
	}
	if value := ai_protocols.NumericUsage(raw["output_tokens"], false); value >= 0 {
		usage.CompletionTokens = value
	}
}

func mergeRaw(dst map[string]any, src map[string]any) {
	for key, value := range src {
		if ai_protocols.NumericUsage(value, false) >= 0 {
			dst[key] = value
		}
	}
}
