package ai_protocols

import (
	"fmt"
	"strings"

	"github.com/wklken/apisix-go/pkg/json"
)

func ConvertOpenAIEmbeddingsToVertex(body []byte) ([]byte, error) {
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, fmt.Errorf("decode OpenAI embeddings request: %w", err)
	}
	inputs, err := vertexInputTexts(request["input"])
	if err != nil {
		return nil, err
	}
	instances := make([]map[string]any, 0, len(inputs))
	for _, input := range inputs {
		instances = append(instances, map[string]any{"content": input})
	}
	converted, err := json.Marshal(map[string]any{"instances": instances})
	if err != nil {
		return nil, fmt.Errorf("encode Vertex predict request: %w", err)
	}
	return converted, nil
}

func ConvertVertexEmbeddingsToOpenAI(body []byte, model string) ([]byte, error) {
	var response struct {
		Predictions []struct {
			Embeddings struct {
				Values     []any `json:"values"`
				Statistics struct {
					TokenCount int64 `json:"token_count"`
				} `json:"statistics"`
			} `json:"embeddings"`
		} `json:"predictions"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decode Vertex predict response: %w", err)
	}
	if response.Predictions == nil {
		return nil, fmt.Errorf("vertex response missing predictions")
	}
	data := make([]map[string]any, 0, len(response.Predictions))
	var totalTokens int64
	for i, prediction := range response.Predictions {
		if prediction.Embeddings.Values == nil {
			return nil, fmt.Errorf("invalid embedding at index %d", i+1)
		}
		data = append(data, map[string]any{
			"object":    "embedding",
			"index":     i,
			"embedding": prediction.Embeddings.Values,
		})
		totalTokens += prediction.Embeddings.Statistics.TokenCount
	}
	converted, err := json.Marshal(map[string]any{
		"object": "list",
		"data":   data,
		"model":  model,
		"usage": map[string]any{
			"prompt_tokens": totalTokens,
			"total_tokens":  totalTokens,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encode OpenAI embeddings response: %w", err)
	}
	return converted, nil
}

func vertexInputTexts(input any) ([]string, error) {
	switch typed := input.(type) {
	case string:
		return []string{typed}, nil
	case []any:
		texts := make([]string, 0, len(typed))
		for i, item := range typed {
			switch value := item.(type) {
			case string:
				texts = append(texts, value)
			case []any:
				parts := make([]string, len(value))
				for j, token := range value {
					parts[j] = fmt.Sprint(token)
				}
				texts = append(texts, strings.Join(parts, " "))
			default:
				return nil, fmt.Errorf("unsupported input type at index %d", i+1)
			}
		}
		return texts, nil
	case nil:
		return nil, fmt.Errorf("input is required for embeddings")
	default:
		return nil, fmt.Errorf("input must be string or array")
	}
}
