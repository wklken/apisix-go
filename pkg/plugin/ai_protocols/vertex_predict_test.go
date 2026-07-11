package ai_protocols

import (
	"testing"

	"github.com/wklken/apisix-go/pkg/json"
)

func TestConvertOpenAIEmbeddingsToVertex(t *testing.T) {
	converted, err := ConvertOpenAIEmbeddingsToVertex([]byte(`{
	  "model":"text-embedding-005",
	  "input":["hello",[1,2,3]],
	  "encoding_format":"float"
	}`))
	if err != nil {
		t.Fatalf("ConvertOpenAIEmbeddingsToVertex() error = %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(converted, &body); err != nil {
		t.Fatalf("decode converted request: %v", err)
	}
	instances := body["instances"].([]any)
	if len(body) != 1 || instances[0].(map[string]any)["content"] != "hello" ||
		instances[1].(map[string]any)["content"] != "1 2 3" {
		t.Fatalf("converted request = %#v", body)
	}
}

func TestConvertVertexEmbeddingsToOpenAI(t *testing.T) {
	converted, err := ConvertVertexEmbeddingsToOpenAI([]byte(`{
	  "predictions":[
	    {"embeddings":{"values":[0.1,0.2],"statistics":{"token_count":3}}},
	    {"embeddings":{"values":[0.3,0.4],"statistics":{"token_count":4}}}
	  ]
	}`), "text-embedding-005")
	if err != nil {
		t.Fatalf("ConvertVertexEmbeddingsToOpenAI() error = %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(converted, &body); err != nil {
		t.Fatalf("decode converted response: %v", err)
	}
	data := body["data"].([]any)
	usage := body["usage"].(map[string]any)
	if body["object"] != "list" || body["model"] != "text-embedding-005" || len(data) != 2 ||
		usage["total_tokens"] != float64(7) {
		t.Fatalf("converted response = %#v", body)
	}
}
