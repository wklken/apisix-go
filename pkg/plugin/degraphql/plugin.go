package degraphql

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

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
	return validateGraphQLQuery(p.config.Query, p.config.OperationName)
}

func validateGraphQLQuery(query string, operationName string) error {
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("GraphQL query must not be empty")
	}
	if len(query) > 1024 {
		return fmt.Errorf("GraphQL query exceeds 1024 bytes")
	}

	braceDepth := 0
	parenDepth := 0
	bracketDepth := 0
	operationCount := 0
	pendingDefinition := ""
	inString := false
	inBlockString := false
	inComment := false
	escaped := false

	for i := 0; i < len(query); {
		if inComment {
			if query[i] == '\n' || query[i] == '\r' {
				inComment = false
			}
			i++
			continue
		}
		if inBlockString {
			if strings.HasPrefix(query[i:], `"""`) {
				inBlockString = false
				i += 3
				continue
			}
			i++
			continue
		}
		if inString {
			if escaped {
				escaped = false
				i++
				continue
			}
			switch query[i] {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			i++
			continue
		}

		if query[i] == '#' {
			inComment = true
			i++
			continue
		}
		if strings.HasPrefix(query[i:], `"""`) {
			inBlockString = true
			i += 3
			continue
		}
		if query[i] == '"' {
			inString = true
			i++
			continue
		}

		switch query[i] {
		case '{':
			if braceDepth == 0 && parenDepth == 0 && bracketDepth == 0 {
				if pendingDefinition != "operation" {
					operationCount++
				}
				pendingDefinition = ""
			}
			braceDepth++
		case '}':
			braceDepth--
			if braceDepth < 0 {
				return fmt.Errorf("invalid GraphQL query: unexpected }")
			}
		case '(':
			parenDepth++
		case ')':
			parenDepth--
			if parenDepth < 0 {
				return fmt.Errorf("invalid GraphQL query: unexpected )")
			}
		case '[':
			bracketDepth++
		case ']':
			bracketDepth--
			if bracketDepth < 0 {
				return fmt.Errorf("invalid GraphQL query: unexpected ]")
			}
		default:
			if isGraphQLNameStart(query[i]) {
				start := i
				i++
				for i < len(query) && isGraphQLNamePart(query[i]) {
					i++
				}
				if braceDepth == 0 && parenDepth == 0 && bracketDepth == 0 {
					definitionName := query[start:i]
					switch definitionName {
					case "query", "mutation", "subscription":
						operationCount++
						pendingDefinition = "operation"
					case "fragment", "schema", "directive":
						pendingDefinition = "non-operation"
					default:
						if operationCount == 0 && pendingDefinition == "" {
							return fmt.Errorf("invalid GraphQL query: unexpected top-level name %q", definitionName)
						}
					}
				}
				continue
			}
		}
		i++
	}

	if inString || inBlockString || braceDepth != 0 || parenDepth != 0 || bracketDepth != 0 ||
		pendingDefinition == "operation" {
		return fmt.Errorf("invalid GraphQL query: unterminated selection or literal")
	}
	if operationCount == 0 {
		return fmt.Errorf("invalid GraphQL query: no operation found")
	}
	if operationCount > 1 && strings.TrimSpace(operationName) == "" {
		return fmt.Errorf("operation_name is required when query contains multiple operations")
	}
	return nil
}

func isGraphQLNameStart(char byte) bool {
	return char == '_' || char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z'
}

func isGraphQLNamePart(char byte) bool {
	return isGraphQLNameStart(char) || char >= '0' && char <= '9'
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
		raw, err := base.ReadRequestBody(r)
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
