package route

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"github.com/ohler55/ojg/jp"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/graphql"
)

type routeBodyVarsKey struct{}

type routeBodyVars struct {
	body       []byte
	bodyRead   bool
	bodyErr    error
	parsed     any
	parsedRead bool
	graphql    map[string]any
}

func routeBodyVariables(request *http.Request) *routeBodyVars {
	if state, ok := request.Context().Value(routeBodyVarsKey{}).(*routeBodyVars); ok {
		return state
	}
	state := &routeBodyVars{}
	*request = *request.WithContext(context.WithValue(request.Context(), routeBodyVarsKey{}, state))
	return state
}

func (state *routeBodyVars) value(request *http.Request, name string, graphqlMaxSize int) any {
	if strings.HasPrefix(name, "graphql_") {
		if state.graphql == nil {
			state.graphql = state.parseGraphQL(request, graphqlMaxSize)
		}
		return state.graphql[strings.TrimPrefix(name, "graphql_")]
	}
	if strings.HasPrefix(name, "post_arg_") {
		if !strings.HasPrefix(request.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			return nil
		}
		values, _ := state.parseBody(request).(map[string]any)
		value := values[strings.TrimPrefix(name, "post_arg_")]
		if repeated, ok := value.([]any); ok && len(repeated) > 0 {
			return repeated[0]
		}
		return value
	}
	path := strings.TrimPrefix(name, "post_arg.")
	value := state.parseBody(request)
	if strings.ContainsAny(path, "[*") || strings.Contains(path, "..") {
		query, err := jp.ParseString("$." + path)
		if err != nil {
			return nil
		}
		values := query.Get(value)
		if len(values) == 0 {
			return nil
		}
		return values
	}
	for part := range strings.SplitSeq(path, ".") {
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = object[part]
	}
	return value
}

func (state *routeBodyVars) readBody(request *http.Request) ([]byte, error) {
	if !state.bodyRead {
		state.body, state.bodyErr = base.ReadRequestBody(request)
		state.bodyRead = true
	}
	return state.body, state.bodyErr
}

func (state *routeBodyVars) parseBody(request *http.Request) any {
	if state.parsedRead {
		return state.parsed
	}
	state.parsedRead = true
	body, err := state.readBody(request)
	if err != nil {
		return nil
	}
	contentType := request.Header.Get("Content-Type")
	switch {
	case strings.Contains(contentType, "application/json"):
		if err := json.Unmarshal(body, &state.parsed); err != nil {
			state.parsed = nil
		}
	case strings.Contains(contentType, "application/x-www-form-urlencoded"):
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return nil
		}
		state.parsed = routeFormValues(values)
	case strings.Contains(contentType, "multipart/form-data"):
		_, params, err := mime.ParseMediaType(contentType)
		if err != nil || params["boundary"] == "" {
			return nil
		}
		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		values := make(url.Values)
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil
			}
			value, err := io.ReadAll(part)
			if err != nil {
				return nil
			}
			values.Add(part.FormName(), string(value))
		}
		state.parsed = routeFormValues(values)
	}
	return state.parsed
}

func routeFormValues(values url.Values) map[string]any {
	result := make(map[string]any, len(values))
	for name, items := range values {
		if len(items) == 1 {
			result[name] = items[0]
		} else {
			repeated := make([]any, len(items))
			for i, value := range items {
				repeated[i] = value
			}
			result[name] = repeated
		}
	}
	return result
}

type routeReplayBody struct {
	io.Reader
	io.Closer
}

func (state *routeBodyVars) graphQLBody(request *http.Request, limit int) ([]byte, error) {
	if limit <= 0 {
		limit = base.DefaultRequestBodyMaxBytes
	}
	if state.bodyRead {
		if len(state.body) > limit {
			return nil, &base.BodyTooLargeError{Limit: int64(limit)}
		}
		return state.body, state.bodyErr
	}
	if request.Body == nil || request.Body == http.NoBody {
		return nil, nil
	}
	original := request.Body
	body, err := io.ReadAll(io.LimitReader(original, int64(limit)+1))
	// A rejected GraphQL candidate must preserve the complete body for fallback
	// routes, including unread bytes beyond graphql.max_size.
	request.Body = &routeReplayBody{Reader: io.MultiReader(bytes.NewReader(body), original), Closer: original}
	if len(body) > limit {
		return nil, &base.BodyTooLargeError{Limit: int64(limit)}
	}
	state.body, state.bodyErr, state.bodyRead = body, err, true
	return body, err
}

func (state *routeBodyVars) parseGraphQL(request *http.Request, limit int) map[string]any {
	result := make(map[string]any)
	var query string
	switch request.Method {
	case http.MethodGet:
		query = request.URL.Query().Get("query")
	case http.MethodPost:
		body, err := state.graphQLBody(request, limit)
		if err != nil {
			return result
		}
		if request.Header.Get("Content-Type") == "application/json" {
			var document struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal(body, &document); err != nil {
				return result
			}
			query = document.Query
		} else {
			query = string(body)
		}
	default:
		return result
	}
	document, err := graphql.Parse(query)
	if err != nil || len(document.Operations) == 0 {
		return result
	}
	operation := document.Operations[0]
	fields := make([]any, 0, len(operation.SelectionSet))
	for _, selection := range operation.SelectionSet {
		if field, ok := selection.(*ast.Field); ok {
			fields = append(fields, field.Name)
		}
	}
	result["name"] = operation.Name
	result["operation"] = string(operation.Operation)
	result["root_fields"] = fields
	return result
}
