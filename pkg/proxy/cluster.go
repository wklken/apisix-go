package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"golang.org/x/net/http2"
)

// DefaultMaxInFlight bounds the number of concurrently active response bodies
// per cluster when the configuration does not select an explicit limit.
const DefaultMaxInFlight = 1024

// ErrClusterOverloaded is returned by a cluster RoundTripper when the
// configured in-flight limit is exhausted. Callers map it to an HTTP 503.
var ErrClusterOverloaded = errors.New("upstream cluster overloaded")

// ClusterKey is the digest of a cluster's complete effective configuration.
type ClusterKey [sha256.Size]byte

// ClusterConfig is the immutable, effective configuration of one upstream
// cluster. Every field is owned by the route generation that produces it and
// is interned by digest; the same value must always select the same cluster.
type ClusterConfig struct {
	Name              string
	Targets           map[string]int
	Priorities        map[string]int
	Checks            map[string]any
	Transport         TransportOption
	SendTimeout       time.Duration
	ReadTimeout       time.Duration
	Retries           int
	RetriesConfigured bool
	MaxInFlight       int
	HTTP2Cleartext    bool
}

// clusterKeyIdentity is the deterministic serialization used to derive
// ClusterKey. Targets are sorted so map iteration order never changes the
// digest, and the complete transport identity is embedded so any TLS,
// timeout, idle, or connection-cap change produces a new cluster.
type clusterKeyIdentity struct {
	Name              string
	Targets           []clusterKeyTarget
	Priorities        []clusterKeyPriority
	Checks            map[string]any
	Transport         transportKeyIdentity
	SendTimeout       time.Duration
	ReadTimeout       time.Duration
	Retries           int
	RetriesConfigured bool
	MaxInFlight       int
	HTTP2Cleartext    bool
}

type clusterKeyTarget struct {
	Target string
	Weight int
}

type clusterKeyPriority struct {
	Target   string
	Priority int
}

// Key computes the identity digest for this effective cluster configuration.
// An error means the configuration cannot be serialized deterministically and
// the caller must fail rather than reuse a partial digest.
func (c ClusterConfig) Key() (ClusterKey, error) {
	identity := clusterKeyIdentity{
		Name:              c.Name,
		Targets:           sortedClusterTargets(c.Targets),
		Priorities:        sortedClusterPriorities(c.Priorities),
		Checks:            c.Checks,
		Transport:         c.Transport.keyIdentity(),
		SendTimeout:       c.SendTimeout,
		ReadTimeout:       c.ReadTimeout,
		Retries:           c.Retries,
		RetriesConfigured: c.RetriesConfigured,
		MaxInFlight:       c.MaxInFlight,
		HTTP2Cleartext:    c.HTTP2Cleartext,
	}
	raw, err := json.Marshal(identity)
	if err != nil {
		return ClusterKey{}, err
	}
	return sha256.Sum256(raw), nil
}

func sortedClusterTargets(targets map[string]int) []clusterKeyTarget {
	keys := make([]string, 0, len(targets))
	for target := range targets {
		keys = append(keys, target)
	}
	sort.Strings(keys)
	result := make([]clusterKeyTarget, 0, len(keys))
	for _, target := range keys {
		result = append(result, clusterKeyTarget{Target: target, Weight: targets[target]})
	}
	return result
}

func sortedClusterPriorities(priorities map[string]int) []clusterKeyPriority {
	if len(priorities) == 0 {
		return nil
	}
	keys := make([]string, 0, len(priorities))
	for target := range priorities {
		keys = append(keys, target)
	}
	sort.Strings(keys)
	result := make([]clusterKeyPriority, 0, len(keys))
	for _, target := range keys {
		result = append(result, clusterKeyPriority{Target: target, Priority: priorities[target]})
	}
	return result
}

// Cluster owns one base transport, one retry/progress wrapper chain, one load
// balancer, an optional active-probe owner, and a non-queueing in-flight
// limiter. It is immutable after construction except for close.
type Cluster struct {
	config      ClusterConfig
	key         ClusterKey
	lb          LoadBalancer
	transport   http.RoundTripper
	observer    ClusterObserver
	health      healthChecker
	closeIdle   func()
	closed      atomic.Bool
	closeOnce   sync.Once
	maxInFlight int
}

func newCluster(config ClusterConfig, observer ClusterObserver) (*Cluster, error) {
	if config.HTTP2Cleartext {
		transport := newCleartextHTTP2Transport(config.Transport)
		base := newResponseHeaderTimeoutTransport(transport, config.Transport.responseHeaderTimeout)
		return newClusterWithTransport(config, observer, base, transport.CloseIdleConnections)
	}
	transport := NewTransport(config.Transport)
	return newClusterWithTransport(config, observer, transport, transport.CloseIdleConnections)
}

func newCleartextHTTP2Transport(option TransportOption) *http2.Transport {
	return &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, address string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{Timeout: option.dialTimeout, KeepAlive: 30 * time.Second}).DialContext(
				ctx,
				network,
				address,
			)
		},
	}
}

// newClusterWithTransport builds a cluster around an injected base transport
// and idle-close callback. Production passes the configured *http.Transport;
// tests inject a deterministic transport so admission, retry, and health
// behavior can be observed without real connections.
func newClusterWithTransport(
	config ClusterConfig,
	observer ClusterObserver,
	base http.RoundTripper,
	closeIdle func(),
) (*Cluster, error) {
	key, err := config.Key()
	if err != nil {
		return nil, err
	}
	maxInFlight := config.MaxInFlight
	if maxInFlight <= 0 {
		maxInFlight = DefaultMaxInFlight
	}

	var lb LoadBalancer
	if len(config.Targets) > 0 {
		lb, err = newUpstreamLoadBalanceWithPriorities(config.Targets, config.Priorities, config.Checks)
		if err != nil {
			return nil, err
		}
	}

	observeRetry := func(result string) {
		observer.ObserveRetry(config.Name, result)
	}
	transport := NewProgressTimeoutTransport(base, config.SendTimeout, config.ReadTimeout)
	transport = NewRetryTransportWithObserver(transport, observeRetry)
	transport = newAdmissionTransport(transport, maxInFlight, config.Name, observer)

	cluster := &Cluster{
		config:      config,
		key:         key,
		lb:          lb,
		transport:   transport,
		observer:    observer,
		closeIdle:   closeIdle,
		maxInFlight: maxInFlight,
	}
	if healthAware, ok := lb.(*HealthAwareLoadBalance); ok {
		healthAware.setObserver(config.Name, observer)
		active, enabled, err := ParseActiveHealthConfig(config.Checks)
		if err != nil {
			return nil, err
		}
		if enabled {
			checker := newActiveHealthChecker(active, healthAware, config.Targets, config.Name, observer, base)
			checker.Start()
			cluster.health = checker
		}
	}
	return cluster, nil
}

// LoadBalancer returns the cluster's target selector.
func (c *Cluster) LoadBalancer() LoadBalancer {
	return c.lb
}

// RoundTripper returns the cluster's bounded, retrying, progress-aware
// transport chain. Overload admission is fail-fast: it never queues.
func (c *Cluster) RoundTripper() http.RoundTripper {
	return c.transport
}

// MaxInFlight returns the cluster's effective in-flight admission limit.
func (c *Cluster) MaxInFlight() int {
	return c.maxInFlight
}

// Close stops active health probes and closes idle upstream connections. It
// is safe to call more than once.
func (c *Cluster) Close() {
	c.closeOnce.Do(func() {
		if c.health != nil {
			c.health.Close()
		}
		if healthAware, ok := c.lb.(*HealthAwareLoadBalance); ok {
			healthAware.clearObserver()
		}
		if c.closeIdle != nil {
			c.closeIdle()
		}
		c.closed.Store(true)
	})
}

// Closed reports whether Close has been called. It exists only for
// deterministic lifecycle tests.
func (c *Cluster) Closed() bool {
	return c.closed.Load()
}

// admissionTransport bounds the number of concurrently active response bodies
// in a cluster. A token is acquired before RoundTrip and released exactly once
// when the response body is closed or reaches EOF, so a 1024-byte-limit
// cluster rejects the 1025th concurrent body with ErrClusterOverloaded.
type admissionTransport struct {
	base     http.RoundTripper
	tokens   chan struct{}
	observer ClusterObserver
	name     string
}

func newAdmissionTransport(
	base http.RoundTripper,
	limit int,
	name string,
	observer ClusterObserver,
) *admissionTransport {
	return &admissionTransport{
		base:     base,
		tokens:   make(chan struct{}, limit),
		observer: observer,
		name:     name,
	}
}

func (t *admissionTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	select {
	case t.tokens <- struct{}{}:
		t.observer.SetInFlight(t.name, 1)
	default:
		t.observer.ObserveRejected(t.name)
		return nil, ErrClusterOverloaded
	}
	response, err := t.base.RoundTrip(request)
	if err != nil {
		t.release()
		return response, err
	}
	if response == nil || response.Body == nil {
		t.release()
		return response, nil
	}
	response.Body = wrapReleaseBody(response.Body, t.release)
	return response, nil
}

func (t *admissionTransport) release() {
	<-t.tokens
	t.observer.SetInFlight(t.name, -1)
}

// releaseBody releases the cluster admission token exactly once when the
// underlying response body is closed or fully drained.
type releaseBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *releaseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.once.Do(b.release)
	}
	return n, err
}

func (b *releaseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}

type releaseDuplexBody struct {
	*releaseBody
	duplex io.ReadWriteCloser
}

func (b *releaseDuplexBody) Write(payload []byte) (int, error) {
	n, err := b.duplex.Write(payload)
	if err != nil {
		b.once.Do(b.release)
	}
	return n, err
}

func wrapReleaseBody(body io.ReadCloser, release func()) io.ReadCloser {
	releaseBody := &releaseBody{ReadCloser: body, release: release}
	duplex, ok := body.(io.ReadWriteCloser)
	if !ok {
		return releaseBody
	}
	return &releaseDuplexBody{releaseBody: releaseBody, duplex: duplex}
}
