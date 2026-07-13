package ai_stream

import (
	"bufio"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"

	"github.com/wklken/apisix-go/pkg/json"
)

type anthropicStreamState struct {
	started          bool
	done             bool
	nextContentIndex int
	currentBlock     int
	hasCurrentBlock  bool
	currentBlockType string
	toolCallIndices  map[int]int
	pendingDelta     map[string]any
	usage            Usage
}

type anthropicSSEEvent struct {
	typeName string
	data     map[string]any
}

func ForwardOpenAIAsAnthropicSSE(
	w http.ResponseWriter,
	body io.Reader,
	maxBytes int64,
	toolNameMap map[string]string,
) (Usage, error) {
	state := anthropicStreamState{
		toolCallIndices: make(map[int]int),
		usage: Usage{
			Raw: make(map[string]any), PromptTokens: -1, CompletionTokens: -1,
		},
	}
	reader := bufio.NewReader(body)
	var total int64
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			total += int64(len(line))
			if maxBytes > 0 && total > maxBytes {
				return state.usage, fmt.Errorf("max_response_bytes exceeded")
			}
			events, conversionErr := state.convertLine(line, toolNameMap)
			if conversionErr != nil {
				return state.usage, conversionErr
			}
			for _, event := range events {
				if writeErr := writeAnthropicSSEEvent(w, event); writeErr != nil {
					return state.usage, writeErr
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return state.usage, err
		}
	}
	if events := state.finish(); len(events) > 0 {
		for _, event := range events {
			if err := writeAnthropicSSEEvent(w, event); err != nil {
				return state.usage, err
			}
		}
	}
	return state.usage, nil
}

func (s *anthropicStreamState) convertLine(line string, toolNameMap map[string]string) ([]anthropicSSEEvent, error) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return nil, nil
	}
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "" {
		return nil, nil
	}
	if data == "[DONE]" {
		return s.finish(), nil
	}
	var chunk map[string]any
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return nil, fmt.Errorf("decode OpenAI SSE chunk: %w", err)
	}
	return s.convertChunk(chunk, toolNameMap), nil
}

func (s *anthropicStreamState) convertChunk(
	chunk map[string]any,
	toolNameMap map[string]string,
) []anthropicSSEEvent {
	if usage, ok := chunk["usage"].(map[string]any); ok {
		s.mergeUsage(usage)
	}
	if s.done {
		return s.flushPendingStop()
	}
	events := make([]anthropicSSEEvent, 0)
	if !s.started {
		s.started = true
		s.usage.Model = stringValue(chunk["model"])
		events = append(events, newAnthropicSSEEvent("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": chunk["id"], "type": "message", "role": "assistant", "model": chunk["model"],
				"content": []any{}, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		}))
	}
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return events
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	if reasoning := stringValue(firstValue(delta["reasoning_content"], delta["reasoning"])); reasoning != "" {
		events = append(events, s.ensureBlock("thinking", map[string]any{
			"type": "thinking", "thinking": "",
		})...)
		events = append(events, newAnthropicSSEEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.currentBlock,
			"delta": map[string]any{"type": "thinking_delta", "thinking": reasoning},
		}))
	}
	if text := stringValue(delta["content"]); text != "" {
		s.usage.Text += text
		events = append(events, s.ensureBlock("text", map[string]any{"type": "text", "text": ""})...)
		events = append(events, newAnthropicSSEEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.currentBlock,
			"delta": map[string]any{"type": "text_delta", "text": text},
		}))
	}
	for _, rawToolCall := range asSlice(delta["tool_calls"]) {
		toolCall, _ := rawToolCall.(map[string]any)
		index := int(numericValue(toolCall["index"]))
		contentIndex, exists := s.toolCallIndices[index]
		function, _ := toolCall["function"].(map[string]any)
		if !exists {
			events = append(events, s.closeCurrentBlock()...)
			contentIndex = s.nextContentIndex
			s.nextContentIndex++
			s.toolCallIndices[index] = contentIndex
			s.currentBlock = contentIndex
			s.hasCurrentBlock = true
			s.currentBlockType = "tool_use"
			name := stringValue(function["name"])
			if original := toolNameMap[name]; original != "" {
				name = original
			}
			events = append(events, newAnthropicSSEEvent("content_block_start", map[string]any{
				"type": "content_block_start", "index": contentIndex,
				"content_block": map[string]any{
					"type": "tool_use", "id": stringValue(toolCall["id"]), "name": name, "input": map[string]any{},
				},
			}))
		}
		if arguments := stringValue(function["arguments"]); arguments != "" {
			events = append(events, newAnthropicSSEEvent("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": contentIndex,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": arguments},
			}))
		}
	}
	if reason := normalizeFinishReason(choice["finish_reason"]); reason != "" {
		events = append(events, s.closeCurrentBlock()...)
		s.pendingDelta = map[string]any{
			"type": "message_delta", "delta": map[string]any{"stop_reason": anthropicFinishReason(reason)},
		}
		if s.usage.PromptTokens >= 0 || s.usage.CompletionTokens >= 0 {
			s.pendingDelta["usage"] = s.anthropicUsage()
		}
		s.done = true
	}
	return events
}

func (s *anthropicStreamState) ensureBlock(blockType string, block map[string]any) []anthropicSSEEvent {
	if s.hasCurrentBlock && s.currentBlockType == blockType {
		return nil
	}
	events := s.closeCurrentBlock()
	s.currentBlock = s.nextContentIndex
	s.nextContentIndex++
	s.hasCurrentBlock = true
	s.currentBlockType = blockType
	return append(events, newAnthropicSSEEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": s.currentBlock, "content_block": block,
	}))
}

func (s *anthropicStreamState) closeCurrentBlock() []anthropicSSEEvent {
	if !s.hasCurrentBlock {
		return nil
	}
	event := newAnthropicSSEEvent("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": s.currentBlock,
	})
	s.hasCurrentBlock = false
	s.currentBlockType = ""
	return []anthropicSSEEvent{event}
}

func (s *anthropicStreamState) finish() []anthropicSSEEvent {
	if s.pendingDelta != nil {
		return s.flushPendingStop()
	}
	if !s.started || s.done {
		return nil
	}
	events := s.closeCurrentBlock()
	s.pendingDelta = map[string]any{
		"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"},
		"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
	}
	s.done = true
	return append(events, s.flushPendingStop()...)
}

func (s *anthropicStreamState) flushPendingStop() []anthropicSSEEvent {
	if s.pendingDelta == nil {
		return nil
	}
	if s.usage.PromptTokens >= 0 || s.usage.CompletionTokens >= 0 {
		s.pendingDelta["usage"] = s.anthropicUsage()
	}
	events := []anthropicSSEEvent{
		newAnthropicSSEEvent("message_delta", s.pendingDelta),
		newAnthropicSSEEvent("message_stop", map[string]any{"type": "message_stop"}),
	}
	s.pendingDelta = nil
	return events
}

func (s *anthropicStreamState) mergeUsage(raw map[string]any) {
	maps.Copy(s.usage.Raw, raw)
	s.usage.PromptTokens = numericUsage(raw["prompt_tokens"])
	s.usage.CompletionTokens = numericUsage(raw["completion_tokens"])
	if model := stringValue(raw["model"]); model != "" {
		s.usage.Model = model
	}
}

func (s *anthropicStreamState) anthropicUsage() map[string]any {
	prompt := max(s.usage.PromptTokens, 0)
	completion := max(s.usage.CompletionTokens, 0)
	usage := map[string]any{"input_tokens": prompt, "output_tokens": completion}
	if details, ok := s.usage.Raw["prompt_tokens_details"].(map[string]any); ok {
		cached := numericUsage(details["cached_tokens"])
		if cached > 0 {
			usage["input_tokens"] = maxInt64(0, prompt-cached)
			usage["cache_read_input_tokens"] = cached
		}
		if created := numericUsage(details["cache_creation_input_tokens"]); created >= 0 {
			usage["cache_creation_input_tokens"] = created
		}
	}
	return usage
}

func writeAnthropicSSEEvent(w http.ResponseWriter, event anthropicSSEEvent) error {
	encoded, err := json.Marshal(event.data)
	if err != nil {
		return fmt.Errorf("encode Anthropic SSE event: %w", err)
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.typeName, encoded); err != nil {
		return err
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func newAnthropicSSEEvent(typeName string, data map[string]any) anthropicSSEEvent {
	return anthropicSSEEvent{typeName: typeName, data: data}
}

func anthropicFinishReason(reason string) string {
	switch reason {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return "end_turn"
	}
}

func normalizeFinishReason(value any) string {
	reason := strings.TrimSpace(stringValue(value))
	if reason == "null" {
		return ""
	}
	return reason
}

func firstValue(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func asSlice(value any) []any {
	values, _ := value.([]any)
	return values
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func numericValue(value any) int64 {
	if parsed := numericUsage(value); parsed >= 0 {
		return parsed
	}
	return 0
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
