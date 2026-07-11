package ai_protocols

import (
	"reflect"
	"strings"
	"testing"
)

func TestDetectUsesAPISIXProtocolOrder(t *testing.T) {
	tests := []struct {
		name string
		path string
		body map[string]any
		want Protocol
	}{
		{
			name: "bedrock before chat",
			path: "/model/anthropic/converse",
			body: map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"text": "hello"}}}},
			},
			want: BedrockConverse,
		},
		{
			name: "anthropic before chat",
			path: "/v1/messages",
			body: map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": "hello"}},
			},
			want: AnthropicMessages,
		},
		{name: "anthropic empty object", path: "/v1/messages", body: map[string]any{}, want: AnthropicMessages},
		{
			name: "responses before chat and embeddings",
			path: "/v1/responses",
			body: map[string]any{
				"input":    "hello",
				"messages": []any{map[string]any{"role": "user", "content": "hello"}},
			},
			want: OpenAIResponses,
		},
		{
			name: "chat before embeddings",
			path: "/anything",
			body: map[string]any{
				"input":    "hello",
				"messages": []any{map[string]any{"role": "user", "content": "hello"}},
			},
			want: OpenAIChat,
		},
		{name: "embeddings", path: "/anything", body: map[string]any{"input": "hello"}, want: OpenAIEmbeddings},
		{name: "passthrough", path: "/anything", body: map[string]any{"prompt": "hello"}, want: Passthrough},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			protocol, err := Detect(tt.path, tt.body)
			if err != nil {
				t.Fatalf("Detect() error = %v", err)
			}
			if protocol != tt.want {
				t.Fatalf("Detect() = %#v, want %#v", protocol, tt.want)
			}
		})
	}
}

func TestDetectRejectsUnsupportedBodies(t *testing.T) {
	_, err := Detect("/anything", map[string]any{})
	if err == nil {
		t.Fatal("Detect() error = nil, want unsupported protocol error")
	}
	if !strings.Contains(err.Error(), "unsupported AI request protocol") {
		t.Fatalf("Detect() error = %q, want actionable unsupported protocol message", err)
	}
}

func TestProtocolMessagesAndMutation(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		body         map[string]any
		prepend      []Message
		wantMessages []Message
		wantBody     map[string]any
	}{
		{
			name: "chat flattens content blocks",
			path: "/v1/chat/completions",
			body: map[string]any{
				"messages": []any{map[string]any{
					"role": "user",
					"content": []any{
						map[string]any{"type": "text", "text": "hello"},
						map[string]any{"type": "image_url", "image_url": "ignored"},
						map[string]any{"type": "text", "text": "world"},
					},
				}},
			},
			wantMessages: []Message{{Role: "user", Content: "hello world"}},
			wantBody: map[string]any{
				"messages": []any{
					map[string]any{"role": "system", "content": "before"},
					map[string]any{
						"role": "user",
						"content": []any{
							map[string]any{"type": "text", "text": "hello"},
							map[string]any{"type": "image_url", "image_url": "ignored"},
							map[string]any{"type": "text", "text": "world"},
						},
					},
					map[string]any{"role": "user", "content": "after"},
				},
			},
		},
		{
			name: "responses use instructions and input",
			path: "/v1/responses",
			body: map[string]any{
				"instructions": "existing",
				"input":        []any{"first", map[string]any{"role": "assistant", "content": "second"}},
			},
			wantMessages: []Message{
				{Role: "system", Content: "existing"},
				{Role: "user", Content: "first"},
				{Role: "assistant", Content: "second"},
			},
			wantBody: map[string]any{
				"instructions": "before\nexisting",
				"input": []any{
					"first",
					map[string]any{"role": "assistant", "content": "second"},
					map[string]any{"type": "message", "role": "user", "content": "after"},
				},
			},
		},
		{
			name: "anthropic preserves system and text blocks",
			path: "/v1/messages",
			body: map[string]any{
				"system": "system policy",
				"messages": []any{map[string]any{
					"role":    "user",
					"content": []any{map[string]any{"type": "text", "text": "question"}},
				}},
			},
			wantMessages: []Message{{Role: "system", Content: "system policy"}, {Role: "user", Content: "question"}},
			wantBody: map[string]any{
				"system": "system policy",
				"messages": []any{
					map[string]any{"role": "system", "content": "before"},
					map[string]any{
						"role":    "user",
						"content": []any{map[string]any{"type": "text", "text": "question"}},
					},
					map[string]any{"role": "user", "content": "after"},
				},
			},
		},
		{
			name: "bedrock separates system messages",
			path: "/model/x/converse",
			body: map[string]any{
				"system": []any{map[string]any{"text": "system policy"}},
				"messages": []any{map[string]any{
					"role":    "user",
					"content": []any{map[string]any{"text": "question"}},
				}},
			},
			wantMessages: []Message{{Role: "system", Content: "system policy"}, {Role: "user", Content: "question"}},
			prepend:      []Message{{Role: "system", Content: "before"}, {Role: "user", Content: "before"}},
			wantBody: map[string]any{
				"system": []any{map[string]any{"text": "before"}, map[string]any{"text": "system policy"}},
				"messages": []any{
					map[string]any{"role": "user", "content": []any{map[string]any{"text": "before"}}},
					map[string]any{"role": "user", "content": []any{map[string]any{"text": "question"}}},
					map[string]any{"role": "user", "content": []any{map[string]any{"text": "after"}}},
				},
			},
		},
		{
			name:         "embeddings expose string input without decoration",
			path:         "/v1/embeddings",
			body:         map[string]any{"input": "embedding text"},
			wantMessages: []Message{{Role: "user", Content: "embedding text"}},
			wantBody:     map[string]any{"input": "embedding text"},
		},
		{
			name:         "passthrough has no messages or decoration",
			path:         "/anything",
			body:         map[string]any{"prompt": "passthrough"},
			wantMessages: []Message{},
			wantBody:     map[string]any{"prompt": "passthrough"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			protocol, err := Detect(tt.path, tt.body)
			if err != nil {
				t.Fatalf("Detect() error = %v", err)
			}
			if got := ExtractMessages(protocol, tt.body); !reflect.DeepEqual(got, tt.wantMessages) {
				t.Fatalf("ExtractMessages() = %#v, want %#v", got, tt.wantMessages)
			}
			prepend := tt.prepend
			if len(prepend) == 0 {
				prepend = []Message{{Role: "system", Content: "before"}}
			}
			PrependMessages(protocol, tt.body, prepend)
			AppendMessages(protocol, tt.body, []Message{{Role: "user", Content: "after"}})
			if !reflect.DeepEqual(tt.body, tt.wantBody) {
				t.Fatalf("mutated body = %#v, want %#v", tt.body, tt.wantBody)
			}
		})
	}
}

func TestBuildDenyResponseUsesNativeProtocolShapes(t *testing.T) {
	tests := []struct {
		name     string
		protocol Protocol
		assert   func(*testing.T, map[string]any)
	}{
		{
			name:     "chat",
			protocol: OpenAIChat,
			assert: func(t *testing.T, body map[string]any) {
				choices := body["choices"].([]any)
				if body["object"] != "chat.completion" ||
					choices[0].(map[string]any)["message"].(map[string]any)["content"] != "blocked" {
					t.Fatalf("chat deny body = %#v", body)
				}
			},
		},
		{
			name:     "responses",
			protocol: OpenAIResponses,
			assert: func(t *testing.T, body map[string]any) {
				output := body["output"].([]any)
				content := output[0].(map[string]any)["content"].([]any)
				if body["object"] != "response" || content[0].(map[string]any)["text"] != "blocked" {
					t.Fatalf("responses deny body = %#v", body)
				}
			},
		},
		{
			name:     "embeddings",
			protocol: OpenAIEmbeddings,
			assert: func(t *testing.T, body map[string]any) {
				if body["error"].(map[string]any)["type"] != "content_policy_violation" {
					t.Fatalf("embeddings deny body = %#v", body)
				}
			},
		},
		{
			name:     "anthropic",
			protocol: AnthropicMessages,
			assert: func(t *testing.T, body map[string]any) {
				content := body["content"].([]any)
				if body["type"] != "message" || content[0].(map[string]any)["text"] != "blocked" {
					t.Fatalf("anthropic deny body = %#v", body)
				}
			},
		},
		{
			name:     "bedrock",
			protocol: BedrockConverse,
			assert: func(t *testing.T, body map[string]any) {
				content := body["output"].(map[string]any)["message"].(map[string]any)["content"].([]any)
				if body["stopReason"] != "end_turn" || content[0].(map[string]any)["text"] != "blocked" {
					t.Fatalf("bedrock deny body = %#v", body)
				}
			},
		},
		{
			name:     "passthrough",
			protocol: Passthrough,
			assert: func(t *testing.T, body map[string]any) {
				if body["message"] != "blocked" {
					t.Fatalf("passthrough deny body = %#v", body)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.assert(t, BuildDenyResponse(tt.protocol, "model", "blocked"))
		})
	}
}

func TestBuildSimpleRequestAndExtractResponseText(t *testing.T) {
	tests := []struct {
		name         string
		protocol     Protocol
		assertBody   func(*testing.T, map[string]any)
		responseBody map[string]any
		wantText     string
	}{
		{
			name:     "chat",
			protocol: OpenAIChat,
			assertBody: func(t *testing.T, body map[string]any) {
				if body["model"] != "model-a" || len(body["messages"].([]any)) != 2 {
					t.Fatalf("chat request body = %#v", body)
				}
			},
			responseBody: map[string]any{"choices": []any{map[string]any{
				"message": map[string]any{"content": "rewritten"},
			}}},
			wantText: "rewritten",
		},
		{
			name:     "anthropic",
			protocol: AnthropicMessages,
			assertBody: func(t *testing.T, body map[string]any) {
				if body["system"] != "rewrite" || body["max_tokens"] != 4096 {
					t.Fatalf("anthropic request body = %#v", body)
				}
			},
			responseBody: map[string]any{"content": []any{map[string]any{"type": "text", "text": "rewritten"}}},
			wantText:     "rewritten",
		},
		{
			name:     "bedrock",
			protocol: BedrockConverse,
			assertBody: func(t *testing.T, body map[string]any) {
				if _, ok := body["model"]; ok || body["system"] == nil {
					t.Fatalf("bedrock request body = %#v", body)
				}
			},
			responseBody: map[string]any{"output": map[string]any{"message": map[string]any{
				"content": []any{map[string]any{"text": "rewritten"}},
			}}},
			wantText: "rewritten",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := BuildSimpleRequest(tt.protocol, "rewrite", "original", map[string]any{"model": "model-a"})
			tt.assertBody(t, body)
			if got := ExtractResponseText(tt.protocol, tt.responseBody); got != tt.wantText {
				t.Fatalf("ExtractResponseText() = %q, want %q", got, tt.wantText)
			}
		})
	}
}

func TestExtractRequestContentFromEmbeddingArray(t *testing.T) {
	contents := ExtractRequestContent(OpenAIEmbeddings, map[string]any{
		"input": []any{"first", "second"},
	})
	if len(contents) != 2 || contents[0] != "first" || contents[1] != "second" {
		t.Fatalf("contents = %#v", contents)
	}
}

func TestBuildDenyWireResponseUsesNativeStreamingShape(t *testing.T) {
	for _, test := range []struct {
		name     string
		protocol Protocol
		want     []string
	}{
		{"chat", OpenAIChat, []string{`"object":"chat.completion.chunk"`, "data: [DONE]"}},
		{"responses", OpenAIResponses, []string{"event: response.output_text.delta", "event: response.completed"}},
		{"anthropic", AnthropicMessages, []string{"event: message_start", "event: message_stop"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			wire, contentType, err := BuildDenyWireResponse(test.protocol, "model", "blocked", true)
			if err != nil {
				t.Fatalf("BuildDenyWireResponse() error = %v", err)
			}
			if contentType != "text/event-stream" {
				t.Fatalf("content type = %q", contentType)
			}
			for _, expected := range test.want {
				if !strings.Contains(string(wire), expected) {
					t.Fatalf("wire response missing %q: %s", expected, wire)
				}
			}
		})
	}
}

func TestExtractResponseMetadata(t *testing.T) {
	tests := []struct {
		name       string
		protocol   Protocol
		body       string
		wantPrompt int64
		wantOutput int64
	}{
		{
			name:       "chat",
			protocol:   OpenAIChat,
			body:       `{"model":"gpt-4.1","usage":{"prompt_tokens":12,"completion_tokens":5}}`,
			wantPrompt: 12,
			wantOutput: 5,
		},
		{
			name:       "responses",
			protocol:   OpenAIResponses,
			body:       `{"model":"gpt-4.1","usage":{"input_tokens":9,"output_tokens":3}}`,
			wantPrompt: 9,
			wantOutput: 3,
		},
		{
			name:       "embeddings",
			protocol:   OpenAIEmbeddings,
			body:       `{"model":"text-embedding-3-small","usage":{"prompt_tokens":7}}`,
			wantPrompt: 7,
			wantOutput: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metadata := ExtractResponseMetadata(tt.protocol, []byte(tt.body))
			if metadata.Model == "" {
				t.Fatal("Model is empty, want response model")
			}
			if metadata.PromptTokens != tt.wantPrompt {
				t.Fatalf("PromptTokens = %d, want %d", metadata.PromptTokens, tt.wantPrompt)
			}
			if metadata.CompletionTokens != tt.wantOutput {
				t.Fatalf("CompletionTokens = %d, want %d", metadata.CompletionTokens, tt.wantOutput)
			}
		})
	}
}

func TestExtractResponseMetadataUsesNativeUsageFields(t *testing.T) {
	tests := []struct {
		name     string
		protocol Protocol
		body     string
	}{
		{"anthropic", AnthropicMessages, `{"usage":{"input_tokens":4,"output_tokens":2}}`},
		{"bedrock", BedrockConverse, `{"usage":{"inputTokens":4,"outputTokens":2}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metadata := ExtractResponseMetadata(test.protocol, []byte(test.body))
			if metadata.PromptTokens != 4 || metadata.CompletionTokens != 2 {
				t.Fatalf("metadata = %#v", metadata)
			}
		})
	}
}

func TestRequestContentUsesProtocolNativeShape(t *testing.T) {
	messages := []any{map[string]any{"role": "user", "content": "hello"}}
	tests := []struct {
		protocol Protocol
		body     map[string]any
		want     any
	}{
		{OpenAIChat, map[string]any{"messages": messages}, messages},
		{OpenAIResponses, map[string]any{"input": "hello"}, "hello"},
		{OpenAIEmbeddings, map[string]any{"input": []any{"a", "b"}}, []any{"a", "b"}},
		{AnthropicMessages, map[string]any{"messages": messages}, messages},
		{BedrockConverse, map[string]any{"messages": messages}, messages},
		{Passthrough, map[string]any{"input": "hello"}, nil},
	}

	for _, test := range tests {
		if got := RequestContent(test.protocol, test.body); !reflect.DeepEqual(got, test.want) {
			t.Fatalf("RequestContent(%s) = %#v, want %#v", test.protocol.OverrideKey, got, test.want)
		}
	}
}

func TestExtractStreamEventText(t *testing.T) {
	tests := []struct {
		protocol Protocol
		event    map[string]any
		want     string
	}{
		{OpenAIChat, map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{"content": "chat"},
		}}}, "chat"},
		{OpenAIResponses, map[string]any{"type": "response.output_text.delta", "delta": "responses"}, "responses"},
		{AnthropicMessages, map[string]any{
			"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": "anthropic"},
		}, "anthropic"},
	}

	for _, test := range tests {
		if got := ExtractStreamEventText(test.protocol, test.event); got != test.want {
			t.Fatalf("ExtractStreamEventText(%s) = %q, want %q", test.protocol.OverrideKey, got, test.want)
		}
	}
}
