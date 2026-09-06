package route

import (
	"cmp"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/expr"
	"github.com/wklken/apisix-go/pkg/resource"
)

var parameterInPathRegexp = regexp.MustCompile(`^:[A-Za-z_][A-Za-z0-9_]*$`)

var supportedRouteMethods = map[string]struct{}{
	http.MethodConnect: {},
	http.MethodDelete:  {},
	http.MethodGet:     {},
	http.MethodHead:    {},
	http.MethodOptions: {},
	http.MethodPatch:   {},
	http.MethodPost:    {},
	http.MethodPut:     {},
	"PURGE":            {},
	http.MethodTrace:   {},
}

// ConvertURI convert the apisix uri to chi compatible uri
// NOTE:
// 1. full path match: /blog/bar   same
// 2. prefix match: /blog/bar*     same
// 3. parameters in path: /blog/:name => /blog/{name} ok
// 4. embedded wildcard: /articles/*/comments => chi prefix wildcard plus an exact suffix guard
// 5. named terminal wildcards: /user/:user/*action => /user/{user}/*
func convertURI(uri string) (string, error) {
	if uri == "" || !strings.HasPrefix(uri, "/") || strings.ContainsAny(uri, "{}") {
		return "", fmt.Errorf("not supported uri: %s", uri)
	}

	withColon := strings.ContainsRune(uri, ':')
	withAsterisk := strings.ContainsRune(uri, '*')

	if !withColon && !withAsterisk {
		return uri, nil
	}

	if withColon {
		segments := strings.Split(uri, "/")
		names := make(map[string]struct{})
		for i, segment := range segments {
			if !strings.ContainsRune(segment, ':') {
				continue
			}
			if !parameterInPathRegexp.MatchString(segment) {
				return "", fmt.Errorf("not supported uri: %s", uri)
			}
			name := strings.TrimPrefix(segment, ":")
			if _, exists := names[name]; exists {
				return "", fmt.Errorf("not supported uri: %s", uri)
			}
			names[name] = struct{}{}
			segments[i] = "{" + name + "}"
		}
		uri = strings.Join(segments, "/")
		if !withAsterisk {
			return uri, nil
		}
	}

	if withAsterisk {
		if strings.Count(uri, "*") != 1 {
			return "", fmt.Errorf("not supported uri: %s", uri)
		}
		if strings.HasSuffix(uri, "*") {
			return uri, nil
		}
		wildcard := strings.IndexByte(uri, '*')
		if wildcard == 0 || uri[wildcard-1] != '/' {
			return "", fmt.Errorf("not supported uri: %s", uri)
		}
		name, suffix, _ := strings.Cut(uri[wildcard+1:], "/")
		if name != "" && !parameterInPathRegexp.MatchString(":"+name) || strings.ContainsRune(suffix, '{') {
			return "", fmt.Errorf("not supported uri: %s", uri)
		}
		return uri[:wildcard+1], nil
	}

	return "", fmt.Errorf("not supported uri: %s", uri)
}

// effectiveRouteURI returns the routing shape used by chi. Parameter names do
// not participate in matching, so routes such as /users/:id and
// /users/:name have the same effective URI and must not be registered twice
// within one APISIX route.
func effectiveRouteURI(converted string) string {
	segments := strings.Split(converted, "/")
	for index, segment := range segments {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			segments[index] = "{}"
		}
	}
	return strings.Join(segments, "/")
}

type routeRegistrar struct {
	mux                   *chi.Mux
	notFound              http.Handler
	dispatchers           map[string]*wildcardDispatcher
	nextRegistrationIndex uint64
	hasVars               bool
	graphqlMaxSize        int
	fallbackPaths         routeFallbackNode
}

func newRouteRegistrar(mux *chi.Mux, notFoundHandlers ...http.Handler) *routeRegistrar {
	registerPurgeMethod()
	notFound := apisixRouteNotFoundHandler()
	if len(notFoundHandlers) > 0 && notFoundHandlers[0] != nil {
		notFound = notFoundHandlers[0]
	}
	return &routeRegistrar{
		mux:         mux,
		notFound:    notFound,
		dispatchers: make(map[string]*wildcardDispatcher),
	}
}

type routeRegistrationOptions struct {
	vars     *expr.Expression
	priority int
}

func (r *routeRegistrar) registerRouteWithHosts(
	methods []string,
	uri string,
	hosts []string,
	handler http.Handler,
	options ...routeRegistrationOptions,
) error {
	converted, err := convertURI(uri)
	if err != nil {
		return err
	}
	registrationIndex := r.nextRegistrationIndex
	r.nextRegistrationIndex++
	var option routeRegistrationOptions
	if len(options) > 0 {
		option = options[0]
	}
	r.hasVars = r.hasVars || option.vars != nil
	r.registerWildcardRoute(methods, converted, uri, hosts, handler, registrationIndex, option)
	return nil
}

type wildcardRoute struct {
	method            string
	pattern           string
	embedded          bool
	hosts             []string
	handler           http.Handler
	registrationIndex uint64
	priority          int
	vars              *expr.Expression
	graphqlMaxSize    int
}

type wildcardDispatcher struct {
	prefix      string
	notFound    http.Handler
	nonEmbedded *routeDecisionIndex
	embedded    map[string]*routeDecisionIndex
	registrar   *routeRegistrar
}

type routeCandidate struct {
	route    wildcardRoute
	valid    bool
	previous *routeCandidate
}

type routeHostDecision struct {
	exact    map[string]routeCandidate
	wildcard routeCandidate
}

type routeDecisionIndex struct {
	pattern       string
	hostless      routeHostDecision
	exactHosts    map[string]*routeHostDecision
	wildcardHosts *wildcardHostIndex
}

type wildcardHostIndex struct {
	root wildcardHostNode
}

type wildcardHostNode struct {
	children map[byte]*wildcardHostNode
	decision *routeHostDecision
	parent   *wildcardHostNode
}

func (index *wildcardHostIndex) ensure(suffix string) *routeHostDecision {
	node := &index.root
	for position := len(suffix) - 1; position >= 0; position-- {
		if node.children == nil {
			node.children = make(map[byte]*wildcardHostNode)
		}
		child := node.children[suffix[position]]
		if child == nil {
			child = &wildcardHostNode{parent: node}
			node.children[suffix[position]] = child
		}
		node = child
	}
	if node.decision == nil {
		node.decision = &routeHostDecision{}
	}
	return node.decision
}

func (index *wildcardHostIndex) exact(suffix string) *routeHostDecision {
	if index == nil {
		return nil
	}
	node := &index.root
	for position := len(suffix) - 1; position >= 0; position-- {
		node = node.children[suffix[position]]
		if node == nil {
			return nil
		}
	}
	return node.decision
}

func (index *wildcardHostIndex) visitMatches(host string, visit func(*routeHostDecision) bool) {
	if index == nil {
		return
	}
	node := &index.root
	for position := len(host) - 1; position >= 0; position-- {
		child := node.children[host[position]]
		if child == nil {
			break
		}
		node = child
	}
	for node != &index.root {
		if node.decision != nil && !visit(node.decision) {
			return
		}
		node = node.parent
	}
}

func (d *routeDecisionIndex) add(route wildcardRoute) {
	if len(route.hosts) == 0 {
		d.hostless.add(route)
		return
	}
	for _, host := range route.hosts {
		host = normalizeRouteHost(host)
		if strings.ContainsAny(host, "*?[") {
			suffix, ok := wildcardRouteHostKey(host)
			if !ok {
				continue
			}
			if d.wildcardHosts == nil {
				d.wildcardHosts = &wildcardHostIndex{}
			}
			decision := d.wildcardHosts.ensure(suffix)
			decision.add(route)
			continue
		}
		if d.exactHosts == nil {
			d.exactHosts = make(map[string]*routeHostDecision)
		}
		decision := d.exactHosts[host]
		if decision == nil {
			decision = &routeHostDecision{}
			d.exactHosts[host] = decision
		}
		decision.add(route)
	}
}

func (d *routeHostDecision) add(route wildcardRoute) {
	if route.method == "*" {
		d.wildcard = insertRouteCandidate(d.wildcard, route)
		return
	}
	if d.exact == nil {
		d.exact = make(map[string]routeCandidate)
	}
	d.exact[route.method] = insertRouteCandidate(d.exact[route.method], route)
}

func higherRoutePrecedence(candidate, current wildcardRoute) bool {
	if candidate.priority != current.priority {
		return candidate.priority > current.priority
	}
	if candidate.embedded && current.embedded && len(candidate.pattern) != len(current.pattern) {
		return len(candidate.pattern) > len(current.pattern)
	}
	return candidate.registrationIndex > current.registrationIndex
}

func insertRouteCandidate(current routeCandidate, route wildcardRoute) routeCandidate {
	if !current.valid || higherRoutePrecedence(route, current.route) {
		candidate := routeCandidate{route: route, valid: true}
		// Only conditional winners need to retain lower-priority alternatives.
		if route.vars != nil && current.valid {
			candidate.previous = &current
		}
		return candidate
	}
	if current.route.vars != nil {
		var previous routeCandidate
		if current.previous != nil {
			previous = *current.previous
		}
		previous = insertRouteCandidate(previous, route)
		current.previous = &previous
	}
	return current
}

func (candidate routeCandidate) match(request *http.Request) routeCandidate {
	for candidate.valid {
		if candidate.route.vars == nil ||
			candidate.route.vars.Eval(func(name string) any {
				return routeVariableValue(request, name, candidate.route.pattern, candidate.route.graphqlMaxSize)
			}) {
			return candidate
		}
		if candidate.previous == nil {
			break
		}
		candidate = *candidate.previous
	}
	return routeCandidate{}
}

func (d *routeDecisionIndex) lookup(
	host string,
	wildcardHost string,
	hostRank int,
	methodIndex int,
	request *http.Request,
) (routeCandidate, bool, bool) {
	if hostRank == 1 {
		var selected routeCandidate
		hasRoutes := false
		d.wildcardHosts.visitMatches(host, func(decision *routeHostDecision) bool {
			if !decision.hasRoutes() {
				return true
			}
			hasRoutes = true
			if methodIndex == 1 {
				selected = decision.wildcard.match(request)
				return !selected.valid
			}
			selected = decision.exact[request.Method].match(request)
			return !selected.valid
		})
		return selected, hasRoutes, selected.valid
	}
	decision := d.hostDecision(host, wildcardHost, hostRank)
	if decision == nil {
		return routeCandidate{}, false, false
	}
	if !decision.hasRoutes() {
		return routeCandidate{}, false, false
	}
	if methodIndex == 1 {
		candidate := decision.wildcard.match(request)
		return candidate, true, candidate.valid
	}
	candidate := decision.exact[request.Method].match(request)
	return candidate, true, candidate.valid
}

func (d *routeHostDecision) hasRoutes() bool {
	return d.wildcard.valid || len(d.exact) > 0
}

func (d *routeDecisionIndex) hostDecision(
	host string,
	wildcardHost string,
	hostRank int,
) *routeHostDecision {
	switch hostRank {
	case 2:
		return d.exactHosts[host]
	case 1:
		if wildcardHost == "" {
			return nil
		}
		return d.wildcardHosts.exact(wildcardHost)
	default:
		return &d.hostless
	}
}

func normalizeRouteHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

func wildcardRouteHostKey(pattern string) (string, bool) {
	if !strings.HasPrefix(pattern, "*.") {
		return "", false
	}
	suffix := pattern[2:]
	if suffix == "" || strings.ContainsAny(suffix, "*?[") {
		return "", false
	}
	return pattern[1:], true
}

func validateRouteHost(host string) error {
	normalized := normalizeRouteHost(strings.TrimSpace(host))
	if normalized == "" {
		return fmt.Errorf("must not be empty")
	}
	if strings.ContainsAny(normalized, "*?[") {
		if _, ok := wildcardRouteHostKey(normalized); !ok {
			return fmt.Errorf("wildcard must be *.suffix")
		}
	}
	return nil
}

func wildcardHostKey(host string) string {
	dot := strings.IndexByte(host, '.')
	if dot <= 0 || dot == len(host)-1 {
		return ""
	}
	return host[dot:]
}

func (r *routeRegistrar) registerWildcardRoute(
	methods []string,
	converted string,
	pattern string,
	hosts []string,
	handler http.Handler,
	registrationIndex uint64,
	option routeRegistrationOptions,
) {
	identity := effectiveRouteURI(converted)
	dispatcher := r.dispatchers[identity]
	if dispatcher == nil {
		dispatcher = &wildcardDispatcher{
			prefix:    strings.SplitN(pattern, "*", 2)[0],
			notFound:  r.notFound,
			registrar: r,
			embedded:  make(map[string]*routeDecisionIndex),
		}
		r.mux.Handle(converted, dispatcher)
		r.dispatchers[identity] = dispatcher
	}

	_, embedded := embeddedWildcardSuffix(pattern)
	r.hasVars = r.hasVars || embedded
	if len(methods) == 0 {
		dispatcher.add(wildcardRoute{
			method:            "*",
			pattern:           pattern,
			embedded:          embedded,
			hosts:             hosts,
			handler:           handler,
			registrationIndex: registrationIndex,
			vars:              option.vars,
			priority:          option.priority,
			graphqlMaxSize:    r.graphqlMaxSize,
		})
		return
	}
	for _, method := range methods {
		logger.Debugf("add route: %s %s", method, converted)
		dispatcher.add(wildcardRoute{
			method:            strings.ToUpper(method),
			pattern:           pattern,
			embedded:          embedded,
			hosts:             hosts,
			handler:           handler,
			registrationIndex: registrationIndex,
			vars:              option.vars,
			priority:          option.priority,
			graphqlMaxSize:    r.graphqlMaxSize,
		})
	}
}

func (d *wildcardDispatcher) add(route wildcardRoute) {
	if d.embedded == nil {
		d.embedded = make(map[string]*routeDecisionIndex)
	}
	if route.embedded {
		suffix, _ := embeddedWildcardSuffix(route.pattern)
		decision := d.embedded[suffix]
		if decision == nil {
			decision = &routeDecisionIndex{pattern: route.pattern}
			d.embedded[suffix] = decision
		}
		decision.add(route)
		return
	}
	if d.nonEmbedded == nil {
		d.nonEmbedded = &routeDecisionIndex{pattern: route.pattern}
	}
	d.nonEmbedded.add(route)
}

func (d *wildcardDispatcher) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	route, matched := d.selectRoute(request)
	if !matched && d.registrar != nil && d.registrar.hasVars {
		route, matched = d.registrar.fallbackPaths.match(request.URL.Path, request, d)
	}
	if matched {
		serveMatchedRoute(writer, request, route, requestHostname(request.Host))
		return
	}
	d.notFound.ServeHTTP(writer, request)
}

func (d *wildcardDispatcher) selectRoute(request *http.Request) (wildcardRoute, bool) {
	host := requestHostname(request.Host)
	wildcardHost := wildcardHostKey(host)
	nonEmbeddedPathMatched := d.nonEmbedded != nil &&
		matchesRoutePath(d.nonEmbedded.pattern, request.URL.Path)
	for embeddedIndex := range 2 {
		for _, hostRank := range []int{2, 1, 0} {
			for methodIndex := range 2 {
				if embeddedIndex == 0 {
					if len(d.embedded) == 0 {
						continue
					}
					route, matched, _, _ := d.matchEmbeddedRoute(
						request,
						host,
						wildcardHost,
						hostRank,
						methodIndex,
					)
					if matched {
						return route, true
					}
					continue
				}
				route, matched, _, _ := d.matchNonEmbeddedRoute(
					request,
					host,
					wildcardHost,
					hostRank,
					methodIndex,
					nonEmbeddedPathMatched,
				)
				if matched {
					return route, true
				}
			}
		}
	}
	return wildcardRoute{}, false
}

func serveMatchedRoute(
	writer http.ResponseWriter,
	request *http.Request,
	route wildcardRoute,
	requestHost string,
) {
	setMatchedRouteParameters(request, route.pattern)
	route.handler.ServeHTTP(
		writer,
		apisixctx.WithMatchedRoute(request, route.pattern, matchedRouteHost(route.hosts, requestHost)),
	)
}

func matchedRouteHost(hosts []string, requestHost string) string {
	for _, host := range hosts {
		normalized := normalizeRouteHost(host)
		if normalized == requestHost {
			return host
		}
	}
	for _, host := range hosts {
		if matchOneLabelHostWildcard(normalizeRouteHost(host), requestHost) {
			return host
		}
	}
	return ""
}

func (d *wildcardDispatcher) matchEmbeddedRoute(
	request *http.Request,
	host string,
	wildcardHost string,
	hostRank int,
	methodIndex int,
) (wildcardRoute, bool, bool, bool) {
	requestPath := request.URL.Path
	prefixLength, prefixMatches := matchRoutePrefix(d.prefix, requestPath)
	if !prefixMatches {
		return wildcardRoute{}, false, false, false
	}

	bestFound := false
	var bestRoute wildcardRoute
	pathMatched := false
	hostMatched := false
	for searchFrom := prefixLength; searchFrom < len(requestPath); {
		relativeSlash := strings.IndexByte(requestPath[searchFrom:], '/')
		if relativeSlash < 0 {
			break
		}
		suffixStart := searchFrom + relativeSlash
		suffix := requestPath[suffixStart:]
		decision := d.embedded[suffix]
		if decision != nil && len(requestPath) >= prefixLength+len(suffix) {
			pathMatched = true
			candidate, matchedHost, ok := decision.lookup(
				host,
				wildcardHost,
				hostRank,
				methodIndex,
				request,
			)
			hostMatched = hostMatched || matchedHost
			if ok && (!bestFound || higherRoutePrecedence(candidate.route, bestRoute)) {
				bestFound = true
				bestRoute = candidate.route
			}
		}
		searchFrom = suffixStart + 1
	}
	if !bestFound {
		return wildcardRoute{}, false, pathMatched, hostMatched
	}
	return bestRoute, true, pathMatched, hostMatched
}

func (d *wildcardDispatcher) matchNonEmbeddedRoute(
	request *http.Request,
	host string,
	wildcardHost string,
	hostRank int,
	methodIndex int,
	pathMatched bool,
) (wildcardRoute, bool, bool, bool) {
	if d.nonEmbedded == nil || !pathMatched {
		return wildcardRoute{}, false, false, false
	}
	candidate, matchedHost, ok := d.nonEmbedded.lookup(
		host,
		wildcardHost,
		hostRank,
		methodIndex,
		request,
	)
	if !ok {
		return wildcardRoute{}, false, true, matchedHost
	}
	return candidate.route, true, true, matchedHost
}

func matchesRoutePath(pattern string, requestPath string) bool {
	if strings.ContainsRune(pattern, ':') {
		return matchesParameterizedRoute(pattern, requestPath)
	}
	if !strings.ContainsRune(pattern, '*') {
		return pattern == requestPath
	}
	return matchesWildcardRoute(pattern, requestPath)
}

func matchesParameterizedRoute(pattern, requestPath string) bool {
	if strings.ContainsRune(pattern, '*') {
		return matchesWildcardRoute(pattern, requestPath)
	}
	patternParts := strings.Split(pattern, "/")
	requestParts := strings.Split(requestPath, "/")
	for i := range patternParts {
		if i >= len(requestParts) {
			return false
		}
		if strings.HasPrefix(patternParts[i], ":") {
			if len(patternParts[i]) == 1 || requestParts[i] == "" {
				return false
			}
			continue
		}
		if patternParts[i] != requestParts[i] {
			return false
		}
	}
	return len(patternParts) == len(requestParts)
}

func routeHostRank(patterns []string, requestHost string) int {
	if len(patterns) == 0 {
		return 0
	}
	host := requestHostname(requestHost)
	best := -1
	for _, pattern := range patterns {
		pattern = strings.ToLower(strings.TrimSuffix(pattern, "."))
		if pattern == host {
			return 2
		}
		if matchOneLabelHostWildcard(pattern, host) {
			best = 1
		}
	}
	return best
}

func requestHostname(requestHost string) string {
	host := requestHost
	if parsedHost, _, err := net.SplitHostPort(requestHost); err == nil {
		host = parsedHost
	} else {
		host = strings.Trim(host, "[]")
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

func matchOneLabelHostWildcard(pattern, host string) bool {
	if !strings.HasPrefix(pattern, "*.") {
		return false
	}
	suffix := pattern[1:]
	return strings.HasSuffix(host, suffix)
}

func embeddedWildcardSuffix(pattern string) (string, bool) {
	wildcard := strings.IndexByte(pattern, '*')
	if wildcard < 0 {
		return "", false
	}
	slash := strings.IndexByte(pattern[wildcard+1:], '/')
	if slash < 0 {
		return "", false
	}
	return pattern[wildcard+1+slash:], true
}

// Resolve a literal/parameter prefix against the actual path so wildcard offsets
// do not depend on the lengths of parameter names in the configured route.
func matchRoutePrefix(prefix, path string) (int, bool) {
	offset := 0
	for i := 0; i < len(prefix); {
		if prefix[i] == ':' && (i == 0 || prefix[i-1] == '/') {
			end := strings.IndexByte(prefix[i:], '/')
			if end < 0 {
				end = len(prefix) - i
			}
			i += end
			if offset >= len(path) || path[offset] == '/' {
				return 0, false
			}
			valueEnd := strings.IndexByte(path[offset:], '/')
			if valueEnd < 0 {
				valueEnd = len(path) - offset
			}
			offset += valueEnd
			continue
		}
		if offset >= len(path) || prefix[i] != path[offset] {
			return 0, false
		}
		i++
		offset++
	}
	return offset, true
}

func matchesWildcardRoute(pattern string, path string) bool {
	wildcard := strings.IndexByte(pattern, '*')
	prefixLength, ok := matchRoutePrefix(pattern[:wildcard], path)
	if !ok {
		return false
	}
	suffix, _ := embeddedWildcardSuffix(pattern)
	return strings.HasSuffix(path, suffix) && len(path) >= prefixLength+len(suffix)
}

func normalizeRouteOrder(routes []resource.Route) []resource.Route {
	normalized := append([]resource.Route(nil), routes...)
	slices.SortStableFunc(normalized, func(left, right resource.Route) int {
		return cmp.Compare(left.Priority, right.Priority)
	})
	return normalized
}

func pinDecodedRoutePath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// APISIX matches the decoded $uri; chi prefers the encoded RawPath.
		if r.URL.RawPath != "" {
			if rctx := chi.RouteContext(r.Context()); rctx != nil {
				rctx.RoutePath = r.URL.Path
			}
		}
		next.ServeHTTP(w, r)
	})
}
