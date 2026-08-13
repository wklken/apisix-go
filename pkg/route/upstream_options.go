package route

import (
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/store"
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

type sslResolver func(string) (resource.SSL, error)

func upstreamUsesTLS(upstream resource.Upstream) bool {
	return strings.EqualFold(upstream.Scheme, "https") || strings.EqualFold(upstream.Scheme, "grpcs")
}

func upstreamHasClientCertificate(upstream resource.Upstream) bool {
	return upstream.TLS != nil &&
		(upstream.TLS.ClientCertID != nil || upstream.TLS.ClientCert != "" || upstream.TLS.ClientKey != "")
}

func resolveUpstreamClientCertificate(
	upstream resource.Upstream,
	resolveSSL sslResolver,
) (tls.Certificate, error) {
	if upstream.TLS == nil {
		return tls.Certificate{}, nil
	}

	clientCert := upstream.TLS.ClientCert
	clientKey := upstream.TLS.ClientKey
	resolvedFromID := false
	if upstream.TLS.ClientCertID != nil {
		if clientCert != "" || clientKey != "" {
			return tls.Certificate{}, fmt.Errorf(
				"upstream client_cert_id cannot be combined with client_cert or client_key",
			)
		}
		id, err := normalizeSSLID(upstream.TLS.ClientCertID)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("invalid upstream client_cert_id: %w", err)
		}
		if resolveSSL == nil {
			return tls.Certificate{}, fmt.Errorf("upstream client_cert_id %q cannot be resolved", id)
		}
		ssl, err := resolveSSL(id)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("resolve upstream client_cert_id %q: %w", id, err)
		}
		if ssl.Status == 0 {
			return tls.Certificate{}, fmt.Errorf("upstream SSL resource %q is disabled", id)
		}
		clientCert = ssl.Cert
		clientKey = ssl.Key
		resolvedFromID = true
	}
	if (clientCert == "") != (clientKey == "") {
		return tls.Certificate{}, fmt.Errorf("upstream client_cert and client_key must be configured together")
	}
	if clientCert == "" {
		if resolvedFromID {
			return tls.Certificate{}, fmt.Errorf("upstream SSL resource must contain client_cert and client_key")
		}
		return tls.Certificate{}, nil
	}
	certificate, err := tls.X509KeyPair([]byte(clientCert), []byte(clientKey))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse upstream client certificate: %w", err)
	}
	return certificate, nil
}

func buildTransportOption(
	routeResource resource.Route,
	upstream resource.Upstream,
) (proxy.TransportOption, error) {
	return buildTransportOptionWithSSLResolver(routeResource, upstream, store.GetSSL)
}

func buildTransportOptionWithSSLResolver(
	routeResource resource.Route,
	upstream resource.Upstream,
	resolveSSL sslResolver,
) (proxy.TransportOption, error) {
	if upstreamHasClientCertificate(upstream) && !upstreamUsesTLS(upstream) {
		return proxy.TransportOption{}, fmt.Errorf(
			"upstream client certificate is only supported for HTTPS or grpcs schemes",
		)
	}

	timeouts := resolveUpstreamTimeouts(routeResource.Timeout, upstream.Timeout)
	optionBuilder := (&proxy.TransportOptionBuilder{}).
		WithDialTimeout(timeouts.connect).
		WithResponseHeaderTimeout(timeouts.responseHeader).
		WithIdleConnTimeout(30 * time.Second).
		WithInsecureSkipVerify(upstreamTLSInsecureSkipVerify(upstream))

	if upstreamUsesTLS(upstream) {
		certificate, err := resolveUpstreamClientCertificate(upstream, resolveSSL)
		if err != nil {
			return proxy.TransportOption{}, err
		}
		if len(certificate.Certificate) > 0 || certificate.PrivateKey != nil {
			optionBuilder = optionBuilder.WithTLSClientCertificate(certificate)
		}
	}

	if appconfig.GlobalConfig != nil {
		proxyConfig := appconfig.GlobalConfig.Proxy
		optionBuilder = optionBuilder.
			WithMaxIdleConnections(proxyConfig.MaxIdleConns).
			WithMaxIdleConnectionsPerHost(proxyConfig.MaxIdleConnsPerHost).
			WithMaxConnectionsPerHost(proxyConfig.MaxConnsPerHost)
	}
	return optionBuilder.Build(), nil
}

// buildClusterConfig converts the effective route/upstream configuration into
// the immutable cluster config that owns transport reuse, capacity, health,
// and retry behavior. The upstream ID/name is only a metric label; cluster
// identity is derived from the full effective configuration digest.
func buildClusterConfigWithSSLResolver(
	routeResource resource.Route,
	upstream resource.Upstream,
	servers map[string]int,
	resolveSSL sslResolver,
) (proxy.ClusterConfig, error) {
	transport, err := buildTransportOptionWithSSLResolver(routeResource, upstream, resolveSSL)
	if err != nil {
		return proxy.ClusterConfig{}, err
	}
	return buildClusterConfigWithTransport(routeResource, upstream, servers, transport)
}

func buildClusterConfigWithTransport(
	routeResource resource.Route,
	upstream resource.Upstream,
	servers map[string]int,
	transport proxy.TransportOption,
) (proxy.ClusterConfig, error) {
	timeouts := resolveUpstreamTimeouts(routeResource.Timeout, upstream.Timeout)

	maxInFlight := proxy.DefaultMaxInFlight
	if appconfig.GlobalConfig != nil {
		proxyConfig := appconfig.GlobalConfig.Proxy
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
		Transport:         transport,
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

func normalizeSSLID(value any) (string, error) {
	switch value := value.(type) {
	case string:
		value = strings.TrimSpace(value)
		if value == "" {
			return "", fmt.Errorf("must not be empty")
		}
		return value, nil
	case json.Number:
		return normalizeSSLNumber(string(value))
	case float64:
		return normalizeSSLFloat(value)
	case float32:
		return normalizeSSLFloat(float64(value))
	case int:
		return strconv.Itoa(value), nil
	case int8:
		return strconv.FormatInt(int64(value), 10), nil
	case int16:
		return strconv.FormatInt(int64(value), 10), nil
	case int32:
		return strconv.FormatInt(int64(value), 10), nil
	case int64:
		return strconv.FormatInt(value, 10), nil
	case uint:
		return strconv.FormatUint(uint64(value), 10), nil
	case uint8:
		return strconv.FormatUint(uint64(value), 10), nil
	case uint16:
		return strconv.FormatUint(uint64(value), 10), nil
	case uint32:
		return strconv.FormatUint(uint64(value), 10), nil
	case uint64:
		return strconv.FormatUint(value, 10), nil
	default:
		return "", fmt.Errorf("must be a string or integer")
	}
}

func normalizeSSLNumber(value string) (string, error) {
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return "", fmt.Errorf("must be a string or integer")
	}
	return normalizeSSLFloat(parsed)
}

func normalizeSSLFloat(value float64) (string, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value != math.Trunc(value) {
		return "", fmt.Errorf("must be an integer")
	}
	return strconv.FormatFloat(value, 'f', -1, 64), nil
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
