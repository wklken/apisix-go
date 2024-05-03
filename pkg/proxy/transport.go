package proxy

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"

	"golang.org/x/net/http2"
)

const (
	// DefaultDialTimeout when connecting to a backend server.
	DefaultDialTimeout = 30 * time.Second

	// DefaultIdleConnsPerHost the default value set for http.Transport.MaxIdleConnsPerHost.
	// krakend is 250 / janus is 64
	DefaultMaxIdleConnsPerHost = 250

	// DefaultIdleConnTimeout is the default value for the the maximum amount of time an idle
	// (keep-alive) connection will remain idle before closing itself.
	DefaultIdleConnTimeout = 90 * time.Second
)

type TransportOption struct {
	maxIdleConnectionsPerHost int
	insecureSkipVerify        bool
	dialTimeout               time.Duration
	responseHeaderTimeout     time.Duration
	idleConnTimeout           time.Duration
}

type TransportOptionBuilder struct {
	opt TransportOption
}

func (ob *TransportOptionBuilder) Build() TransportOption {
	// set default
	if ob.opt.dialTimeout <= 0 {
		ob.opt.dialTimeout = DefaultDialTimeout
	}

	if ob.opt.maxIdleConnectionsPerHost <= 0 {
		ob.opt.maxIdleConnectionsPerHost = DefaultMaxIdleConnsPerHost
	}

	if ob.opt.idleConnTimeout == 0 {
		ob.opt.idleConnTimeout = DefaultIdleConnTimeout
	}

	return ob.opt
}

// WithInsecureSkipVerify sets tls config insecure skip verify
func (ob *TransportOptionBuilder) WithInsecureSkipVerify(value bool) *TransportOptionBuilder {
	ob.opt.insecureSkipVerify = value
	return ob
}

// WithDialTimeout sets the dial context timeout
func (ob *TransportOptionBuilder) WithDialTimeout(d time.Duration) *TransportOptionBuilder {
	ob.opt.dialTimeout = d
	return ob
}

// WithResponseHeaderTimeout sets the response header timeout
func (ob *TransportOptionBuilder) WithResponseHeaderTimeout(d time.Duration) *TransportOptionBuilder {
	ob.opt.responseHeaderTimeout = d
	return ob
}

// WithIdleConnTimeout sets the maximum amount of time an idle
// (keep-alive) connection will remain idle before closing
// itself.
func (ob *TransportOptionBuilder) WithIdleConnTimeout(d time.Duration) *TransportOptionBuilder {
	ob.opt.idleConnTimeout = d
	return ob
}

// Same as net/http.Transport.MaxIdleConnsPerHost, but the default
// is 64. This value supports scenarios with relatively few remote
// hosts. When the routing table contains different hosts in the
// range of hundreds, it is recommended to set this options to a
// lower value.
func (ob *TransportOptionBuilder) WithMaxIdleConnectionsPerHost(value int) *TransportOptionBuilder {
	ob.opt.maxIdleConnectionsPerHost = value
	return ob
}

// reference: https://github.com/hellofresh/janus/blob/master/pkg/proxy/transport/transport.go
// reference: https://github.com/containous/traefik/blob/master/pkg/server/roundtripper.go

// TODO: 有没有必要加register, 复用transport. save newly created transport in registry, to try to reuse it in the future
// New creates a new instance of Transport with the given params
func NewTransport(t TransportOption) *http.Transport {
	// default in http.DefaultTransport: MaxIdleConnsPerHost = 2 / MaxIdleConns = 100

	// ! reference: https://github.com/TykTechnologies/tyk/issues/1560
	// don't set MaxIdleConns at all, leave it to be infinite,
	// it won't grow to more than max_idle_conns_per_host * upstreamHostsNumber anyways

	// reference: https://blog.cloudflare.com/the-complete-guide-to-golang-net-http-timeouts/
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   t.dialTimeout,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
		IdleConnTimeout:       t.idleConnTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: t.responseHeaderTimeout,
		// MaxIdleConns:          100,
		MaxIdleConnsPerHost: t.maxIdleConnectionsPerHost,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: t.insecureSkipVerify},
	}

	http2.ConfigureTransport(tr)

	return tr
}
