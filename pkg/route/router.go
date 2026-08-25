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
	"github.com/wklken/apisix-go/pkg/logger"
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
// FIXME:
//
//	https://github.com/api7/lua-resty-radixtree/#parameters-in-path
//	5. not supported yet:
//	   - /user/:user/*action
//	   this will match `/user/john/` and also `/user/john/send`
//	   - /user/*action
func convertURI(uri string) (string, error) {
	if uri == "" || !strings.HasPrefix(uri, "/") || strings.ContainsAny(uri, "{}") {
		return "", fmt.Errorf("not supported uri: %s", uri)
	}

	withColon := strings.ContainsRune(uri, ':')
	withAsterisk := strings.ContainsRune(uri, '*')

	if !withColon && !withAsterisk {
		return uri, nil
	}

	if withColon && !withAsterisk {
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
		return strings.Join(segments, "/"), nil
	}

	if !withColon && withAsterisk {
		if strings.Count(uri, "*") != 1 {
			return "", fmt.Errorf("not supported uri: %s", uri)
		}
		if strings.HasSuffix(uri, "*") {
			return uri, nil
		}
		if !strings.Contains(uri, "/*/") {
			return "", fmt.Errorf("not supported uri: %s", uri)
		}
		return uri[:strings.IndexByte(uri, '*')+1], nil
	}

	if withColon && withAsterisk {
		// not supported yet
		return "", fmt.Errorf("not supported uri: %s", uri)
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
	dispatchers           map[string]*wildcardDispatcher
	nextRegistrationIndex uint64
}

func newRouteRegistrar(mux *chi.Mux) *routeRegistrar {
	registerPurgeMethod()
	return &routeRegistrar{
		mux:         mux,
		dispatchers: make(map[string]*wildcardDispatcher),
	}
}

func (r *routeRegistrar) registerRouteWithHosts(
	methods []string,
	uri string,
	hosts []string,
	handler http.Handler,
) error {
	converted, err := convertURI(uri)
	if err != nil {
		return err
	}
	registrationIndex := r.nextRegistrationIndex
	r.nextRegistrationIndex++
	if strings.ContainsRune(uri, '*') || len(hosts) > 0 || !strings.ContainsRune(uri, ':') {
		r.registerWildcardRoute(methods, converted, uri, hosts, handler, registrationIndex)
		return nil
	}
	if len(methods) == 0 {
		r.mux.Handle(converted, handler)
		return nil
	}
	for _, method := range methods {
		logger.Debugf("add route: %s %s", method, converted)
		r.mux.Method(method, converted, handler)
	}
	return nil
}

type wildcardRoute struct {
	method            string
	pattern           string
	embedded          bool
	hosts             []string
	handler           http.Handler
	registrationIndex uint64
}

type wildcardDispatcher struct {
	prefix      string
	nonEmbedded *routeDecisionIndex
	embedded    map[string]*routeDecisionIndex
}

type routeCandidate struct {
	route wildcardRoute
	valid bool
}

type routeHostDecision struct {
	exact    map[string]routeCandidate
	wildcard routeCandidate
	allowed  []string
}

type routeDecisionIndex struct {
	pattern       string
	hostless      routeHostDecision
	exactHosts    map[string]*routeHostDecision
	wildcardHosts map[string]*routeHostDecision
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
				d.wildcardHosts = make(map[string]*routeHostDecision)
			}
			decision := d.wildcardHosts[suffix]
			if decision == nil {
				decision = &routeHostDecision{}
				d.wildcardHosts[suffix] = decision
			}
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
	candidate := routeCandidate{route: route, valid: true}
	if route.method == "*" {
		if !d.wildcard.valid || d.wildcard.route.registrationIndex < route.registrationIndex {
			d.wildcard = candidate
		}
		return
	}
	if d.exact == nil {
		d.exact = make(map[string]routeCandidate)
	}
	current, ok := d.exact[route.method]
	if !ok || current.route.registrationIndex < route.registrationIndex {
		d.exact[route.method] = candidate
	}
	if !ok {
		index, _ := slices.BinarySearch(d.allowed, route.method)
		d.allowed = append(d.allowed, "")
		copy(d.allowed[index+1:], d.allowed[index:])
		d.allowed[index] = route.method
	}
}

func (d *routeDecisionIndex) lookup(
	host string,
	wildcardHost string,
	hostRank int,
	methodIndex int,
	method string,
) (routeCandidate, bool, bool) {
	decision := d.hostDecision(host, wildcardHost, hostRank)
	if decision == nil {
		return routeCandidate{}, false, false
	}
	if !decision.hasRoutes() {
		return routeCandidate{}, false, false
	}
	if methodIndex == 1 {
		return decision.wildcard, true, decision.wildcard.valid
	}
	candidate, ok := decision.exact[method]
	return candidate, true, ok
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
		return d.wildcardHosts[wildcardHost]
	default:
		return &d.hostless
	}
}

func (d *routeDecisionIndex) addAllowedMethods(
	host string,
	wildcardHost string,
	allowed []string,
) []string {
	add := func(decision *routeHostDecision) {
		if decision == nil {
			return
		}
		for _, method := range decision.allowed {
			index, found := slices.BinarySearch(allowed, method)
			if found {
				continue
			}
			allowed = append(allowed, "")
			copy(allowed[index+1:], allowed[index:])
			allowed[index] = method
		}
	}
	add(d.exactHosts[host])
	if wildcardHost != "" {
		add(d.wildcardHosts[wildcardHost])
	}
	add(&d.hostless)
	return allowed
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
) {
	dispatcher := r.dispatchers[converted]
	if dispatcher == nil {
		dispatcher = &wildcardDispatcher{
			prefix:   strings.TrimSuffix(converted, "*"),
			embedded: make(map[string]*routeDecisionIndex),
		}
		r.mux.Handle(converted, dispatcher)
		r.dispatchers[converted] = dispatcher
	}

	embedded := strings.Contains(pattern, "/*/")
	if len(methods) == 0 {
		dispatcher.add(wildcardRoute{
			method:            "*",
			pattern:           pattern,
			embedded:          embedded,
			hosts:             hosts,
			handler:           handler,
			registrationIndex: registrationIndex,
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
		})
	}
}

func (d *wildcardDispatcher) add(route wildcardRoute) {
	if d.embedded == nil {
		d.embedded = make(map[string]*routeDecisionIndex)
	}
	if route.embedded {
		suffix := route.pattern[strings.IndexByte(route.pattern, '*')+1:]
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
	host := requestHostname(request.Host)
	wildcardHost := wildcardHostKey(host)
	nonEmbeddedPathMatched := d.nonEmbedded != nil &&
		matchesRoutePath(d.nonEmbedded.pattern, request.URL.Path)
	pathMatched := false
	hostMatched := false
	for embeddedIndex := range 2 {
		for _, hostRank := range []int{2, 1, 0} {
			for methodIndex := range 2 {
				if embeddedIndex == 0 {
					if len(d.embedded) == 0 {
						continue
					}
					route, matched, matchedPath, matchedHost := d.matchEmbeddedRoute(
						request,
						host,
						wildcardHost,
						hostRank,
						methodIndex,
					)
					pathMatched = pathMatched || matchedPath
					hostMatched = hostMatched || matchedHost
					if matched {
						route.handler.ServeHTTP(writer, request)
						return
					}
					continue
				}
				route, matched, matchedPath, matchedHost := d.matchNonEmbeddedRoute(
					request,
					host,
					wildcardHost,
					hostRank,
					methodIndex,
					nonEmbeddedPathMatched,
				)
				pathMatched = pathMatched || matchedPath
				hostMatched = hostMatched || matchedHost
				if matched {
					route.handler.ServeHTTP(writer, request)
					return
				}
			}
		}
	}
	if pathMatched && hostMatched {
		allowedMethods := d.allowedMethods(request)
		if len(allowedMethods) > 0 {
			writer.Header().Set("Allow", strings.Join(allowedMethods, ", "))
		}
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	http.NotFound(writer, request)
}

func (d *wildcardDispatcher) allowedMethods(request *http.Request) []string {
	host := requestHostname(request.Host)
	wildcardHost := wildcardHostKey(host)
	var allowed []string
	if d.nonEmbedded != nil && matchesRoutePath(d.nonEmbedded.pattern, request.URL.Path) {
		allowed = d.nonEmbedded.addAllowedMethods(host, wildcardHost, allowed)
	}
	for searchFrom := len(d.prefix); searchFrom < len(request.URL.Path); {
		relativeSlash := strings.IndexByte(request.URL.Path[searchFrom:], '/')
		if relativeSlash < 0 {
			break
		}
		suffixStart := searchFrom + relativeSlash
		suffix := request.URL.Path[suffixStart:]
		if decision := d.embedded[suffix]; decision != nil &&
			len(request.URL.Path) > len(d.prefix)+len(suffix) {
			allowed = decision.addAllowedMethods(host, wildcardHost, allowed)
		}
		searchFrom = suffixStart + 1
	}
	return allowed
}

func (d *wildcardDispatcher) matchEmbeddedRoute(
	request *http.Request,
	host string,
	wildcardHost string,
	hostRank int,
	methodIndex int,
) (wildcardRoute, bool, bool, bool) {
	requestPath := request.URL.Path
	if len(requestPath) <= len(d.prefix) || !strings.HasPrefix(requestPath, d.prefix) {
		return wildcardRoute{}, false, false, false
	}

	bestIndex := uint64(0)
	bestFound := false
	var bestRoute wildcardRoute
	pathMatched := false
	hostMatched := false
	for searchFrom := len(d.prefix); searchFrom < len(requestPath); {
		relativeSlash := strings.IndexByte(requestPath[searchFrom:], '/')
		if relativeSlash < 0 {
			break
		}
		suffixStart := searchFrom + relativeSlash
		suffix := requestPath[suffixStart:]
		decision := d.embedded[suffix]
		if decision != nil && len(requestPath) > len(d.prefix)+len(suffix) {
			pathMatched = true
			candidate, matchedHost, ok := decision.lookup(
				host,
				wildcardHost,
				hostRank,
				methodIndex,
				request.Method,
			)
			hostMatched = hostMatched || matchedHost
			if ok && (!bestFound || candidate.route.registrationIndex > bestIndex) {
				bestIndex = candidate.route.registrationIndex
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
		request.Method,
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
	patternParts := strings.Split(pattern, "/")
	requestParts := strings.Split(requestPath, "/")
	if len(patternParts) != len(requestParts) {
		return false
	}
	for i := range patternParts {
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
	return true
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
	if !strings.HasSuffix(host, suffix) {
		return false
	}
	prefix := strings.TrimSuffix(host, suffix)
	return prefix != "" && !strings.Contains(prefix, ".")
}

func matchesWildcardRoute(pattern string, path string) bool {
	wildcard := strings.IndexByte(pattern, '*')
	prefix := pattern[:wildcard]
	suffix := pattern[wildcard+1:]
	if suffix == "" {
		return strings.HasPrefix(path, prefix)
	}
	return strings.HasPrefix(path, prefix) && strings.HasSuffix(path, suffix) &&
		len(path) > len(prefix)+len(suffix)
}

func normalizeRouteOrder(routes []resource.Route) []resource.Route {
	normalized := append([]resource.Route(nil), routes...)
	slices.SortStableFunc(normalized, func(left, right resource.Route) int {
		return cmp.Compare(left.Priority, right.Priority)
	})
	return normalized
}
