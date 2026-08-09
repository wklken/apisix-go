package route

import (
	"encoding/hex"
	"fmt"
	"time"

	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
)

type upstreamTimeouts struct {
	connect        time.Duration
	send           time.Duration
	read           time.Duration
	responseHeader time.Duration
}

func resolveUpstreamTimeouts(routeTimeout, upstreamTimeout resource.Timeout) upstreamTimeouts {
	resolved := upstreamTimeout
	if routeTimeout.Connect > 0 {
		resolved.Connect = routeTimeout.Connect
	}
	if routeTimeout.Send > 0 {
		resolved.Send = routeTimeout.Send
	}
	if routeTimeout.Read > 0 {
		resolved.Read = routeTimeout.Read
	}
	connect := proxy.DefaultDialTimeout
	if resolved.Connect > 0 {
		connect = time.Duration(resolved.Connect) * time.Second
	}
	read := time.Duration(resolved.Read) * time.Second
	return upstreamTimeouts{
		connect:        connect,
		send:           time.Duration(resolved.Send) * time.Second,
		read:           read,
		responseHeader: read,
	}
}

func upstreamTLSInsecureSkipVerify(upstream resource.Upstream) bool {
	return upstream.TLS == nil || !upstream.TLS.Verify
}

// buildClusterConfig converts the effective route/upstream configuration into
// the immutable cluster config that owns transport reuse, capacity, health,
// and retry behavior. The upstream ID/name is only a metric label; cluster
// identity is derived from the full effective configuration digest.
func buildClusterConfig(
	routeResource resource.Route,
	upstream resource.Upstream,
	servers map[string]int,
) (proxy.ClusterConfig, error) {
	timeouts := resolveUpstreamTimeouts(routeResource.Timeout, upstream.Timeout)

	opt := (&proxy.TransportOptionBuilder{}).
		WithDialTimeout(timeouts.connect).
		WithResponseHeaderTimeout(timeouts.responseHeader).
		WithIdleConnTimeout(30 * time.Second).
		WithInsecureSkipVerify(upstreamTLSInsecureSkipVerify(upstream))

	maxInFlight := proxy.DefaultMaxInFlight
	if appconfig.GlobalConfig != nil {
		proxyConfig := appconfig.GlobalConfig.Proxy
		opt = opt.
			WithMaxIdleConnections(proxyConfig.MaxIdleConns).
			WithMaxIdleConnectionsPerHost(proxyConfig.MaxIdleConnsPerHost).
			WithMaxConnectionsPerHost(proxyConfig.MaxConnsPerHost)
		if proxyConfig.MaxInFlight > 0 {
			maxInFlight = proxyConfig.MaxInFlight
		}
	}

	checks := upstream.Checks
	if appconfig.GlobalConfig != nil && appconfig.GlobalConfig.Apisix.DisableUpstreamHealthcheck {
		checks = withoutActiveChecks(checks)
	}

	config := proxy.ClusterConfig{
		Name:              upstreamMetricLabel(routeResource, upstream),
		Targets:           servers,
		Checks:            checks,
		Transport:         opt.Build(),
		SendTimeout:       timeouts.send,
		ReadTimeout:       timeouts.read,
		Retries:           httpRetryCount(upstream),
		RetriesConfigured: upstream.RetriesConfigured(),
		MaxInFlight:       maxInFlight,
	}
	if config.Name == "" {
		key, err := config.Key()
		if err != nil {
			return proxy.ClusterConfig{}, fmt.Errorf("compute cluster key for label: %w", err)
		}
		config.Name = hex.EncodeToString(key[:])[:12]
	}
	return config, nil
}

// upstreamMetricLabel returns the bounded metric label for a cluster. The
// upstream ID/name is a label only and never contributes to cluster identity.
func upstreamMetricLabel(routeResource resource.Route, upstream resource.Upstream) string {
	if upstream.Name != "" {
		return upstream.Name
	}
	return routeResource.UpstreamID
}

// withoutActiveChecks returns a copy of the upstream checks with the active
// probe block removed, honoring apisix.disable_upstream_healthcheck while
// retaining passive checks and ordinary weighted selection.
func withoutActiveChecks(checks map[string]any) map[string]any {
	if checks == nil {
		return nil
	}
	result := make(map[string]any, len(checks))
	for key, value := range checks {
		if key == "active" {
			continue
		}
		result[key] = value
	}
	return result
}
