package stream

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/mqtt_proxy"
	pxy "github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

const defaultStreamConnectTimeout = 5 * time.Second

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
	balancer  pxy.LoadBalancer
	chash     bool
	hashNodes []hashTarget
	serve     func(context.Context, net.Conn, string) (string, string, error)
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
	entries := make([]routeEntry, 0, len(routes))
	for _, route := range routes {
		entry, err := buildRouteEntry(route, r.enabledPlugins)
		if err != nil {
			return err
		}
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(left, right int) bool {
		return routeSpecificity(entries[left].route) > routeSpecificity(entries[right].route)
	})

	r.mu.Lock()
	r.routes = entries
	r.mu.Unlock()
	return nil
}

func (r *Router) Serve(ctx context.Context, listener net.Listener, client net.Conn) error {
	if client == nil {
		return fmt.Errorf("stream client connection is nil")
	}
	listenerAddr := ""
	if listener != nil && listener.Addr() != nil {
		listenerAddr = listener.Addr().String()
	}
	remoteAddr := ""
	if client.RemoteAddr() != nil {
		remoteAddr = client.RemoteAddr().String()
	}

	r.mu.RLock()
	entry, ok := r.matchEntry(listenerAddr, remoteAddr)
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

	targets := make(map[string]int, len(route.Upstream.Nodes))
	hashNodes := make([]hashTarget, 0, len(route.Upstream.Nodes))
	for _, node := range route.Upstream.Nodes {
		address, err := nodeAddress(node)
		if err != nil {
			return routeEntry{}, fmt.Errorf("stream route %q upstream node: %w", route.ID, err)
		}
		target := "tcp://" + address
		weight := node.Weight
		if weight <= 0 {
			weight = 1
		}
		targets[target] = weight
		hashNodes = append(hashNodes, hashTarget{target: target, weight: weight})
	}
	entry := routeEntry{
		route:     route,
		balancer:  pxy.NewWeightedRRLoadBalance(targets),
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
		if err := util.Validate(config, p.GetSchema()); err != nil {
			return routeEntry{}, fmt.Errorf("validate stream plugin %s: %w", name, err)
		}
		if err := util.Parse(config, p.Config()); err != nil {
			return routeEntry{}, fmt.Errorf("parse stream plugin %s: %w", name, err)
		}
		if err := p.PostInit(); err != nil {
			return routeEntry{}, fmt.Errorf("initialize stream plugin %s: %w", name, err)
		}
		entry.serve = func(ctx context.Context, client net.Conn, peer string) (string, string, error) {
			info, err := p.ServeStream(ctx, client, peer, entry.dial)
			return info.ClientID, "mqtt", err
		}
	}
	return entry, nil
}

func (e routeEntry) rawServe(ctx context.Context, client net.Conn, peer string) (string, string, error) {
	upstream, err := e.dial(ctx, peer)
	if err != nil {
		_ = client.Close()
		return "", "tcp", err
	}
	return "", "tcp", bridge(ctx, client, upstream)
}

func (e routeEntry) dial(ctx context.Context, key string) (net.Conn, error) {
	target := e.selectTarget(key)
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
	if !e.chash || key == "" || len(e.hashNodes) == 0 {
		return e.balancer.Next()
	}
	var total uint64
	for _, node := range e.hashNodes {
		if node.weight > 0 {
			total += uint64(node.weight)
		}
	}
	if total == 0 {
		return e.balancer.Next()
	}
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(key))
	offset := uint64(hasher.Sum32()) % total
	for _, node := range e.hashNodes {
		if offset < uint64(node.weight) {
			return node.target
		}
		offset -= uint64(node.weight)
	}
	return e.hashNodes[len(e.hashNodes)-1].target
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

func routeSpecificity(route resource.StreamRoute) int {
	score := 0
	if route.ServerAddr != "" && route.ServerAddr != "0.0.0.0" && route.ServerAddr != "::" {
		score += 8
	}
	if route.ServerPort != 0 {
		score += 4
	}
	if route.RemoteAddr != "" {
		score += 2
		if net.ParseIP(route.RemoteAddr) != nil {
			score++
		}
	}
	return score
}

func bridge(ctx context.Context, client net.Conn, upstream net.Conn) error {
	if upstream == nil {
		_ = client.Close()
		return fmt.Errorf("stream upstream connection is nil")
	}
	closeDone := closeOnContextDone(ctx, client, upstream)
	defer closeDone()
	defer client.Close()
	defer upstream.Close()

	results := make(chan error, 2)
	go copyDirection(upstream, client, results)
	go copyDirection(client, upstream, results)
	first := <-results
	_ = client.Close()
	_ = upstream.Close()
	second := <-results
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err := normalizeCopyError(first); err != nil {
		return err
	}
	return normalizeCopyError(second)
}

func copyDirection(dst net.Conn, src net.Conn, results chan<- error) {
	_, err := io.Copy(dst, src)
	results <- err
}

func closeOnContextDone(ctx context.Context, conns ...net.Conn) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			for _, conn := range conns {
				_ = conn.Close()
			}
		case <-done:
		}
	}()
	return func() { close(done) }
}

func normalizeCopyError(err error) error {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "closed pipe") || strings.Contains(message, "use of closed network connection") {
		return nil
	}
	return err
}
