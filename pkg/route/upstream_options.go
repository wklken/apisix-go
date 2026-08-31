package route

import (
	"bytes"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	appconfig "github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/proxy_control"
	"github.com/wklken/apisix-go/pkg/plugin/traffic_split"
	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/resource"
)

type upstreamTimeouts struct {
	connect        time.Duration
	send           time.Duration
	read           time.Duration
	responseHeader time.Duration
}

const defaultUpstreamTimeout = 60 * time.Second

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
	return upstreamTimeouts{
		connect:        durationOrDefault(resolved.Connect),
		send:           durationOrDefault(resolved.Send),
		read:           durationOrDefault(resolved.Read),
		responseHeader: durationOrDefault(resolved.Read),
	}
}

func durationOrDefault(seconds int) time.Duration {
	if seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return defaultUpstreamTimeout
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

	return optionBuilder.Build(), nil
}

// buildClusterConfig converts the effective route/upstream configuration into
// the immutable cluster config that owns transport reuse, capacity, health,
// and retry behavior. The upstream ID/name participates in cluster identity
// because the shared observers capture that bounded label at construction.
func buildClusterConfigWithSSLResolver(
	routeResource resource.Route,
	upstream resource.Upstream,
	servers map[string]int,
	resolveSSL sslResolver,
	staticConfig *appconfig.Config,
	priorities ...map[string]int,
) (proxy.ClusterConfig, error) {
	transport, err := buildTransportOptionWithSSLResolver(routeResource, upstream, resolveSSL)
	if err != nil {
		return proxy.ClusterConfig{}, err
	}
	return buildClusterConfigWithTransport(routeResource, upstream, servers, transport, staticConfig, priorities...)
}

func buildClusterConfigWithTransport(
	routeResource resource.Route,
	upstream resource.Upstream,
	servers map[string]int,
	transport proxy.TransportOption,
	staticConfig *appconfig.Config,
	priorities ...map[string]int,
) (proxy.ClusterConfig, error) {
	timeouts := resolveUpstreamTimeouts(routeResource.Timeout, upstream.Timeout)

	checks := upstream.Checks
	if staticConfig != nil && staticConfig.Apisix.DisableUpstreamHealthcheck {
		checks = withoutActiveChecks(checks)
	}

	config := proxy.ClusterConfig{
		Name:              upstreamMetricLabel(routeResource, upstream),
		Targets:           servers,
		Priorities:        firstPriorityMap(priorities),
		Checks:            checks,
		Transport:         transport,
		SendTimeout:       timeouts.send,
		ReadTimeout:       timeouts.read,
		Retries:           httpRetryCount(upstream),
		RetriesConfigured: upstream.RetriesConfigured(),
		MaxInFlight:       proxy.DefaultMaxInFlight,
		HTTP2Cleartext:    strings.EqualFold(upstream.Scheme, "grpc"),
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

func firstPriorityMap(values []map[string]int) map[string]int {
	if len(values) == 0 {
		return nil
	}
	return values[0]
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

// upstreamMetricLabel returns the bounded metric label for a cluster. It also
// contributes to identity so an interned cluster cannot retain another
// upstream's observer label.
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

type trafficSplitRoundTripper struct {
	fallback http.RoundTripper
}

func (t *trafficSplitRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if override := traffic_split.GetOverride(request); override != nil && override.RoundTripper != nil {
		return override.RoundTripper.RoundTrip(request)
	}
	return t.fallback.RoundTrip(request)
}

func applyTrafficSplitOverride(req *http.Request) bool {
	override := traffic_split.GetOverride(req)
	return applyTrafficSplitTarget(req, override, req.Host)
}

func httpRetryCount(upstream resource.Upstream) int {
	if upstream.RetriesConfigured() {
		return max(upstream.Retries, 0)
	}
	return max(len(upstream.Nodes)-1, 0)
}

func attachHTTPRetriesCompiled(
	request *http.Request,
	upstream resource.Upstream,
	loadBalancer proxy.LoadBalancer,
	targets map[string]compiledUpstreamTarget,
) *http.Request {
	applyProxyRewriteBeforeUpstream(request)
	originalHost := request.Host
	if override := traffic_split.GetOverride(request); override != nil {
		return proxy.WithRetries(request, override.Retries, func(retry *http.Request) bool {
			if override.NextRetry == nil {
				proxy.SetSelectedTarget(retry, "")
				return false
			}
			next := override.NextRetry(retry)
			if !applyTrafficSplitTarget(retry, next, originalHost) {
				proxy.SetSelectedTarget(retry, "")
				return false
			}
			applyFinalProxyRewrite(retry)
			return true
		})
	}
	return proxy.WithRetries(request, httpRetryCount(upstream), func(retry *http.Request) bool {
		if err := applyUpstreamTargetCompiled(retry, loadBalancer, upstream, originalHost, targets); err != nil {
			return false
		}
		// A later transport failure must not report a stale director error
		// from an earlier attempt.
		*retry = *withDirectorError(retry, nil)
		applyFinalProxyRewrite(retry)
		return true
	})
}

func applyTrafficSplitTarget(req *http.Request, override *traffic_split.Override, originalHost string) bool {
	if override == nil {
		return false
	}
	if override.HealthReporter != nil {
		enriched := proxy.WithHealthReporter(req, override.HealthReporter)
		if enriched != req {
			*req = *enriched
		}
		proxy.SetSelectedTarget(req, override.HealthTarget)
	}
	req.URL.Scheme = override.Scheme
	req.URL.Host = override.Host
	switch override.PassHost {
	case "", "pass":
		if originalHost != "" {
			req.Host = originalHost
		} else {
			req.Host = req.URL.Host
		}
	case "rewrite":
		if override.UpstreamHost != "" {
			req.Host = override.UpstreamHost
		} else {
			req.Host = override.Host
		}
	default:
		req.Host = override.Host
	}
	return true
}

func bufferRequestBodyIfNeeded(w http.ResponseWriter, r *http.Request) error {
	if !proxy_control.GetRequestBuffering(r) || r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	limit := proxy_control.GetRequestBufferingLimit(r)
	r.Body = http.MaxBytesReader(w, r.Body, limit)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if err := r.Body.Close(); err != nil {
		return err
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	r.ContentLength = int64(len(body))
	return nil
}
