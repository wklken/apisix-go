package proxy

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"net"
	"net/http"
	"time"

	"github.com/wklken/apisix-go/pkg/logger"
	"golang.org/x/net/http2"
)

const (
	// DefaultDialTimeout when connecting to a backend server.
	DefaultDialTimeout = 30 * time.Second

	// DefaultIdleConnsPerHost the default value set for http.Transport.MaxIdleConnsPerHost.
	// krakend is 250 / janus is 64
	DefaultMaxIdleConnsPerHost = 250

	// DefaultMaxIdleConns the default value set for http.Transport.MaxIdleConns.
	DefaultMaxIdleConns = 1024

	// DefaultMaxConnsPerHost the default value set for http.Transport.MaxConnsPerHost.
	DefaultMaxConnsPerHost = 1024

	// DefaultIdleConnTimeout is the default value for the the maximum amount of time an idle
	// (keep-alive) connection will remain idle before closing itself.
	DefaultIdleConnTimeout = 90 * time.Second
)

type TransportOption struct {
	maxIdleConnections        int
	maxIdleConnectionsPerHost int
	maxConnectionsPerHost     int
	insecureSkipVerify        bool
	tlsClientCertificate      tls.Certificate
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

	if ob.opt.maxIdleConnections <= 0 {
		ob.opt.maxIdleConnections = DefaultMaxIdleConns
	}

	if ob.opt.maxIdleConnectionsPerHost <= 0 {
		ob.opt.maxIdleConnectionsPerHost = DefaultMaxIdleConnsPerHost
	}

	if ob.opt.maxConnectionsPerHost <= 0 {
		ob.opt.maxConnectionsPerHost = DefaultMaxConnsPerHost
	}

	if ob.opt.idleConnTimeout == 0 {
		ob.opt.idleConnTimeout = DefaultIdleConnTimeout
	}

	return cloneTransportOption(ob.opt)
}

// WithInsecureSkipVerify sets tls config insecure skip verify
func (ob *TransportOptionBuilder) WithInsecureSkipVerify(value bool) *TransportOptionBuilder {
	ob.opt.insecureSkipVerify = value
	return ob
}

// WithTLSClientCertificate configures the certificate presented to HTTPS
// upstreams. The certificate bytes are cloned so later caller mutations do
// not change the immutable transport option.
func (ob *TransportOptionBuilder) WithTLSClientCertificate(certificate tls.Certificate) *TransportOptionBuilder {
	ob.opt.tlsClientCertificate = cloneTLSCertificate(certificate)
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

// WithMaxIdleConnections sets the global maximum number of idle (keep-alive)
// connections across all upstream hosts. Zero selects DefaultMaxIdleConns.
func (ob *TransportOptionBuilder) WithMaxIdleConnections(value int) *TransportOptionBuilder {
	ob.opt.maxIdleConnections = value
	return ob
}

// WithMaxConnectionsPerHost sets the maximum number of concurrent
// connections per upstream host. Zero selects DefaultMaxConnsPerHost.
func (ob *TransportOptionBuilder) WithMaxConnectionsPerHost(value int) *TransportOptionBuilder {
	ob.opt.maxConnectionsPerHost = value
	return ob
}

// transportKeyIdentity is the deterministic, complete serialization of every
// effective value that changes transport behavior. It feeds upstream cluster
// identity so a cluster is only reused for byte-identical effective config.
type transportKeyIdentity struct {
	MaxIdleConns                    int
	MaxIdleConnsPerHost             int
	MaxConnsPerHost                 int
	InsecureSkipVerify              bool
	TLSClientCertificateFingerprint [sha256.Size]byte
	DialTimeout                     time.Duration
	ResponseHeaderTimeout           time.Duration
	IdleConnTimeout                 time.Duration
}

func (t TransportOption) keyIdentity() transportKeyIdentity {
	return transportKeyIdentity{
		MaxIdleConns:                    t.maxIdleConnections,
		MaxIdleConnsPerHost:             t.maxIdleConnectionsPerHost,
		MaxConnsPerHost:                 t.maxConnectionsPerHost,
		InsecureSkipVerify:              t.insecureSkipVerify,
		TLSClientCertificateFingerprint: tlsClientCertificateFingerprint(t.tlsClientCertificate),
		DialTimeout:                     t.dialTimeout,
		ResponseHeaderTimeout:           t.responseHeaderTimeout,
		IdleConnTimeout:                 t.idleConnTimeout,
	}
}

func tlsClientCertificateFingerprint(certificate tls.Certificate) [sha256.Size]byte {
	if len(certificate.Certificate) > 0 {
		hash := sha256.New()
		var length [8]byte
		for _, der := range certificate.Certificate {
			binary.BigEndian.PutUint64(length[:], uint64(len(der)))
			_, _ = hash.Write(length[:])
			_, _ = hash.Write(der)
		}
		var fingerprint [sha256.Size]byte
		copy(fingerprint[:], hash.Sum(nil))
		return fingerprint
	}
	if certificate.Leaf != nil && len(certificate.Leaf.Raw) > 0 {
		return sha256.Sum256(certificate.Leaf.Raw)
	}
	return [sha256.Size]byte{}
}

func cloneTransportOption(option TransportOption) TransportOption {
	option.tlsClientCertificate = cloneTLSCertificate(option.tlsClientCertificate)
	return option
}

func cloneTLSCertificate(certificate tls.Certificate) tls.Certificate {
	clone := certificate
	clone.Certificate = cloneByteSlices(certificate.Certificate)
	clone.SupportedSignatureAlgorithms = append([]tls.SignatureScheme(nil), certificate.SupportedSignatureAlgorithms...)
	clone.OCSPStaple = append([]byte(nil), certificate.OCSPStaple...)
	clone.SignedCertificateTimestamps = cloneByteSlices(certificate.SignedCertificateTimestamps)
	return clone
}

func cloneByteSlices(value [][]byte) [][]byte {
	if value == nil {
		return nil
	}
	clone := make([][]byte, len(value))
	for index, bytes := range value {
		clone[index] = append([]byte(nil), bytes...)
	}
	return clone
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
	tlsConfig := &tls.Config{InsecureSkipVerify: t.insecureSkipVerify}
	if len(t.tlsClientCertificate.Certificate) > 0 || t.tlsClientCertificate.PrivateKey != nil {
		tlsConfig.Certificates = []tls.Certificate{cloneTLSCertificate(t.tlsClientCertificate)}
	}

	tr := &http.Transport{
		DisableCompression: true,
		DialContext: (&net.Dialer{
			Timeout:   t.dialTimeout,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
		IdleConnTimeout:       t.idleConnTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: t.responseHeaderTimeout,
		MaxIdleConns:          t.maxIdleConnections,
		MaxIdleConnsPerHost:   t.maxIdleConnectionsPerHost,
		MaxConnsPerHost:       t.maxConnectionsPerHost,
		TLSClientConfig:       tlsConfig,
	}

	if err := http2.ConfigureTransport(tr); err != nil {
		logger.Errorf("configure HTTP/2 transport fail, upstream requests fall back to HTTP/1.1: %s", err)
	}

	return tr
}
