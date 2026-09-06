package route

import (
	"bytes"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/expr"
	"github.com/wklken/apisix-go/pkg/resource"
)

func compileRouteConditions(route resource.Route) (*expr.Expression, error) {
	raw := bytes.TrimSpace(route.Vars)
	var conditions []any
	if len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		var rules []any
		if err := json.Unmarshal(raw, &rules); err != nil {
			return nil, fmt.Errorf("vars: %w", err)
		}
		if len(rules) > 0 {
			conditions = append(conditions, rules)
		}
	}
	addresses := slices.Clone(route.RemoteAddrs)
	if route.RemoteAddrConfigured() {
		if addresses != nil {
			return nil, fmt.Errorf("remote_addr and remote_addrs cannot both be configured")
		}
		addresses = []string{route.RemoteAddr}
	}
	if addresses != nil {
		if len(addresses) == 0 {
			// A standalone empty IP list matches no address in resty.ipmatcher.
			conditions = append(conditions, []any{"remote_addr", "in", []any{}})
		} else {
			ipCondition := []any{"remote_addr", "ipmatch", addresses}
			if _, err := expr.Compile([]any{ipCondition}); err != nil {
				return nil, fmt.Errorf("remote_addr/remote_addrs: %w", err)
			}
			conditions = append(conditions, ipCondition)
		}
	}
	if len(conditions) == 0 {
		return nil, nil
	}
	compiled, err := expr.Compile(conditions)
	if err != nil {
		return nil, fmt.Errorf("vars: %w", err)
	}
	return compiled, nil
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
		var value any
		visitRouteParameters(pattern, request.URL.Path, func(parameter, matched string) {
			if parameter == strings.TrimPrefix(name, "uri_param_") {
				value = matched
			}
		})
		return value
	case strings.HasPrefix(name, "post_arg_") || strings.HasPrefix(name, "post_arg.") || strings.HasPrefix(name, "graphql_"):
		return routeBodyVariables(request).value(request, name, graphqlMaxSize)
	case strings.HasPrefix(name, "arg_"):
		value, exists := expr.QueryArgument(request.URL.RawQuery, strings.TrimPrefix(name, "arg_"))
		if !exists {
			return nil
		}
		return value
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
	visitRouteParameters(pattern, request.URL.Path, func(name, value string) { context.URLParams.Add(name, value) })
}

func visitRouteParameters(pattern, requestPath string, visit func(string, string)) {
	values := strings.Split(requestPath, "/")
	offset := 0
	for i, part := range strings.Split(pattern, "/") {
		if i >= len(values) {
			return
		}
		if strings.HasPrefix(part, ":") {
			visit(part[1:], values[i])
		}
		if wildcard := strings.IndexByte(part, '*'); wildcard >= 0 {
			start := offset + wildcard
			if start > len(requestPath) {
				return
			}
			suffix, _ := embeddedWildcardSuffix(pattern)
			end := len(requestPath) - len(suffix)
			if end < start {
				return
			}
			name := part[wildcard+1:]
			if name == "" {
				visit("*", requestPath[start:end])
				name = ":ext"
			}
			visit(name, requestPath[start:end])
			return
		}
		offset += len(values[i]) + 1
	}
}
