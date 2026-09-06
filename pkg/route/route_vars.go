package route

import (
	"bytes"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/expr"
)

func compileRouteVars(raw []byte) (*expr.Expression, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var rules []any
	if err := json.Unmarshal(raw, &rules); err != nil {
		return nil, err
	}
	if len(rules) == 0 {
		return nil, nil
	}
	return expr.Compile(rules)
}

func routeVariableValue(request *http.Request, name, pattern string, graphqlMaxSize int) any {
	name = strings.TrimPrefix(name, "$")
	// nginx distinguishes an absent request variable from an explicit empty value.
	switch {
	case name == "http_host":
		if request.Host == "" {
			return nil
		}
		return request.Host
	case strings.HasPrefix(name, "uri_param_"):
		values := strings.Split(request.URL.Path, "/")
		for i, part := range strings.Split(pattern, "/") {
			if part == ":"+strings.TrimPrefix(name, "uri_param_") && i < len(values) {
				return values[i]
			}
		}
		return nil
	case strings.HasPrefix(name, "post_arg_") || strings.HasPrefix(name, "post_arg.") || strings.HasPrefix(name, "graphql_"):
		return routeBodyVariables(request).value(request, name, graphqlMaxSize)
	case strings.HasPrefix(name, "arg_"):
		if !request.URL.Query().Has(strings.TrimPrefix(name, "arg_")) {
			return nil
		}
	case strings.HasPrefix(name, "http_"):
		if len(request.Header.Values(strings.ReplaceAll(strings.TrimPrefix(name, "http_"), "_", "-"))) == 0 {
			return nil
		}
	case strings.HasPrefix(name, "cookie_"):
		if _, err := request.Cookie(strings.TrimPrefix(name, "cookie_")); err != nil {
			return nil
		}
	}
	return expr.RequestValue(request, name)
}

// chi resolves a path before invoking its handler. When every candidate at that
// path fails vars, this immutable index resumes matching less specific paths.
// It is used only on misses in snapshots containing conditional routes.
type routeFallbackNode struct {
	literal   map[byte]*routeFallbackNode
	parameter *routeFallbackNode
	exact     *wildcardDispatcher
	wildcard  *wildcardDispatcher
}

func (node *routeFallbackNode) add(path string, dispatcher *wildcardDispatcher) {
	for len(path) > 0 {
		switch path[0] {
		case '*':
			node.wildcard = dispatcher
			return
		case '{':
			if node.parameter == nil {
				node.parameter = &routeFallbackNode{}
			}
			node = node.parameter
			path = path[strings.IndexByte(path, '}')+1:]
		default:
			if node.literal == nil {
				node.literal = make(map[byte]*routeFallbackNode)
			}
			child := node.literal[path[0]]
			if child == nil {
				child = &routeFallbackNode{}
				node.literal[path[0]] = child
			}
			node = child
			path = path[1:]
		}
	}
	node.exact = dispatcher
}

func (node *routeFallbackNode) match(
	path string,
	request *http.Request,
	skip *wildcardDispatcher,
) (wildcardRoute, bool) {
	if len(path) == 0 {
		if node.exact != nil && node.exact != skip {
			if route, ok := node.exact.selectRoute(request); ok {
				return route, true
			}
		}
	} else {
		if child := node.literal[path[0]]; child != nil {
			if route, ok := child.match(path[1:], request, skip); ok {
				return route, true
			}
		}
		if node.parameter != nil && path[0] != '/' {
			end := strings.IndexByte(path, '/')
			if end < 0 {
				end = len(path)
			}
			if route, ok := node.parameter.match(path[end:], request, skip); ok {
				return route, true
			}
		}
	}
	if node.wildcard != nil && node.wildcard != skip {
		if route, ok := node.wildcard.selectRoute(request); ok {
			return route, true
		}
	}
	return wildcardRoute{}, false
}

func setMatchedRouteParameters(request *http.Request, pattern string) {
	context := chi.RouteContext(request.Context())
	if context == nil {
		return
	}
	context.URLParams.Keys = context.URLParams.Keys[:0]
	context.URLParams.Values = context.URLParams.Values[:0]
	if wildcard := strings.IndexByte(pattern, '*'); wildcard >= 0 {
		context.URLParams.Add("*", request.URL.Path[wildcard:])
		return
	}
	if !strings.ContainsRune(pattern, ':') {
		return
	}
	values := strings.Split(request.URL.Path, "/")
	for i, part := range strings.Split(pattern, "/") {
		if strings.HasPrefix(part, ":") && i < len(values) {
			context.URLParams.Add(part[1:], values[i])
		}
	}
}
