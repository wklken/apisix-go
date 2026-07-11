package ai_rag

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
	client *http.Client
}

const (
	priority = 1060
	name     = "ai-rag"
)

const schema = `
{
  "type": "object",
  "properties": {
    "embeddings_provider": {
      "type": "object",
      "properties": {
        "azure_openai": {
          "type": "object",
          "properties": {
            "endpoint": {
              "type": "string",
              "minLength": 1
            },
            "api_key": {
              "type": "string",
              "minLength": 1
            }
          },
          "required": ["endpoint", "api_key"]
        }
      },
      "required": ["azure_openai"]
    },
    "vector_search_provider": {
      "type": "object",
      "properties": {
        "azure_ai_search": {
          "type": "object",
          "properties": {
            "endpoint": {
              "type": "string",
              "minLength": 1
            },
            "api_key": {
              "type": "string",
              "minLength": 1
            }
          },
          "required": ["endpoint", "api_key"]
        }
      },
      "required": ["azure_ai_search"]
    },
    "ssl_verify": {
      "type": "boolean",
      "default": true
    }
  },
  "required": ["embeddings_provider", "vector_search_provider"]
}
`

type Config struct {
	EmbeddingsProvider   EmbeddingsProvider   `json:"embeddings_provider"`
	VectorSearchProvider VectorSearchProvider `json:"vector_search_provider"`
	SSLVerify            *bool                `json:"ssl_verify,omitempty"`
}

type EmbeddingsProvider struct {
	AzureOpenAI AzureProvider `json:"azure_openai"`
}

type VectorSearchProvider struct {
	AzureAISearch AzureProvider `json:"azure_ai_search"`
}

type AzureProvider struct {
	Endpoint string `json:"endpoint"`
	APIKey   string `json:"api_key"`
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.SSLVerify == nil {
		sslVerify := true
		p.config.SSLVerify = &sslVerify
	}
	p.client = &http.Client{Transport: p.transport()}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody(r)
		if err != nil {
			writeJSONMessage(w, http.StatusBadRequest, "could not get body: "+err.Error())
			return
		}
		if len(bytes.TrimSpace(body)) == 0 {
			writeJSONMessage(w, http.StatusBadRequest, "could not get body: request body is empty")
			return
		}

		var bodyTab map[string]any
		if err := json.Unmarshal(body, &bodyTab); err != nil {
			writeJSONMessage(w, http.StatusBadRequest, "could not parse JSON request body: "+err.Error())
			return
		}

		embeddingsReq, fields, ok := parseAIRAG(bodyTab)
		if !ok {
			writeJSONMessage(w, http.StatusBadRequest, `request body must have "ai_rag" field`)
			return
		}

		embedding, status, message := p.requestEmbeddings(r, embeddingsReq)
		if status != http.StatusOK {
			writeJSONMessage(w, status, message)
			return
		}

		searchResult, status, message := p.requestVectorSearch(r, fields, embedding)
		if status != http.StatusOK {
			writeJSONMessage(w, status, message)
			return
		}

		delete(bodyTab, "ai_rag")
		appendSearchResult(r, bodyTab, searchResult)

		rewritten, err := json.Marshal(bodyTab)
		if err != nil {
			writeJSONMessage(
				w,
				http.StatusInternalServerError,
				"failed to parse modified JSON request body: "+err.Error(),
			)
			return
		}

		r.Body = io.NopCloser(bytes.NewReader(rewritten))
		r.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(rewritten)), nil
		}
		r.ContentLength = int64(len(rewritten))
		r.Header.Set("Content-Length", fmt.Sprint(len(rewritten)))

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func parseAIRAG(body map[string]any) (map[string]any, string, bool) {
	aiRAG, ok := body["ai_rag"].(map[string]any)
	if !ok {
		return nil, "", false
	}

	embeddings, ok := aiRAG["embeddings"].(map[string]any)
	if !ok {
		return nil, "", false
	}
	if _, ok := embeddings["input"]; !ok {
		return nil, "", false
	}

	vectorSearch, ok := aiRAG["vector_search"].(map[string]any)
	if !ok {
		return nil, "", false
	}
	fields, ok := vectorSearch["fields"].(string)
	if !ok || fields == "" {
		return nil, "", false
	}

	return embeddings, fields, true
}

func (p *Plugin) requestEmbeddings(r *http.Request, embeddingsReq map[string]any) (any, int, string) {
	rawBody, err := json.Marshal(embeddingsReq)
	if err != nil {
		return nil, http.StatusInternalServerError, "failed to encode embeddings request body: " + err.Error()
	}

	provider := p.config.EmbeddingsProvider.AzureOpenAI
	respBody, status, message := p.postAzureJSON(r, provider.Endpoint, provider.APIKey, rawBody, "embeddings")
	if status != http.StatusOK {
		return nil, status, message
	}

	var decoded struct {
		Data []struct {
			Embedding any `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return nil, http.StatusInternalServerError, "failed to decode embeddings response: " + err.Error()
	}
	if len(decoded.Data) == 0 || decoded.Data[0].Embedding == nil {
		return nil, http.StatusInternalServerError, "failed to extract embeddings response"
	}

	return decoded.Data[0].Embedding, http.StatusOK, ""
}

func (p *Plugin) requestVectorSearch(r *http.Request, fields string, embedding any) (string, int, string) {
	rawBody, err := json.Marshal(map[string]any{
		"vectorQueries": []map[string]any{
			{
				"kind":   "vector",
				"vector": embedding,
				"fields": fields,
			},
		},
	})
	if err != nil {
		return "", http.StatusInternalServerError, "failed to encode vector search request body: " + err.Error()
	}

	provider := p.config.VectorSearchProvider.AzureAISearch
	respBody, status, message := p.postAzureJSON(r, provider.Endpoint, provider.APIKey, rawBody, "vector search")
	if status != http.StatusOK {
		return "", status, message
	}
	return string(respBody), http.StatusOK, ""
}

func (p *Plugin) postAzureJSON(
	r *http.Request,
	endpoint string,
	apiKey string,
	body []byte,
	kind string,
) ([]byte, int, string) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, http.StatusInternalServerError, "failed to create " + kind + " request: " + err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("api-key", apiKey)

	client := p.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, http.StatusInternalServerError, "failed to request " + kind + ": " + err.Error()
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, http.StatusInternalServerError, "failed to read " + kind + " response body: " + err.Error()
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, string(respBody)
	}
	return respBody, http.StatusOK, ""
}

func appendSearchResult(r *http.Request, body map[string]any, searchResult string) {
	protocol, err := ai_protocols.Detect(r.URL.Path, body)
	if err != nil {
		return
	}
	ai_protocols.AppendMessages(protocol, body, []ai_protocols.Message{{Role: "user", Content: searchResult}})
}

func (p *Plugin) transport() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if p.config.SSLVerify != nil && !*p.config.SSLVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}
	return transport
}

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	if closeErr := r.Body.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, err
}

func writeJSONMessage(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"message":%q}`, message)
}
