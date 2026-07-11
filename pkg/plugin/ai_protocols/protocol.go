package ai_protocols

import (
	"fmt"
	"math"
	"strings"

	"github.com/gofrs/uuid"
	"github.com/wklken/apisix-go/pkg/json"
)

type Protocol struct {
	OverrideKey string
	RequestType string
	Endpoint    string
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

var (
	OpenAIChat = Protocol{
		OverrideKey: "openai-chat",
		RequestType: "ai_chat",
		Endpoint:    "/v1/chat/completions",
	}
	OpenAIResponses = Protocol{
		OverrideKey: "openai-responses",
		RequestType: "ai_responses",
		Endpoint:    "/v1/responses",
	}
	OpenAIEmbeddings = Protocol{
		OverrideKey: "openai-embeddings",
		RequestType: "ai_embeddings",
		Endpoint:    "/v1/embeddings",
	}
	AnthropicMessages = Protocol{
		OverrideKey: "anthropic-messages",
		RequestType: "ai_chat",
		Endpoint:    "/v1/messages",
	}
	BedrockConverse = Protocol{
		OverrideKey: "bedrock-converse",
		RequestType: "ai_chat",
		Endpoint:    "/converse",
	}
	Passthrough = Protocol{
		OverrideKey: "passthrough",
		RequestType: "ai_chat",
	}
)

type ResponseMetadata struct {
	Model            string
	PromptTokens     int64
	CompletionTokens int64
}

func Detect(requestPath string, body map[string]any) (Protocol, error) {
	if isNonEmptyObject(body) && strings.HasSuffix(requestPath, BedrockConverse.Endpoint) && hasMessages(body) {
		return BedrockConverse, nil
	}
	if body != nil && strings.HasSuffix(requestPath, AnthropicMessages.Endpoint) {
		return AnthropicMessages, nil
	}
	if strings.HasSuffix(requestPath, OpenAIResponses.Endpoint) && hasInput(body) {
		return OpenAIResponses, nil
	}
	if hasMessages(body) {
		return OpenAIChat, nil
	}
	if hasInput(body) {
		return OpenAIEmbeddings, nil
	}
	if isNonEmptyObject(body) {
		return Passthrough, nil
	}
	return Protocol{}, fmt.Errorf("unsupported AI request protocol: expected a non-empty JSON object")
}

func ExtractMessages(protocol Protocol, body map[string]any) []Message {
	switch protocol {
	case OpenAIResponses:
		return responseMessages(body)
	case OpenAIEmbeddings:
		switch input := body["input"].(type) {
		case string:
			return []Message{{Role: "user", Content: input}}
		case []any:
			messages := make([]Message, 0, len(input))
			for _, value := range input {
				if text, ok := value.(string); ok {
					messages = append(messages, Message{Role: "user", Content: text})
				}
			}
			return messages
		}
	case AnthropicMessages:
		return anthropicMessages(body)
	case BedrockConverse:
		return bedrockMessages(body)
	case OpenAIChat:
		return chatMessages(body)
	}
	return []Message{}
}

func ExtractRequestContent(protocol Protocol, body map[string]any) []string {
	messages := ExtractMessages(protocol, body)
	contents := make([]string, 0, len(messages))
	for _, message := range messages {
		if strings.TrimSpace(message.Content) != "" {
			contents = append(contents, message.Content)
		}
	}
	return contents
}

func PrependMessages(protocol Protocol, body map[string]any, messages []Message) {
	if len(messages) == 0 {
		return
	}

	switch protocol {
	case OpenAIResponses:
		prependText := joinMessageContent(messages)
		if instructions, ok := body["instructions"].(string); ok {
			body["instructions"] = prependText + "\n" + instructions
		} else {
			body["instructions"] = prependText
		}
	case AnthropicMessages:
		body["messages"] = prependPlainMessages(asMessages(body["messages"]), messages)
	case BedrockConverse:
		prependBedrockMessages(body, messages)
	case OpenAIChat:
		body["messages"] = prependPlainMessages(asMessages(body["messages"]), messages)
	}
}

func AppendMessages(protocol Protocol, body map[string]any, messages []Message) {
	if len(messages) == 0 {
		return
	}

	switch protocol {
	case OpenAIResponses:
		appendText := joinMessageContent(messages)
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
	case AnthropicMessages:
		body["messages"] = appendPlainMessages(asMessages(body["messages"]), messages)
	case BedrockConverse:
		appendBedrockMessages(body, messages)
	case OpenAIChat:
		body["messages"] = appendPlainMessages(asMessages(body["messages"]), messages)
	}
}

func BuildDenyResponse(protocol Protocol, model, text string) map[string]any {
	switch protocol {
	case OpenAIResponses:
		return map[string]any{
			"id":     responseID(),
			"object": "response",
			"status": "completed",
			"model":  model,
			"output": []any{map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []any{map[string]any{
					"type": "output_text",
					"text": text,
				}},
			}},
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
		}
	case OpenAIEmbeddings:
		return map[string]any{"error": map[string]any{
			"message": text,
			"type":    "content_policy_violation",
		}}
	case AnthropicMessages:
		return map[string]any{
			"id":          responseID(),
			"type":        "message",
			"role":        "assistant",
			"model":       model,
			"content":     []any{map[string]any{"type": "text", "text": text}},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 0, "output_tokens": 0},
		}
	case BedrockConverse:
		return map[string]any{
			"output": map[string]any{"message": map[string]any{
				"role":    "assistant",
				"content": []any{map[string]any{"text": text}},
			}},
			"stopReason": "end_turn",
			"usage":      map[string]any{"inputTokens": 0, "outputTokens": 0, "totalTokens": 0},
		}
	case Passthrough:
		return map[string]any{"message": text}
	default:
		return map[string]any{
			"id":     responseID(),
			"object": "chat.completion",
			"model":  model,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": text},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
		}
	}
}

func BuildDenyWireResponse(protocol Protocol, model, denyText string, streaming bool) ([]byte, string, error) {
	if !streaming || protocol == OpenAIEmbeddings || protocol == BedrockConverse || protocol == Passthrough {
		encoded, err := json.Marshal(BuildDenyResponse(protocol, model, denyText))
		return encoded, "application/json", err
	}
	var wire string
	switch protocol {
	case OpenAIResponses:
		response := BuildDenyResponse(protocol, model, denyText)
		delta, err := json.Marshal(map[string]any{
			"type": "response.output_text.delta", "delta": denyText,
		})
		if err != nil {
			return nil, "", err
		}
		completed, err := json.Marshal(map[string]any{"type": "response.completed", "response": response})
		if err != nil {
			return nil, "", err
		}
		wire = "event: response.output_text.delta\ndata: " + string(delta) + "\n\n" +
			"event: response.completed\ndata: " + string(completed) + "\n\n"
	case AnthropicMessages:
		id := responseID()
		events := []struct {
			name string
			data map[string]any
		}{
			{"message_start", map[string]any{
				"type": "message_start", "message": map[string]any{
					"id": id, "type": "message", "role": "assistant", "model": model, "content": []any{},
					"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
				},
			}},
			{"content_block_start", map[string]any{
				"type": "content_block_start", "index": 0,
				"content_block": map[string]any{"type": "text", "text": ""},
			}},
			{"content_block_delta", map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": denyText},
			}},
			{"content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}},
			{"message_delta", map[string]any{
				"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"},
				"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
			}},
			{"message_stop", map[string]any{"type": "message_stop"}},
		}
		var builder strings.Builder
		for _, event := range events {
			encoded, err := json.Marshal(event.data)
			if err != nil {
				return nil, "", err
			}
			fmt.Fprintf(&builder, "event: %s\ndata: %s\n\n", event.name, encoded)
		}
		wire = builder.String()
	default:
		chunk := map[string]any{
			"id": responseID(), "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"content": denyText}, "finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
		}
		encoded, err := json.Marshal(chunk)
		if err != nil {
			return nil, "", err
		}
		wire = "data: " + string(encoded) + "\n\ndata: [DONE]\n\n"
	}
	return []byte(wire), "text/event-stream", nil
}

func BuildSimpleRequest(protocol Protocol, systemPrompt, userContent string, options map[string]any) map[string]any {
	switch protocol {
	case OpenAIResponses:
		body := map[string]any{"input": userContent, "stream": false}
		if systemPrompt != "" {
			body["instructions"] = systemPrompt
		}
		copySimpleOptions(body, options)
		return body
	case OpenAIEmbeddings:
		body := map[string]any{"input": userContent}
		copySimpleOptions(body, options)
		return body
	case AnthropicMessages:
		body := map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": userContent}},
			"stream":   false,
		}
		if systemPrompt != "" {
			body["system"] = systemPrompt
		}
		if _, ok := options["max_tokens"]; !ok {
			body["max_tokens"] = 4096
		}
		copySimpleOptions(body, options)
		return body
	case BedrockConverse:
		body := map[string]any{
			"messages": []any{map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"text": userContent}},
			}},
		}
		if systemPrompt != "" {
			body["system"] = []any{map[string]any{"text": systemPrompt}}
		}
		if maxTokens, ok := options["max_tokens"]; ok {
			body["inferenceConfig"] = map[string]any{"maxTokens": maxTokens}
		}
		for key, value := range options {
			if key != "model" && key != "max_tokens" {
				body[key] = value
			}
		}
		return body
	default:
		body := map[string]any{
			"messages": []any{
				map[string]any{"role": "system", "content": systemPrompt},
				map[string]any{"role": "user", "content": userContent},
			},
			"stream": false,
		}
		copySimpleOptions(body, options)
		return body
	}
}

func ExtractResponseText(protocol Protocol, body map[string]any) string {
	switch protocol {
	case OpenAIResponses:
		parts := make([]string, 0)
		for _, rawOutput := range asMessages(body["output"]) {
			output, ok := rawOutput.(map[string]any)
			if !ok {
				continue
			}
			for _, rawContent := range asMessages(output["content"]) {
				content, ok := rawContent.(map[string]any)
				if ok && content["type"] == "output_text" {
					if text, ok := content["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
		}
		return strings.Join(parts, " ")
	case AnthropicMessages:
		return textContent(body["content"], true)
	case BedrockConverse:
		output, _ := body["output"].(map[string]any)
		message, _ := output["message"].(map[string]any)
		return textContent(message["content"], false)
	case OpenAIEmbeddings, Passthrough:
		return ""
	default:
		parts := make([]string, 0)
		for _, rawChoice := range asMessages(body["choices"]) {
			choice, ok := rawChoice.(map[string]any)
			if !ok {
				continue
			}
			message, _ := choice["message"].(map[string]any)
			if content, ok := message["content"].(string); ok {
				parts = append(parts, content)
			}
		}
		return strings.Join(parts, " ")
	}
}

func IsStreaming(protocol Protocol, body map[string]any) bool {
	if protocol == OpenAIEmbeddings {
		return false
	}
	streaming, _ := body["stream"].(bool)
	return streaming
}

func copySimpleOptions(body map[string]any, options map[string]any) {
	for key, value := range options {
		body[key] = value
	}
}

func ExtractResponseMetadata(protocol Protocol, body []byte) ResponseMetadata {
	metadata := ResponseMetadata{PromptTokens: -1, CompletionTokens: -1}
	var decoded struct {
		Model string         `json:"model"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return metadata
	}

	metadata.Model = decoded.Model
	switch protocol {
	case OpenAIResponses:
		metadata.PromptTokens = numericUsage(decoded.Usage["input_tokens"])
		metadata.CompletionTokens = numericUsage(decoded.Usage["output_tokens"])
	case OpenAIEmbeddings:
		metadata.PromptTokens = numericUsage(decoded.Usage["prompt_tokens"])
		metadata.CompletionTokens = 0
	case AnthropicMessages:
		metadata.PromptTokens = numericUsage(decoded.Usage["input_tokens"])
		metadata.CompletionTokens = numericUsage(decoded.Usage["output_tokens"])
	case BedrockConverse:
		metadata.PromptTokens = numericUsage(decoded.Usage["inputTokens"])
		metadata.CompletionTokens = numericUsage(decoded.Usage["outputTokens"])
	default:
		metadata.PromptTokens = numericUsage(decoded.Usage["prompt_tokens"])
		metadata.CompletionTokens = numericUsage(decoded.Usage["completion_tokens"])
	}
	return metadata
}

func chatMessages(body map[string]any) []Message {
	messages := make([]Message, 0)
	for _, raw := range asMessages(body["messages"]) {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if content := textContent(message["content"], true); content != "" {
			messages = append(messages, Message{Role: stringValue(message["role"]), Content: content})
		}
	}
	return messages
}

func responseMessages(body map[string]any) []Message {
	messages := make([]Message, 0)
	if instructions, ok := body["instructions"].(string); ok {
		messages = append(messages, Message{Role: "system", Content: instructions})
	}
	switch input := body["input"].(type) {
	case string:
		messages = append(messages, Message{Role: "user", Content: input})
	case []any:
		for _, item := range input {
			switch typed := item.(type) {
			case string:
				messages = append(messages, Message{Role: "user", Content: typed})
			case map[string]any:
				role := stringValue(typed["role"])
				if role == "" {
					role = "user"
				}
				if content := stringValue(firstNonNil(typed["content"], typed["text"])); content != "" {
					messages = append(messages, Message{Role: role, Content: content})
				}
			}
		}
	}
	return messages
}

func anthropicMessages(body map[string]any) []Message {
	messages := make([]Message, 0)
	if system, ok := body["system"].(string); ok {
		messages = append(messages, Message{Role: "system", Content: system})
	}
	for _, raw := range asMessages(body["messages"]) {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if content := textContent(message["content"], true); content != "" {
			messages = append(messages, Message{Role: stringValue(message["role"]), Content: content})
		}
	}
	return messages
}

func bedrockMessages(body map[string]any) []Message {
	messages := make([]Message, 0)
	if system := textContent(body["system"], false); system != "" {
		messages = append(messages, Message{Role: "system", Content: system})
	}
	for _, raw := range asMessages(body["messages"]) {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if content := textContent(message["content"], false); content != "" {
			messages = append(messages, Message{Role: stringValue(message["role"]), Content: content})
		}
	}
	return messages
}

func prependPlainMessages(existing []any, messages []Message) []any {
	updated := make([]any, 0, len(messages)+len(existing))
	for _, message := range messages {
		updated = append(updated, plainMessage(message))
	}
	return append(updated, existing...)
}

func appendPlainMessages(existing []any, messages []Message) []any {
	updated := make([]any, 0, len(existing)+len(messages))
	updated = append(updated, existing...)
	for _, message := range messages {
		updated = append(updated, plainMessage(message))
	}
	return updated
}

func prependBedrockMessages(body map[string]any, messages []Message) {
	system := make([]any, 0)
	chat := make([]any, 0)
	for _, message := range messages {
		if message.Role == "system" {
			system = append(system, map[string]any{"text": message.Content})
			continue
		}
		chat = append(chat, bedrockMessage(message))
	}
	if len(system) > 0 {
		body["system"] = append(system, asMessages(body["system"])...)
	}
	if len(chat) > 0 {
		body["messages"] = append(chat, asMessages(body["messages"])...)
	}
}

func appendBedrockMessages(body map[string]any, messages []Message) {
	for _, message := range messages {
		if message.Role == "system" {
			body["system"] = append(asMessages(body["system"]), map[string]any{"text": message.Content})
			continue
		}
		body["messages"] = append(asMessages(body["messages"]), bedrockMessage(message))
	}
}

func hasMessages(body map[string]any) bool {
	_, ok := body["messages"].([]any)
	return ok
}

func hasInput(body map[string]any) bool {
	input, ok := body["input"]
	return ok && input != nil
}

func isNonEmptyObject(body map[string]any) bool {
	return len(body) > 0
}

func asMessages(value any) []any {
	messages, _ := value.([]any)
	return messages
}

func textContent(value any, requireTextType bool) string {
	if content, ok := value.(string); ok {
		return content
	}
	parts := make([]string, 0)
	for _, raw := range asMessages(value) {
		block, ok := raw.(map[string]any)
		if !ok || (requireTextType && block["type"] != "text") {
			continue
		}
		if text, ok := block["text"].(string); ok {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, " ")
}

func plainMessage(message Message) map[string]any {
	return map[string]any{"role": message.Role, "content": message.Content}
}

func bedrockMessage(message Message) map[string]any {
	return map[string]any{"role": message.Role, "content": []any{map[string]any{"text": message.Content}}}
}

func joinMessageContent(messages []Message) string {
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		parts = append(parts, message.Content)
	}
	return strings.Join(parts, "\n")
}

func stringValue(value any) string {
	result, _ := value.(string)
	return result
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func responseID() string {
	value, err := uuid.NewV4()
	if err != nil {
		return ""
	}
	return value.String()
}

func numericUsage(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(math.Round(typed))
	case int64:
		return typed
	case int:
		return int64(typed)
	default:
		return -1
	}
}
