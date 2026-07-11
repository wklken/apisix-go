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
	Raw              map[string]any
	PromptTokens     int64
	CompletionTokens int64
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
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			total += int64(len(line))
			if maxBytes > 0 && total > maxBytes {
				return usage, fmt.Errorf("max_response_bytes exceeded")
			}
			mergeSSEUsage(&usage, protocol, line)
			if _, writeErr := io.WriteString(w, line); writeErr != nil {
				return usage, writeErr
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return usage, err
		}
	}
	if usage.PromptTokens < 0 {
		usage.PromptTokens = numericUsage(usage.Raw["prompt_tokens"])
	}
	if usage.CompletionTokens < 0 {
		usage.CompletionTokens = numericUsage(usage.Raw["completion_tokens"])
	}
	return usage, nil
}

func mergeSSEUsage(usage *Usage, protocol ai_protocols.Protocol, line string) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return
	}
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "" || data == "[DONE]" {
		return
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return
	}
	if model, ok := event["model"].(string); ok {
		usage.Model = model
	}
	switch protocol {
	case ai_protocols.OpenAIResponses:
		response, _ := event["response"].(map[string]any)
		if model, ok := response["model"].(string); ok {
			usage.Model = model
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
	default:
		mergeOpenAIUsage(usage, event["usage"], false)
	}
}

func mergeOpenAIUsage(usage *Usage, value any, responses bool) {
	raw, ok := value.(map[string]any)
	if !ok {
		return
	}
	mergeRaw(usage.Raw, raw)
	if responses {
		usage.PromptTokens = numericUsage(raw["input_tokens"])
		usage.CompletionTokens = numericUsage(raw["output_tokens"])
		return
	}
	usage.PromptTokens = numericUsage(raw["prompt_tokens"])
	usage.CompletionTokens = numericUsage(raw["completion_tokens"])
}

func mergeAnthropicUsage(usage *Usage, value any) {
	raw, ok := value.(map[string]any)
	if !ok {
		return
	}
	mergeRaw(usage.Raw, raw)
	if value := numericUsage(raw["input_tokens"]); value >= 0 {
		usage.PromptTokens = value
	}
	if value := numericUsage(raw["output_tokens"]); value >= 0 {
		usage.CompletionTokens = value
	}
}

func mergeRaw(dst map[string]any, src map[string]any) {
	for key, value := range src {
		if numericUsage(value) >= 0 {
			dst[key] = value
		}
	}
}

func numericUsage(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	default:
		return -1
	}
}
