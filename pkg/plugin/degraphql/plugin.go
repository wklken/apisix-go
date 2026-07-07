package degraphql

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	priority = 509
	name     = "degraphql"
)

const schema = `
{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "minLength": 1,
      "maxLength": 1024
    },
    "variables": {
      "type": "array",
      "items": {
        "type": "string"
      },
      "minItems": 1
    },
    "operation_name": {
      "type": "string",
      "minLength": 1,
      "maxLength": 1024
    }
  },
  "required": ["query"]
}
`

type Config struct {
	Query         string   `json:"query"`
	Variables     []string `json:"variables,omitempty"`
	OperationName string   `json:"operation_name,omitempty"`
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
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			if err := p.rewritePOST(r); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		case http.MethodGet:
			p.rewriteGET(r)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) rewritePOST(r *http.Request) error {
	var body map[string]any
	if len(p.config.Variables) > 0 {
		raw, err := readBody(r)
		if err != nil {
			return err
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			return fmt.Errorf("missing request body")
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return fmt.Errorf("invalid request body: %w", err)
		}
	}

	payload := map[string]any{
		"query": p.config.Query,
	}
	if p.config.OperationName != "" {
		payload["operationName"] = p.config.OperationName
	}
	if len(p.config.Variables) > 0 {
		payload["variables"] = p.pickVariables(body)
	}

	rewritten, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	r.Body = io.NopCloser(bytes.NewReader(rewritten))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(rewritten)), nil
	}
	r.ContentLength = int64(len(rewritten))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Content-Length", fmt.Sprint(len(rewritten)))
	return nil
}

func (p *Plugin) rewriteGET(r *http.Request) {
	args := url.Values{}
	args.Set("query", p.config.Query)
	if p.config.OperationName != "" {
		args.Set("operationName", p.config.OperationName)
	}
	if len(p.config.Variables) > 0 {
		variables := map[string]string{}
		source := r.URL.Query()
		for _, name := range p.config.Variables {
			variables[name] = source.Get(name)
		}
		encoded, err := json.Marshal(variables)
		if err == nil {
			args.Set("variables", string(encoded))
		}
	}
	r.URL.RawQuery = args.Encode()
}

func (p *Plugin) pickVariables(body map[string]any) map[string]any {
	variables := make(map[string]any, len(p.config.Variables))
	for _, name := range p.config.Variables {
		variables[name] = body[name]
	}
	return variables
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
