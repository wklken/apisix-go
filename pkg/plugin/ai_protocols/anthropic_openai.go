package ai_protocols

import (
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/wklken/apisix-go/pkg/json"
)

const openAIToolNameMaxLength = 64

var invalidOpenAIToolName = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func ConvertAnthropicHeadersToOpenAI(headers http.Header) {
	if apiKey := headers.Get("X-Api-Key"); apiKey != "" && headers.Get("Authorization") == "" {
		headers.Set("Authorization", "Bearer "+apiKey)
	}
	headers.Del("X-Api-Key")
	for name := range headers {
		lowerName := strings.ToLower(name)
		if strings.HasPrefix(lowerName, "anthropic-") || strings.HasPrefix(lowerName, "x-stainless-") {
			headers.Del(name)
		}
	}
}

func ConvertAnthropicMessagesToOpenAI(body []byte) ([]byte, map[string]string, error) {
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, nil, fmt.Errorf("decode Anthropic request: %w", err)
	}
	rawMessages, ok := request["messages"].([]any)
	if !ok || len(rawMessages) == 0 {
		return nil, nil, fmt.Errorf("missing messages")
	}

	converted := make(map[string]any)
	copyStringField(converted, request, "model")
	copyField(converted, request, "stream")
	if streaming, _ := request["stream"].(bool); streaming {
		converted["stream_options"] = map[string]any{"include_usage": true}
	}
	if maxTokens, ok := request["max_tokens"]; ok {
		converted["max_completion_tokens"] = maxTokens
	}
	copyField(converted, request, "temperature")
	copyField(converted, request, "top_p")
	if stop, ok := request["stop_sequences"].([]any); ok {
		converted["stop"] = stop
	}
	convertAnthropicThinking(converted, request["thinking"])
	convertAnthropicToolChoice(converted, request["tool_choice"])
	convertAnthropicResponseFormat(converted, firstNonNil(request["output_config"], request["output_format"]))
	if metadata, ok := request["metadata"].(map[string]any); ok {
		copyStringFieldAs(converted, metadata, "user_id", "user")
	}
	copyStringField(converted, request, "service_tier")

	messages := make([]any, 0, len(rawMessages)+1)
	if system := convertAnthropicSystem(request["system"]); system != nil {
		messages = append(messages, system)
	}
	for i, rawMessage := range rawMessages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("invalid message at index %d", i+1)
		}
		role, ok := message["role"].(string)
		if !ok || role == "" {
			return nil, nil, fmt.Errorf("invalid message at index %d", i+1)
		}
		convertedMessages, err := convertAnthropicMessage(role, message["content"])
		if err != nil {
			return nil, nil, fmt.Errorf("invalid message content at index %d: %w", i+1, err)
		}
		messages = append(messages, convertedMessages...)
	}
	converted["messages"] = messages

	tools, toolNameMap := convertAnthropicTools(request["tools"])
	if len(tools) > 0 {
		converted["tools"] = tools
	}
	rewriteConvertedToolChoice(converted, toolNameMap)

	encoded, err := json.Marshal(converted)
	if err != nil {
		return nil, nil, fmt.Errorf("encode OpenAI Chat request: %w", err)
	}
	return encoded, toolNameMap, nil
}

func ConvertOpenAIChatToAnthropic(body []byte, model string, toolNameMap map[string]string) ([]byte, error) {
	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decode OpenAI Chat response: %w", err)
	}
	if rawError, ok := response["error"]; ok {
		converted := map[string]any{"type": "error", "error": convertOpenAIError(rawError)}
		return marshalConvertedAnthropicResponse(converted)
	}
	choices, _ := response["choices"].([]any)
	if len(choices) == 0 {
		return nil, fmt.Errorf("no choices in response")
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	content := make([]any, 0)
	if reasoning := stringValue(firstNonNil(message["reasoning_content"], message["reasoning"])); reasoning != "" {
		content = append(content, map[string]any{
			"type": "thinking", "thinking": reasoning, "signature": "",
		})
	}
	if text, ok := message["content"].(string); ok && text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, rawToolCall := range asMessages(message["tool_calls"]) {
		toolCall, _ := rawToolCall.(map[string]any)
		function, _ := toolCall["function"].(map[string]any)
		name, _ := function["name"].(string)
		if original := toolNameMap[name]; original != "" {
			name = original
		}
		input := map[string]any{}
		if arguments, ok := function["arguments"].(string); ok && arguments != "" {
			if err := json.Unmarshal([]byte(arguments), &input); err != nil {
				return nil, fmt.Errorf("invalid tool_call arguments: %w", err)
			}
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": stringValue(toolCall["id"]), "name": name, "input": input,
		})
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	if model == "" {
		model = stringValue(response["model"])
	}
	converted := map[string]any{
		"id":          response["id"],
		"type":        "message",
		"role":        "assistant",
		"model":       model,
		"content":     content,
		"stop_reason": anthropicStopReason(stringValue(choice["finish_reason"])),
		"usage":       anthropicUsage(response["usage"]),
	}
	return marshalConvertedAnthropicResponse(converted)
}

func convertAnthropicMessage(role string, content any) ([]any, error) {
	if text, ok := content.(string); ok {
		return []any{map[string]any{"role": role, "content": text}}, nil
	}
	blocks, ok := content.([]any)
	if !ok {
		return nil, fmt.Errorf("content must be string or array")
	}
	contentParts := make([]any, 0)
	toolCalls := make([]any, 0)
	toolResults := make([]any, 0)
	hasMultimodal := false
	for _, rawBlock := range blocks {
		block, ok := rawBlock.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "text":
			if text, ok := block["text"].(string); ok {
				contentParts = append(contentParts, map[string]any{"type": "text", "text": text})
			}
		case "image", "document":
			if media := convertAnthropicMedia(block); media != nil {
				contentParts = append(contentParts, media)
				hasMultimodal = true
			}
		case "tool_use":
			id, idOK := block["id"].(string)
			name, nameOK := block["name"].(string)
			if idOK && nameOK {
				arguments, err := json.Marshal(firstNonNil(block["input"], map[string]any{}))
				if err != nil {
					return nil, err
				}
				toolCalls = append(toolCalls, map[string]any{
					"id": id, "type": "function",
					"function": map[string]any{"name": name, "arguments": string(arguments)},
				})
			}
		case "tool_result":
			if id, ok := block["tool_use_id"].(string); ok {
				toolResults = append(toolResults, map[string]any{
					"role": "tool", "tool_call_id": id, "content": convertAnthropicToolResult(block["content"]),
				})
			}
		}
	}
	if len(toolResults) > 0 {
		messages := make([]any, 0, len(toolResults)+1)
		if text := textFromOpenAIContentParts(contentParts); text != "" {
			messages = append(messages, map[string]any{"role": role, "content": text})
		}
		return append(messages, toolResults...), nil
	}
	message := map[string]any{"role": role}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		if text := textFromOpenAIContentParts(contentParts); text != "" {
			message["content"] = text
		}
	} else if hasMultimodal || len(contentParts) > 1 {
		message["content"] = contentParts
	} else if len(contentParts) == 1 {
		message["content"] = contentParts[0].(map[string]any)["text"]
	} else {
		message["content"] = ""
	}
	return []any{message}, nil
}

func convertAnthropicMedia(block map[string]any) map[string]any {
	source, _ := block["source"].(map[string]any)
	sourceType, _ := source["type"].(string)
	if sourceType == "url" && block["type"] == "image" {
		if value, ok := source["url"].(string); ok && value != "" {
			return map[string]any{"type": "image_url", "image_url": map[string]any{"url": value}}
		}
	}
	if sourceType != "base64" {
		return nil
	}
	data, _ := source["data"].(string)
	if data == "" {
		return nil
	}
	mediaType, _ := source["media_type"].(string)
	if mediaType == "" {
		if block["type"] == "document" {
			mediaType = "application/pdf"
		} else {
			mediaType = "image/png"
		}
	}
	return map[string]any{
		"type": "image_url", "image_url": map[string]any{"url": "data:" + mediaType + ";base64," + data},
	}
}

func convertAnthropicSystem(system any) map[string]any {
	if text, ok := system.(string); ok && text != "" {
		return map[string]any{"role": "system", "content": text}
	}
	blocks, _ := system.([]any)
	var content strings.Builder
	for _, rawBlock := range blocks {
		block, _ := rawBlock.(map[string]any)
		if block["type"] == "text" {
			content.WriteString(stripAnthropicBillingCCH(stringValue(block["text"])))
		}
	}
	if content.Len() == 0 {
		return nil
	}
	return map[string]any{"role": "system", "content": content.String()}
}

func stripAnthropicBillingCCH(text string) string {
	const prefix = "x-anthropic-billing-header:"
	if !strings.HasPrefix(strings.ToLower(text), prefix) {
		return text
	}
	parts := strings.Split(text[len(prefix):], ";")
	kept := parts[:0]
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" && !strings.HasPrefix(part, "cch=") {
			kept = append(kept, part)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return prefix + strings.Join(kept, ";")
}

func convertAnthropicTools(value any) ([]any, map[string]string) {
	rawTools, _ := value.([]any)
	tools := make([]any, 0, len(rawTools))
	nameMap := make(map[string]string)
	used := make(map[string]bool)
	for _, rawTool := range rawTools {
		tool, _ := rawTool.(map[string]any)
		if isAnthropicBuiltinTool(stringValue(tool["type"])) {
			continue
		}
		original, _ := tool["name"].(string)
		if original == "" {
			continue
		}
		name := uniqueOpenAIToolName(sanitizeOpenAIToolName(original), used)
		used[name] = true
		if name != original {
			nameMap[name] = original
		}
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": name, "description": tool["description"], "parameters": tool["input_schema"],
			},
		})
	}
	return tools, nameMap
}

func sanitizeOpenAIToolName(name string) string {
	name = invalidOpenAIToolName.ReplaceAllString(name, "_")
	for len(name) > openAIToolNameMaxLength {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}

func uniqueOpenAIToolName(name string, used map[string]bool) string {
	if !used[name] {
		return name
	}
	for suffix := 2; ; suffix++ {
		tail := fmt.Sprintf("_%d", suffix)
		base := name
		for len(base)+len(tail) > openAIToolNameMaxLength {
			base = base[:len(base)-1]
		}
		candidate := base + tail
		if !used[candidate] {
			return candidate
		}
	}
}

func isAnthropicBuiltinTool(toolType string) bool {
	for _, prefix := range []string{"computer_", "bash_", "text_editor_", "web_search", "code_execution_"} {
		if strings.HasPrefix(toolType, prefix) {
			return true
		}
	}
	return false
}

func convertAnthropicToolResult(value any) any {
	if text, ok := value.(string); ok {
		return text
	}
	blocks, _ := value.([]any)
	parts := make([]any, 0)
	texts := make([]string, 0)
	hasMedia := false
	for _, rawBlock := range blocks {
		block, _ := rawBlock.(map[string]any)
		if block["type"] == "text" {
			text := stringValue(block["text"])
			texts = append(texts, text)
			parts = append(parts, map[string]any{"type": "text", "text": text})
		} else if media := convertAnthropicMedia(block); media != nil {
			parts = append(parts, media)
			hasMedia = true
		}
	}
	if hasMedia {
		return parts
	}
	return strings.Join(texts, "")
}

func textFromOpenAIContentParts(parts []any) string {
	var text strings.Builder
	for _, rawPart := range parts {
		part, _ := rawPart.(map[string]any)
		if part["type"] == "text" {
			text.WriteString(stringValue(part["text"]))
		}
	}
	return text.String()
}

func convertAnthropicThinking(dst map[string]any, value any) {
	thinking, _ := value.(map[string]any)
	if thinking["type"] != "enabled" {
		return
	}
	budget, ok := thinking["budget_tokens"].(float64)
	switch {
	case !ok || budget < 16384 && budget >= 4096:
		dst["reasoning_effort"] = "medium"
	case budget < 4096:
		dst["reasoning_effort"] = "low"
	default:
		dst["reasoning_effort"] = "high"
	}
}

func convertAnthropicToolChoice(dst map[string]any, value any) {
	choice, _ := value.(map[string]any)
	switch choice["type"] {
	case "auto":
		dst["tool_choice"] = "auto"
	case "any":
		dst["tool_choice"] = "required"
	case "none":
		dst["tool_choice"] = "none"
	case "tool":
		if name, ok := choice["name"].(string); ok {
			dst["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}
	if choice["disable_parallel_tool_use"] == true {
		dst["parallel_tool_calls"] = false
	}
}

func convertAnthropicResponseFormat(dst map[string]any, value any) {
	format, _ := value.(map[string]any)
	switch format["type"] {
	case "json_schema":
		if schema := format["json_schema"]; schema != nil {
			dst["response_format"] = map[string]any{"type": "json_schema", "json_schema": schema}
		}
	case "json", "json_object":
		dst["response_format"] = map[string]any{"type": "json_object"}
	}
}

func rewriteConvertedToolChoice(body map[string]any, nameMap map[string]string) {
	choice, _ := body["tool_choice"].(map[string]any)
	function, _ := choice["function"].(map[string]any)
	name, _ := function["name"].(string)
	for sanitized, original := range nameMap {
		if original == name {
			function["name"] = sanitized
			return
		}
	}
}

func copyField(dst, src map[string]any, key string) {
	if value, ok := src[key]; ok {
		dst[key] = value
	}
}

func copyStringField(dst, src map[string]any, key string) {
	copyStringFieldAs(dst, src, key, key)
}

func copyStringFieldAs(dst, src map[string]any, sourceKey, targetKey string) {
	if value, ok := src[sourceKey].(string); ok {
		dst[targetKey] = value
	}
}

func convertOpenAIError(value any) map[string]any {
	errorType := "api_error"
	message := ""
	switch typed := value.(type) {
	case map[string]any:
		if candidate := stringValue(firstNonNil(typed["type"], typed["code"])); candidate != "" {
			errorType = candidate
		}
		message = stringValue(typed["message"])
	case string:
		message = typed
	}
	return map[string]any{"type": errorType, "message": message}
}

func anthropicStopReason(reason string) string {
	switch reason {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return "end_turn"
	}
}

func anthropicUsage(value any) map[string]any {
	raw, _ := value.(map[string]any)
	prompt := numericFloat(raw["prompt_tokens"])
	completion := numericFloat(raw["completion_tokens"])
	usage := map[string]any{"input_tokens": prompt, "output_tokens": completion}
	if details, ok := raw["prompt_tokens_details"].(map[string]any); ok {
		cached := numericFloat(details["cached_tokens"])
		usage["input_tokens"] = math.Max(0, prompt-cached)
		usage["cache_read_input_tokens"] = cached
		if created, ok := details["cache_creation_input_tokens"]; ok {
			usage["cache_creation_input_tokens"] = created
		}
	}
	return usage
}

func numericFloat(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case int64:
		return float64(typed)
	case int:
		return float64(typed)
	default:
		return 0
	}
}

func marshalConvertedAnthropicResponse(value map[string]any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode Anthropic response: %w", err)
	}
	return encoded, nil
}
