package stream

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/mqtt_proxy"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
	streambridge "github.com/wklken/apisix-go/pkg/stream/bridge"
	"github.com/wklken/apisix-go/pkg/util"
)

const (
	defaultStreamConnectTimeout = 5 * time.Second
	// defaultStreamIdleTimeout bounds an idle bridge direction when the
	// route does not configure an upstream read timeout.
	defaultStreamIdleTimeout = 60 * time.Second
)

var ErrNoStreamRoute = errors.New("no matching stream route")

type Result struct {
	RouteID  string
	Listener string
	Remote   string
	ClientID string
	Protocol string
	Err      error
}

type Router struct {
	mu             sync.RWMutex
	routes         []routeEntry
	enabledPlugins map[string]struct{}
	onResult       func(Result)
}

type routeEntry struct {
	route     resource.StreamRoute
	groups    []streamTargetGroup
	chash     bool
	hashNodes []hashTarget
	serve     func(context.Context, net.Conn, string) (string, string, error)
}

type streamTargetGroup struct {
	weights   map[string]int
	balancer  pxy.LoadBalancer
	hashNodes []hashTarget
}

type hashTarget struct {
	target string
	weight int
}

func NewRouter(
	routes []resource.StreamRoute,
	enabledPlugins []string,
	onResult func(Result),
) (*Router, error) {
	router := &Router{
		enabledPlugins: make(map[string]struct{}, len(enabledPlugins)),
		onResult:       onResult,
	}
	for _, name := range enabledPlugins {
		router.enabledPlugins[name] = struct{}{}
	}
	if err := router.Reload(routes); err != nil {
		return nil, err
	}
	return router, nil
}

func (r *Router) Reload(routes []resource.StreamRoute) error {
	if err := rejectConflictingStreamListens(routes); err != nil {
		return err
	}
	entries := make([]routeEntry, 0, len(routes))
	for _, route := range routes {
		entry, err := buildRouteEntry(route, r.enabledPlugins)
		if err != nil {
			return err
		}
		entries = append(entries, entry)
	}
	r.mu.Lock()
	r.routes = entries
	r.mu.Unlock()
	return nil
}

func rejectConflictingStreamListens(routes []resource.StreamRoute) error {
	seen := make(map[string]string, len(routes))
	for _, route := range routes {
		if route.ServerAddr == "" && route.ServerPort == 0 {
			continue
		}
		key := streamListenKey(route)
		if previous, ok := seen[key]; ok {
			return fmt.Errorf(
				"conflicting stream listen address %s:%d between %q and %q",
				route.ServerAddr,
				route.ServerPort,
				previous,
				route.ID,
			)
		}
		seen[key] = route.ID
	}
	return nil
}

func streamListenKey(route resource.StreamRoute) string {
	return route.ServerAddr + "\x00" + strconv.Itoa(route.ServerPort)
}

func (r *Router) Serve(ctx context.Context, listener net.Listener, client net.Conn) error {
	if client == nil {
		return fmt.Errorf("stream client connection is nil")
	}
	listenerAddr := ""
	if listener != nil && listener.Addr() != nil {
		listenerAddr = listener.Addr().String()
	}
	serverAddr := listenerAddr
	if client.LocalAddr() != nil {
		serverAddr = client.LocalAddr().String()
	}
	remoteAddr := ""
	if client.RemoteAddr() != nil {
		remoteAddr = client.RemoteAddr().String()
	}

	r.mu.RLock()
	entry, ok := r.matchEntry(serverAddr, remoteAddr)
	r.mu.RUnlock()
	if !ok {
		err := ErrNoStreamRoute
		_ = client.Close()
		r.emit(Result{Listener: listenerAddr, Remote: remoteAddr, Protocol: "tcp", Err: err})
		return err
	}

	clientID, protocol, err := entry.serve(ctx, client, remoteAddr)
	result := Result{
		RouteID:  entry.route.ID,
		Listener: listenerAddr,
		Remote:   remoteAddr,
		ClientID: clientID,
		Protocol: protocol,
		Err:      err,
	}
	r.emit(result)
	return err
}

func (r *Router) routeMatches(route resource.StreamRoute, listenerAddr, remoteAddr string) bool {
	if route.ServerPort != 0 {
		_, port, err := net.SplitHostPort(listenerAddr)
		if err != nil || port != strconv.Itoa(route.ServerPort) {
			return false
		}
	}
	if route.ServerAddr != "" {
		host, _, err := net.SplitHostPort(listenerAddr)
		if err != nil || !matchesListenerHost(route.ServerAddr, host) {
			return false
		}
	}
	if route.RemoteAddr == "" {
		return true
	}

	peerHost, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		peerHost = remoteAddr
	}
	if peerHost == route.RemoteAddr {
		return true
	}
	_, network, err := net.ParseCIDR(route.RemoteAddr)
	return err == nil && network.Contains(net.ParseIP(peerHost))
}

func (r *Router) matchEntry(listenerAddr, remoteAddr string) (routeEntry, bool) {
	for _, entry := range r.routes {
		if r.routeMatches(entry.route, listenerAddr, remoteAddr) {
			return entry, true
		}
	}
	return routeEntry{}, false
}

func (r *Router) emit(result Result) {
	if r.onResult != nil {
		r.onResult(result)
	}
}

func buildRouteEntry(route resource.StreamRoute, enabledPlugins map[string]struct{}) (routeEntry, error) {
	if err := validateUnsupportedDiscovery(route); err != nil {
		return routeEntry{}, err
	}
	if route.UpstreamID != "" && len(route.Upstream.Nodes) == 0 {
		return routeEntry{}, fmt.Errorf("stream route %q upstream_id %q was not resolved", route.ID, route.UpstreamID)
	}
	if len(route.Upstream.Nodes) == 0 {
		return routeEntry{}, fmt.Errorf("stream route %q has no upstream nodes", route.ID)
	}
	if route.Upstream.Scheme != "" && route.Upstream.Scheme != "tcp" {
		return routeEntry{}, fmt.Errorf("unsupported stream upstream scheme %q", route.Upstream.Scheme)
	}
	if strings.EqualFold(route.Upstream.Type, "chash") && route.Upstream.HashOn != "" &&
		!strings.EqualFold(route.Upstream.HashOn, "vars") {
		return routeEntry{}, fmt.Errorf("unsupported stream chash hash_on %q", route.Upstream.HashOn)
	}
	if route.RemoteAddr != "" && net.ParseIP(route.RemoteAddr) == nil {
		if _, _, err := net.ParseCIDR(route.RemoteAddr); err != nil {
			return routeEntry{}, fmt.Errorf("stream route %q remote_addr %q is invalid", route.ID, route.RemoteAddr)
		}
	}

	groupWeights := make(map[int]map[string]int, len(route.Upstream.Nodes))
	groupHashNodes := make(map[int][]hashTarget, len(route.Upstream.Nodes))
	for _, node := range route.Upstream.Nodes {
		address, err := nodeAddress(node)
		if err != nil {
			return routeEntry{}, fmt.Errorf("stream route %q upstream node: %w", route.ID, err)
		}
		target := "tcp://" + address
		weight := node.Weight
		if !node.WeightConfigured() {
			weight = 1
		}
		if weight < 0 {
			return routeEntry{}, fmt.Errorf(
				"stream route %q upstream node %q: weight must be non-negative",
				route.ID,
				target,
			)
		}
		if weight == 0 {
			continue
		}
		weights, ok := groupWeights[node.Priority]
		if !ok {
			weights = make(map[string]int)
			groupWeights[node.Priority] = weights
		}
		weights[target] = weight
		groupHashNodes[node.Priority] = append(
			groupHashNodes[node.Priority],
			hashTarget{target: target, weight: weight},
		)
	}
	if len(groupWeights) == 0 {
		return routeEntry{}, fmt.Errorf(
			"stream route %q upstream node weights: at least one upstream node must have a positive weight",
			route.ID,
		)
	}
	priorities := make([]int, 0, len(groupWeights))
	for priority := range groupWeights {
		priorities = append(priorities, priority)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(priorities)))
	groups := make([]streamTargetGroup, 0, len(priorities))
	hashNodes := make([]hashTarget, 0, len(route.Upstream.Nodes))
	for _, priority := range priorities {
		weights := groupWeights[priority]
		priorityHashNodes := groupHashNodes[priority]
		groups = append(groups, streamTargetGroup{
			weights:   weights,
			balancer:  pxy.NewWeightedRRLoadBalance(weights),
			hashNodes: priorityHashNodes,
		})
		hashNodes = append(hashNodes, priorityHashNodes...)
	}
	entry := routeEntry{
		route:     route,
		groups:    groups,
		chash:     strings.EqualFold(route.Upstream.Type, "chash"),
		hashNodes: hashNodes,
	}

	if len(route.Plugins) == 0 {
		entry.serve = entry.rawServe
		return entry, nil
	}
	if len(route.Plugins) != 1 {
		return routeEntry{}, fmt.Errorf("stream route %q must configure exactly one supported stream plugin", route.ID)
	}
	for name, config := range route.Plugins {
		if len(enabledPlugins) > 0 {
			if _, ok := enabledPlugins[name]; !ok {
				return routeEntry{}, fmt.Errorf("stream plugin %q is not enabled", name)
			}
		}
		if name != "mqtt-proxy" {
			return routeEntry{}, fmt.Errorf("stream plugin %q is not supported by the Go stream owner", name)
		}
		p := &mqtt_proxy.Plugin{}
		if err := p.Init(); err != nil {
			return routeEntry{}, fmt.Errorf("initialize stream plugin %s: %w", name, err)
		}
		compiledSchema, err := util.CompileSchema(p.GetSchema())
		if err != nil {
			return routeEntry{}, fmt.Errorf("validate stream plugin %s: %w", name, err)
		}
		if err := compiledSchema.Validate(config); err != nil {
			return routeEntry{}, fmt.Errorf("validate stream plugin %s: %w", name, err)
		}
		if err := util.Parse(config, p.Config()); err != nil {
			return routeEntry{}, fmt.Errorf("parse stream plugin %s: %w", name, err)
		}
		if err := base.MaterializePluginSecrets(p); err != nil {
			return routeEntry{}, fmt.Errorf("materialize stream plugin %s secrets: %w", name, err)
		}
		if err := p.PostInit(); err != nil {
			return routeEntry{}, fmt.Errorf("initialize stream plugin %s: %w", name, err)
		}
		entry.serve = func(ctx context.Context, client net.Conn, peer string) (string, string, error) {
			info, err := p.ServeStreamWithIdle(ctx, client, peer, entry.dial, entry.streamIdleTimeout())
			return info.ClientID, "mqtt", err
		}
	}
	return entry, nil
}

func validateUnsupportedDiscovery(route resource.StreamRoute) error {
	provenance := plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: route.ID}
	if route.UpstreamID != "" {
		provenance = plugin.ResourceProvenance{Kind: plugin.ResourceUpstream, ID: route.UpstreamID}
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "discovery_type", value: route.Upstream.DiscoveryType},
		{name: "service_name", value: route.Upstream.ServiceName},
	} {
		if field.value == "" {
			continue
		}
		return fmt.Errorf(
			"unsupported upstream field %q from %s %q: dynamic discovery is not supported",
			field.name,
			provenance.Kind,
			provenance.ID,
		)
	}
	return nil
}

func (e routeEntry) rawServe(ctx context.Context, client net.Conn, peer string) (string, string, error) {
	upstream, err := e.dial(ctx, peer)
	if err != nil {
		_ = client.Close()
		return "", "tcp", err
	}
	return "", "tcp", streambridge.Pump(ctx, client, upstream, nil, e.streamIdleTimeout())
}

// streamIdleTimeout is the per-direction idle bound for the stream bridge:
// the route's upstream read timeout when configured, else the default.
func (e routeEntry) streamIdleTimeout() time.Duration {
	if e.route.Upstream.Timeout.Read > 0 {
		return time.Duration(e.route.Upstream.Timeout.Read) * time.Second
	}
	return defaultStreamIdleTimeout
}

func (e routeEntry) dial(ctx context.Context, key string) (net.Conn, error) {
	retries := max(e.route.Upstream.Retries, 0)
	tried := make([]map[string]struct{}, len(e.groups))
	for i := range tried {
		tried[i] = make(map[string]struct{})
	}
	var lastErr error
	remaining := retries
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		groupIndex, target := e.nextTarget(key, tried)
		if target == "" {
			for i := range tried {
				clear(tried[i])
			}
			groupIndex, target = e.nextTarget(key, tried)
			if target == "" {
				if lastErr != nil {
					return nil, lastErr
				}
				return nil, errors.New("no stream upstream target available")
			}
		}
		tried[groupIndex][target] = struct{}{}
		conn, err := e.dialTarget(ctx, target)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if remaining == 0 {
			return nil, err
		}
		remaining--
	}
}

func (e routeEntry) dialTarget(ctx context.Context, target string) (net.Conn, error) {
	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme != "tcp" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid stream upstream target %q", target)
	}
	timeout := defaultStreamConnectTimeout
	if e.route.Upstream.Timeout.Connect > 0 {
		timeout = time.Duration(e.route.Upstream.Timeout.Connect) * time.Second
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", parsed.Host)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if dialCtxErr := dialCtx.Err(); dialCtxErr != nil {
			return nil, dialCtxErr
		}
		return nil, err
	}
	return conn, nil
}

func (e routeEntry) selectTarget(key string) string {
	if len(e.groups) == 0 {
		return ""
	}
	return e.groups[0].selectTarget(e.chash, key, nil)
}

func (e routeEntry) nextTarget(key string, tried []map[string]struct{}) (int, string) {
	for i, group := range e.groups {
		if target := group.selectTarget(e.chash, key, tried[i]); target != "" {
			return i, target
		}
	}
	return -1, ""
}

func (g streamTargetGroup) selectTarget(chash bool, key string, tried map[string]struct{}) string {
	if len(tried) == 0 {
		if !chash || key == "" || len(g.hashNodes) == 0 {
			return g.balancer.Next()
		}
		return selectHashTarget(g.hashNodes, key)
	}
	remaining := make(map[string]int, len(g.weights))
	for target, weight := range g.weights {
		if _, ok := tried[target]; !ok {
			remaining[target] = weight
		}
	}
	if len(remaining) == 0 {
		return ""
	}
	if chash && key != "" {
		return selectHashTarget(untriedHashNodes(g.hashNodes, tried), key)
	}
	return pxy.NewWeightedRRLoadBalance(remaining).Next()
}

func untriedHashNodes(nodes []hashTarget, tried map[string]struct{}) []hashTarget {
	untried := make([]hashTarget, 0, len(nodes))
	for _, node := range nodes {
		if _, ok := tried[node.target]; !ok {
			untried = append(untried, node)
		}
	}
	return untried
}

func selectHashTarget(nodes []hashTarget, key string) string {
	if len(nodes) == 0 {
		return ""
	}
	var total uint64
	for _, node := range nodes {
		if node.weight > 0 {
			total += uint64(node.weight)
		}
	}
	if total == 0 {
		return ""
	}
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(key))
	offset := uint64(hasher.Sum32()) % total
	for _, node := range nodes {
		if offset < uint64(node.weight) {
			return node.target
		}
		offset -= uint64(node.weight)
	}
	return nodes[len(nodes)-1].target
}

func nodeAddress(node resource.Node) (string, error) {
	host := strings.TrimSpace(node.Host)
	port := node.Port
	if host == "" {
		return "", fmt.Errorf("upstream node host is empty")
	}
	if parsedHost, parsedPort, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
		parsed, parseErr := strconv.Atoi(parsedPort)
		if parseErr != nil {
			return "", fmt.Errorf("upstream node port %q is invalid", parsedPort)
		}
		port = parsed
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("upstream node port %d is invalid", port)
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func matchesListenerHost(configured, actual string) bool {
	if configured == actual || configured == "0.0.0.0" || configured == "::" {
		return true
	}
	return false
}
