package route

import (
	"fmt"
	"maps"
	"net"
	"strconv"
	"strings"

	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/plugin"
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
		cloneCompileRoute(routeResource), resolved, plannedSSLResolver(ssls), staticConfig,
	)
	if err != nil {
		return UpstreamPlan{}, err
	}
	plan.Transport = transport
	if len(targets) == 0 && scheme != "grpc" && scheme != "grpcs" {
		return plan, nil
	}
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
			return nil, nil, fmt.Errorf("invalid upstream node %q:%d: weight is required", host, port)
		}
		if weight < 0 {
			return nil, nil, fmt.Errorf("invalid upstream node %q:%d: weight must be non-negative", host, port)
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
			return nil, nil, fmt.Errorf("invalid upstream node weights: at least one upstream node must have a positive weight")
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
