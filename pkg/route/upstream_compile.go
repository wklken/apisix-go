package route

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin"
	"github.com/wklken/apisix-go/pkg/plugin/kafka_proxy"
	"github.com/wklken/apisix-go/pkg/plugin/traffic_split"
	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
)

// UpstreamPlan is the owned, side-effect-free ordinary HTTP upstream plan.
type UpstreamPlan struct {
	Upstream      resource.Upstream
	Provenance    plugin.ResourceProvenance
	ClusterConfig *proxy.ClusterConfig
	Transport     proxy.TransportOption
	Targets       map[string]int
	Priorities    map[string]int
}

// PlanTrafficSplitCluster derives the same canonical cluster identity as the
// ordinary upstream path while leaving acquisition to the compiler.
func PlanTrafficSplitCluster(
	routeResource resource.Route,
	upstream *traffic_split.Upstream,
	targets map[string]int,
	priorities map[string]int,
	ssls map[string]resource.SSL,
	staticConfig *appconfig.Config,
) (proxy.ClusterConfig, error) {
	return planTrafficSplitClusterWithSSLResolver(
		routeResource, upstream, targets, priorities, plannedSSLResolver(ssls), staticConfig,
	)
}

func planTrafficSplitClusterWithSSLResolver(
	routeResource resource.Route,
	upstream *traffic_split.Upstream,
	targets map[string]int,
	priorities map[string]int,
	resolveSSL sslResolver,
	staticConfig *appconfig.Config,
) (proxy.ClusterConfig, error) {
	if upstream == nil {
		return proxy.ClusterConfig{}, fmt.Errorf("traffic-split upstream is nil")
	}
	resourceUpstream, err := trafficSplitResourceUpstream(
		upstream,
		maps.Clone(targets),
		maps.Clone(priorities),
	)
	if err != nil {
		return proxy.ClusterConfig{}, err
	}
	transport, err := buildTransportOptionWithSSLResolver(
		cloneCompileRoute(routeResource),
		resourceUpstream,
		resolveSSL,
	)
	if err != nil {
		return proxy.ClusterConfig{}, err
	}
	config, err := buildClusterConfigWithTransport(
		cloneCompileRoute(routeResource), resourceUpstream,
		maps.Clone(targets), transport, staticConfig, maps.Clone(priorities),
	)
	if err != nil {
		return proxy.ClusterConfig{}, err
	}
	config.Retries = max(upstream.Retries, 0)
	config.RetriesConfigured = upstream.RetriesConfigured()
	config.Targets = maps.Clone(config.Targets)
	config.Priorities = maps.Clone(config.Priorities)
	config.Checks = cloneCompileAnyMap(config.Checks)
	return config, nil
}

func trafficSplitResourceUpstream(
	upstream *traffic_split.Upstream,
	targets map[string]int,
	priorities map[string]int,
) (resource.Upstream, error) {
	scheme := upstream.Scheme
	if scheme == "" {
		scheme = "http"
	}
	result := resource.Upstream{
		Name: upstream.Name, Type: upstream.Type, Scheme: scheme, TLS: upstream.TLS,
		Timeout: upstream.Timeout, Checks: upstream.Checks,
		HashOn: upstream.HashOn, Key: upstream.Key,
		PassHost: upstream.PassHost, UpstreamHost: upstream.UpstreamHost,
		Retries: upstream.Retries, RetryTimeout: upstream.RetryTimeout,
		Nodes: make([]resource.Node, 0, len(targets)),
	}
	for _, target := range slices.Sorted(maps.Keys(targets)) {
		parsed, err := url.Parse(target)
		if err != nil {
			return resource.Upstream{}, fmt.Errorf("parse traffic-split target %q: %w", target, err)
		}
		if parsed.Hostname() == "" {
			return resource.Upstream{}, fmt.Errorf("traffic-split target %q has no host", target)
		}
		port := parsed.Port()
		if port == "" {
			switch strings.ToLower(parsed.Scheme) {
			case "https", "grpcs":
				port = "443"
			default:
				port = "80"
			}
		}
		numericPort, err := strconv.Atoi(port)
		if err != nil || numericPort < 1 || numericPort > 65535 {
			return resource.Upstream{}, fmt.Errorf(
				"traffic-split target %q has invalid port",
				target,
			)
		}
		result.Nodes = append(result.Nodes, resource.Node{
			Host: parsed.Hostname(), Port: numericPort,
			Weight: targets[target], Priority: priorities[target],
		})
	}
	return result, nil
}

// PlanRouteUpstream resolves and validates one route's ordinary upstream
// without creating a cluster, transport, load balancer, task, or lease.
func PlanRouteUpstream(
	routeResource resource.Route,
	service resource.Service,
	upstreams map[string]resource.Upstream,
	ssls map[string]resource.SSL,
	staticConfig *appconfig.Config,
) (UpstreamPlan, error) {
	resolved, provenance, err := resolveRouteUpstreamWithGetter(
		cloneCompileRoute(routeResource),
		cloneUpstreamPlanningService(service),
		func(id string) (resource.Upstream, error) {
			upstream, exists := upstreams[id]
			if !exists {
				return resource.Upstream{}, fmt.Errorf("upstream %q is missing", id)
			}
			return clonePlannedUpstream(upstream), nil
		},
	)
	if err != nil {
		return UpstreamPlan{}, err
	}
	resolved = clonePlannedUpstream(resolved)
	if err := validateUnsupportedUpstreamDiscovery(resolved, provenance); err != nil {
		return UpstreamPlan{}, err
	}
	if err := validateHTTPUpstreamType(resolved); err != nil {
		return UpstreamPlan{}, err
	}
	if err := validatePlannedPassHost(resolved); err != nil {
		return UpstreamPlan{}, err
	}
	targets, priorities, err := planUpstreamNodes(resolved)
	if err != nil {
		return UpstreamPlan{}, err
	}
	plan := UpstreamPlan{
		Upstream: resolved, Provenance: provenance,
		Targets: maps.Clone(targets), Priorities: maps.Clone(priorities),
	}
	scheme := strings.ToLower(resolved.Scheme)
	if scheme == "kafka" {
		return plan, nil
	}
	transport, err := buildTransportOptionWithSSLResolver(
		cloneCompileRoute(routeResource), resolved, plannedSSLResolver(ssls),
	)
	if err != nil {
		return UpstreamPlan{}, err
	}
	plan.Transport = transport
	clusterConfig, err := buildClusterConfigWithTransport(
		cloneCompileRoute(routeResource), resolved, maps.Clone(targets), transport,
		staticConfig, maps.Clone(priorities),
	)
	if err != nil {
		return UpstreamPlan{}, err
	}
	clusterConfig.Targets = maps.Clone(clusterConfig.Targets)
	clusterConfig.Priorities = maps.Clone(clusterConfig.Priorities)
	clusterConfig.Checks = cloneCompileAnyMap(clusterConfig.Checks)
	plan.ClusterConfig = &clusterConfig
	return plan, nil
}

func validatePlannedPassHost(upstream resource.Upstream) error {
	switch upstream.PassHost {
	case "", "pass", "node":
		return nil
	case "rewrite":
		if upstream.UpstreamHost == "" {
			return fmt.Errorf("`upstream_host` can't be empty when `pass_host` is `rewrite`")
		}
		return nil
	default:
		return fmt.Errorf("pass_host must be one of pass, node, or rewrite")
	}
}

func planUpstreamNodes(upstream resource.Upstream) (map[string]int, map[string]int, error) {
	targets := make(map[string]int, len(upstream.Nodes))
	priorities := make(map[string]int, len(upstream.Nodes))
	targetScheme := upstream.Scheme
	if strings.EqualFold(targetScheme, "grpc") {
		targetScheme = "http"
	} else if strings.EqualFold(targetScheme, "grpcs") {
		targetScheme = "https"
	}
	for _, node := range upstream.Nodes {
		host, port, weight := node.Host, node.Port, node.Weight
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
			host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
		}
		if port == 0 {
			switch strings.ToLower(targetScheme) {
			case "https":
				port = 443
			case "http":
				port = 80
			}
		}
		if host == "" || port < 1 || port > 65535 {
			return nil, nil, fmt.Errorf("invalid upstream node %q:%d", host, port)
		}
		if !node.WeightConfigured() {
			return nil, nil, fmt.Errorf(
				"invalid upstream node %q:%d: weight is required",
				host,
				port,
			)
		}
		if weight < 0 {
			return nil, nil, fmt.Errorf(
				"invalid upstream node %q:%d: weight must be non-negative",
				host,
				port,
			)
		}
		target := fmt.Sprintf("%s://%s", targetScheme, net.JoinHostPort(host, strconv.Itoa(port)))
		targets[target] = weight
		priorities[target] = node.Priority
	}
	if len(targets) > 0 {
		positive := false
		for _, weight := range targets {
			if weight > 0 {
				positive = true
				break
			}
		}
		if !positive {
			return nil, nil, fmt.Errorf(
				"invalid upstream node weights: at least one upstream node must have a positive weight",
			)
		}
	}
	return targets, priorities, nil
}

func plannedSSLResolver(ssls map[string]resource.SSL) sslResolver {
	return func(id string) (resource.SSL, error) {
		ssl, exists := ssls[id]
		if !exists {
			return resource.SSL{}, fmt.Errorf("SSL resource %q is missing", id)
		}
		return clonePlannedSSL(ssl), nil
	}
}

func clonePlannedUpstream(source resource.Upstream) resource.Upstream {
	cloned := cloneCompileUpstream(source)
	if source.TLS != nil {
		cloned.TLS.ClientCertID = cloneCompileValue(source.TLS.ClientCertID)
	}
	return cloned
}

func cloneUpstreamPlanningService(source resource.Service) resource.Service {
	cloned := source
	cloned.Plugins = cloneCompilePluginConfigs(source.Plugins)
	cloned.Hosts = append([]string(nil), source.Hosts...)
	cloned.Upstream = clonePlannedUpstream(source.Upstream)
	return cloned
}

func clonePlannedSSL(source resource.SSL) resource.SSL {
	cloned := source
	cloned.Snis = append([]string(nil), source.Snis...)
	cloned.Labels = maps.Clone(source.Labels)
	if source.Client != nil {
		client := *source.Client
		client.SkipMTLSURIRegex = append([]string(nil), source.Client.SkipMTLSURIRegex...)
		cloned.Client = &client
	}
	return cloned
}

func resolveRouteUpstreamWithGetter(
	r resource.Route,
	service resource.Service,
	getUpstream func(string) (resource.Upstream, error),
) (resource.Upstream, plugin.ResourceProvenance, error) {
	// Keep this priority identical to buildReverseHandler: inline route,
	// route upstream_id, inline service, then service upstream_id.
	if inlineUpstreamConfigured(r.Upstream) {
		return r.Upstream, plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: r.ID}, nil
	}
	if r.UpstreamID != "" {
		upstream, err := getUpstream(r.UpstreamID)
		if err != nil {
			return resource.Upstream{}, plugin.ResourceProvenance{Kind: plugin.ResourceUpstream, ID: r.UpstreamID},
				fmt.Errorf("get upstream %q fail: %w", r.UpstreamID, err)
		}
		return upstream, plugin.ResourceProvenance{Kind: plugin.ResourceUpstream, ID: r.UpstreamID}, nil
	}
	if inlineUpstreamConfigured(service.Upstream) {
		return service.Upstream, plugin.ResourceProvenance{Kind: plugin.ResourceService, ID: service.ID}, nil
	}
	if service.UpstreamID != "" {
		upstream, err := getUpstream(service.UpstreamID)
		if err != nil {
			return resource.Upstream{}, plugin.ResourceProvenance{
					Kind: plugin.ResourceUpstream,
					ID:   service.UpstreamID,
				},
				fmt.Errorf(
					"get upstream %q fail: %w",
					service.UpstreamID,
					err,
				)
		}
		return upstream, plugin.ResourceProvenance{Kind: plugin.ResourceUpstream, ID: service.UpstreamID}, nil
	}
	return resource.Upstream{}, plugin.ResourceProvenance{Kind: plugin.ResourceRoute, ID: r.ID}, nil
}

func validateUnsupportedUpstreamDiscovery(
	upstream resource.Upstream,
	provenance plugin.ResourceProvenance,
) error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "discovery_type", value: upstream.DiscoveryType},
		{name: "service_name", value: upstream.ServiceName},
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

func validateHTTPUpstreamType(upstream resource.Upstream) error {
	switch strings.ToLower(upstream.Scheme) {
	case "", "http", "https", "grpc", "grpcs":
		if upstream.Type != "" && upstream.Type != "roundrobin" {
			return fmt.Errorf(
				"unsupported upstream type %q for %q scheme: only roundrobin is supported",
				upstream.Type,
				upstream.Scheme,
			)
		}
	}
	return nil
}

func buildKafkaPubSubProxyHandlerStrictWithSSLResolver(
	upstream resource.Upstream,
	factory kafka_proxy.KafkaConsumerFactory,
	resolveSSL sslResolver,
) (http.Handler, error) {
	options := kafka_proxy.TransportOptions{}
	if upstream.Timeout.Connect > 0 {
		options.ConnectTimeout = time.Duration(upstream.Timeout.Connect * float64(time.Second))
	}
	if upstream.Timeout.Send > 0 {
		options.WriteTimeout = time.Duration(upstream.Timeout.Send * float64(time.Second))
	}
	if upstream.Timeout.Read > 0 {
		options.ReadTimeout = time.Duration(upstream.Timeout.Read * float64(time.Second))
	}
	if upstream.TLS != nil {
		clientCert := upstream.TLS.ClientCert
		clientKey := upstream.TLS.ClientKey
		if upstream.TLS.ClientCertID != nil {
			if clientCert != "" || clientKey != "" {
				return nil, fmt.Errorf(
					"kafka upstream client_cert_id cannot be combined with client_cert or client_key",
				)
			}
			id, err := normalizeSSLID(upstream.TLS.ClientCertID)
			if err != nil {
				return nil, fmt.Errorf("invalid Kafka upstream client_cert_id: %w", err)
			}
			if resolveSSL == nil {
				return nil, fmt.Errorf("kafka upstream client_cert_id %q cannot be resolved", id)
			}
			ssl, err := resolveSSL(id)
			if err != nil {
				return nil, fmt.Errorf("resolve Kafka upstream client_cert_id %q: %w", id, err)
			}
			clientCert = ssl.Cert
			clientKey = ssl.Key
		}
		if (clientCert == "") != (clientKey == "") {
			return nil, fmt.Errorf("kafka upstream client_cert and client_key must be configured together")
		}
		tlsConfig := &tls.Config{InsecureSkipVerify: !upstream.TLS.Verify} //nolint:gosec
		if clientCert != "" {
			certificate, err := tls.X509KeyPair(
				[]byte(clientCert),
				[]byte(clientKey),
			)
			if err != nil {
				return nil, fmt.Errorf("parse Kafka upstream client certificate: %w", err)
			}
			tlsConfig.Certificates = []tls.Certificate{certificate}
		}
		options.TLSConfig = tlsConfig
	}
	brokers := make([]string, 0, len(upstream.Nodes))
	for _, node := range upstream.Nodes {
		brokerHost := upstreamNodeHost("kafka", node.Host, strconv.Itoa(node.Port))
		brokers = append(brokers, "kafka://"+brokerHost)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !kafka_proxy.IsWebSocketUpgrade(r) {
			http.Error(w, kafka_proxy.ErrWebSocketUpgradeRequired.Error(), http.StatusUpgradeRequired)
			return
		}
		if len(brokers) == 0 {
			http.Error(w, "Kafka upstream has no configured nodes", http.StatusBadGateway)
			return
		}
		if err := kafka_proxy.ServePubSubWebSocket(w, r, brokers, options, factory); err != nil {
			if kafka_proxy.WebSocketWasHijacked(err) {
				return
			}
			http.Error(w, "Kafka upstream proxy failed", http.StatusBadGateway)
		}
	}), nil
}

func compileUpstreamTargets(servers map[string]int) (map[string]compiledUpstreamTarget, error) {
	targets := make(map[string]compiledUpstreamTarget, len(servers))
	for target := range servers {
		compiled, err := parseCompiledUpstreamTarget(target)
		if err != nil {
			return nil, err
		}
		targets[target] = compiled
	}
	return targets, nil
}

// errEmptyUpstream is reported by the director when a route has no upstream
// nodes and no traffic-split override selected a target.
var errEmptyUpstream = errors.New("upstream has no configured nodes")

func withDirectorError(request *http.Request, err error) *http.Request {
	return request.WithContext(context.WithValue(request.Context(), directorErrorContextKey{}, err))
}

func applyUpstreamTargetCompiled(
	request *http.Request,
	loadBalancer proxy.LoadBalancer,
	upstream resource.Upstream,
	originalHost string,
	targets map[string]compiledUpstreamTarget,
) error {
	target := proxy.NextTarget(loadBalancer, request)
	proxy.SetSelectedTarget(request, target)
	compiled, err := resolveCompiledUpstreamTarget(target, targets)
	if err != nil {
		return err
	}
	request.URL.Scheme = compiled.scheme
	request.URL.Host = compiled.host
	nodeHost := compiled.nodeHost
	switch upstream.PassHost {
	case "", "pass":
		request.Host = originalHost
		if request.Host == "" {
			request.Host = nodeHost
		}
	case "rewrite":
		request.Host = upstream.UpstreamHost
	case "node":
		request.Host = nodeHost
	}
	return nil
}

func inlineUpstreamConfigured(upstream resource.Upstream) bool {
	return upstream.Nodes != nil || upstream.Scheme != "" || upstream.TLS != nil ||
		upstream.Type != "" || upstream.Checks != nil || upstream.HashOn != "" ||
		upstream.Key != "" || upstream.PassHost != "" || upstream.UpstreamHost != "" ||
		upstream.Name != "" || upstream.Desc != "" || upstream.RetriesConfigured() ||
		upstream.Timeout != (resource.Timeout{}) || upstream.DiscoveryType != "" ||
		upstream.ServiceName != ""
}

func upstreamNodeHost(scheme, host, port string) string {
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	standardPort := false
	switch strings.ToLower(scheme) {
	case "http", "grpc":
		standardPort = port == "80"
	case "https", "grpcs":
		standardPort = port == "443"
	}
	if standardPort {
		if strings.Contains(host, ":") {
			return "[" + host + "]"
		}
		return host
	}
	return net.JoinHostPort(host, port)
}

type compiledUpstreamTarget struct {
	scheme   string
	host     string
	nodeHost string
}

func resolveCompiledUpstreamTarget(
	target string,
	targets map[string]compiledUpstreamTarget,
) (compiledUpstreamTarget, error) {
	if compiled, ok := targets[target]; ok {
		return compiled, nil
	}
	// Compatibility fallback for direct helper callers. Built route handlers
	// always provide the immutable precompiled target table.
	return parseCompiledUpstreamTarget(target)
}

func parseCompiledUpstreamTarget(target string) (compiledUpstreamTarget, error) {
	parsed, err := url.Parse(target)
	if err != nil {
		return compiledUpstreamTarget{}, fmt.Errorf("parse upstream target %q: %w", target, err)
	}
	if parsed.Host == "" || parsed.Hostname() == "" {
		return compiledUpstreamTarget{}, fmt.Errorf("upstream target %q has no host", target)
	}
	port := parsed.Port()
	if port != "" {
		numericPort, err := strconv.Atoi(port)
		if err != nil || numericPort < 1 || numericPort > 65535 {
			return compiledUpstreamTarget{}, fmt.Errorf("upstream target %q has invalid port", target)
		}
	}
	return compiledUpstreamTarget{
		scheme:   parsed.Scheme,
		host:     parsed.Host,
		nodeHost: upstreamNodeHost(parsed.Scheme, parsed.Hostname(), port),
	}, nil
}

type directorErrorContextKey struct{}

func requestDirectorError(request *http.Request) error {
	if request == nil {
		return nil
	}
	err, _ := request.Context().Value(directorErrorContextKey{}).(error)
	return err
}
