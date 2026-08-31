package ai_rag

import (
	"bytes"
	"io"
	"net/http"
	"time"

	"github.com/wklken/apisix-go/pkg/httpclient"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/ai_common"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config
	client *http.Client
	ragCredentialState
}

const (
	priority               = 1060
	name                   = "ai-rag"
	providerRequestTimeout = 60 * time.Second
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
	if p.config.SSLVerify == nil {
		sslVerify := true
		p.config.SSLVerify = &sslVerify
	}
	// APISIX inherits 60-second OpenResty socket timeouts. Go uses one total
	// bound so a stalled provider cannot retain generation credentials forever.
	client := &http.Client{Transport: p.transport(), Timeout: providerRequestTimeout}
	return p.installRAGClient(client)
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		body, err := base.ReadRequestBody(r)
		if err != nil {
			writeRequestBodyError(w, "could not get body: "+err.Error())
			return
		}
		if len(bytes.TrimSpace(body)) == 0 {
			writeRequestBodyError(w, "could not get body: request body is empty")
			return
		}

		var bodyTab map[string]any
		if err := json.Unmarshal(body, &bodyTab); err != nil {
			writePlainResponse(w, http.StatusBadRequest, "could not parse JSON request body: "+err.Error())
			return
		}

		embeddingsReq, fields, diagnostic := parseAIRAG(bodyTab)
		if diagnostic != "" {
			logger.Error(diagnostic)
			writePlainResponse(w, http.StatusBadRequest, diagnostic)
			return
		}

		embedding, status, message := p.requestEmbeddings(r, embeddingsReq)
		if status != http.StatusOK {
			logger.Error("could not get embeddings: " + message)
			writePlainResponse(w, status, message)
			return
		}

		searchResult, status, message := p.requestVectorSearch(r, fields, embedding)
		if status != http.StatusOK {
			logger.Error("could not get vector_search result: " + message)
			writePlainResponse(w, status, message)
			return
		}

		delete(bodyTab, "ai_rag")
		appendSearchResult(r, bodyTab, searchResult)

		rewritten, err := json.Marshal(bodyTab)
		if err != nil {
			base.WriteJSONMessage(
				w,
				http.StatusInternalServerError,
				"failed to parse modified JSON request body: "+err.Error(),
			)
			return
		}

		base.ReplaceRequestBody(r, rewritten)

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func parseAIRAG(body map[string]any) (map[string]any, string, string) {
	aiRAG, ok := body["ai_rag"].(map[string]any)
	if !ok {
		return nil, "", `request body must have "ai-rag" field`
	}

	vectorSearch, ok := aiRAG["vector_search"].(map[string]any)
	if !ok {
		return nil, "", `request body fails schema check: property "ai_rag" validation failed: property "vector_search" is required`
	}
	fields, ok := vectorSearch["fields"].(string)
	if !ok || fields == "" {
		return nil, "", `request body fails schema check: property "ai_rag" validation failed: property "vector_search" validation failed: property "fields" is required`
	}

	embeddings, ok := aiRAG["embeddings"].(map[string]any)
	if !ok {
		return nil, "", `request body fails schema check: property "ai_rag" validation failed: property "embeddings" is required`
	}
	if _, ok := embeddings["input"]; !ok {
		return nil, "", `request body fails schema check: property "ai_rag" validation failed: property "embeddings" validation failed: property "input" is required`
	}

	return embeddings, fields, ""
}

func (p *Plugin) requestEmbeddings(r *http.Request, embeddingsReq map[string]any) (any, int, string) {
	rawBody, err := json.Marshal(embeddingsReq)
	if err != nil {
		return nil, http.StatusInternalServerError, "failed to encode embeddings request body: " + err.Error()
	}

	provider := p.config.EmbeddingsProvider.AzureOpenAI
	var (
		respBody []byte
		status   int
		message  string
	)
	err = p.withEmbeddingKey(func(apiKey string) error {
		respBody, status, message = p.postAzureJSON(
			r, provider.Endpoint, apiKey, rawBody, "embeddings",
		)
		return nil
	})
	if err != nil {
		return nil, http.StatusInternalServerError, errRAGCredentialsUnavailable.Error()
	}
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
	var (
		respBody []byte
		status   int
		message  string
	)
	err = p.withSearchKey(func(apiKey string) error {
		respBody, status, message = p.postAzureJSON(
			r, provider.Endpoint, apiKey, rawBody, "vector search",
		)
		return nil
	})
	if err != nil {
		return "", http.StatusInternalServerError, errRAGCredentialsUnavailable.Error()
	}
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
	apiKeyHeader := req.Header[http.CanonicalHeaderKey("api-key")]
	defer func() {
		for index := range apiKeyHeader {
			apiKeyHeader[index] = ""
		}
		req.Header.Del("api-key")
	}()

	client := p.client
	if client == nil {
		client = httpclient.New()
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, http.StatusInternalServerError, "failed to request " + kind + ": " + err.Error()
	}
	defer func() { _ = resp.Body.Close() }()

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
		protocol = ai_protocols.OpenAIChat
	}
	ai_protocols.AppendMessages(protocol, body, []ai_protocols.Message{{Role: "user", Content: searchResult}})
}

func writePlainResponse(w http.ResponseWriter, status int, body string) {
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func writeRequestBodyError(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = io.WriteString(w, util.BuildMessageResponse(message)+"\n")
}

func (p *Plugin) transport() http.RoundTripper {
	transport := httpclient.NewTransport()
	ai_common.ApplyTransportSSLVerify(transport, p.config.SSLVerify)
	return transport
}
