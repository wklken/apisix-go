package proxy_mirror

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/proxy"
	"github.com/wklken/apisix-go/pkg/runtime"
	"golang.org/x/net/http2"
)

type Plugin struct {
	base.BasePlugin
	config    Config
	client    *http.Client
	h2cClient *http.Client
	// baseURL is the parsed mirror host, reused per request; requests copy
	// it instead of reparsing the static configuration.
	baseURL *url.URL

	lookupNetIP     func(context.Context, string, string) ([]netip.Addr, error)
	resolverTimeout time.Duration

	mirrorMu        sync.Mutex
	mirrorAdmission chan struct{}
	mirrorStopped   bool
	mirrorStopOnce  sync.Once
}

const (
	priority = 1010
	name     = "proxy-mirror"

	// maxInFlightMirrors bounds best-effort detached mirror requests per plugin.
	// Admission is non-blocking so a saturated mirror never delays the primary.
	maxInFlightMirrors = 16
	mirrorTimeout      = 5 * time.Second
)

const schema = `
{
  "type": "object",
  "properties": {
    "host": {
      "type": "string",
      "pattern": "^(https?|grpcs?)://([0-9A-Za-z.-]+|\\[[0-9A-Fa-f:]+\\])(:[0-9]+)?$"
    },
    "path": {
      "type": "string",
      "pattern": "^/[^?&]+$"
    },
    "path_concat_mode": {
      "type": "string",
      "default": "replace",
      "enum": ["replace", "prefix"]
    },
    "sample_ratio": {
      "type": "number",
      "minimum": 0.00001,
      "maximum": 1,
      "default": 1
    },
    "max_body_size": {
      "type": "integer",
      "exclusiveMinimum": 0,
      "default": 1048576
    },
    "keep_sensitive_headers": {
      "type": "boolean",
      "default": false
    }
  },
  "required": ["host"]
}
`

var errProxyMirrorRequestLifecycle = errors.New("proxy-mirror request lifecycle is required")

type Config struct {
	Host                 string  `json:"host"`
	Path                 string  `json:"path,omitempty"`
	PathConcatMode       string  `json:"path_concat_mode,omitempty"`
	SampleRatio          float64 `json:"sample_ratio,omitempty"`
	MaxBodySize          int     `json:"max_body_size,omitempty"`
	KeepSensitiveHeaders bool    `json:"keep_sensitive_headers,omitempty"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.PathConcatMode == "" {
		p.config.PathConcatMode = "replace"
	}
	if p.config.SampleRatio == 0 {
		p.config.SampleRatio = 1
	}
	if p.config.MaxBodySize <= 0 {
		p.config.MaxBodySize = base.DefaultRequestBodyMaxBytes
	}
	if p.resolverTimeout <= 0 {
		p.resolverTimeout = mirrorTimeout
		if effective := p.StaticConfig(); effective != nil && effective.Config.Apisix.ResolverTimeout > 0 {
			p.resolverTimeout = time.Duration(effective.Config.Apisix.ResolverTimeout) * time.Second
		}
	}
	if p.lookupNetIP == nil {
		p.lookupNetIP = p.configuredResolver().LookupNetIP
	}
	dialContext := p.dialMirrorContext
	transport := proxy.NewTransport(
		(&proxy.TransportOptionBuilder{}).WithDialTimeout(mirrorTimeout).Build(),
	)
	transport.DialContext = dialContext
	p.client = &http.Client{
		Timeout:   mirrorTimeout,
		Transport: transport,
	}
	p.h2cClient = &http.Client{
		Timeout: mirrorTimeout,
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, address string, _ *tls.Config) (net.Conn, error) {
				return dialContext(ctx, network, address)
			},
		},
	}
	if baseURL, err := url.Parse(p.config.Host); err == nil {
		p.baseURL = baseURL
	}
	p.mirrorMu.Lock()
	if p.mirrorAdmission == nil && !p.mirrorStopped {
		p.mirrorAdmission = make(chan struct{}, maxInFlightMirrors)
	}
	p.mirrorMu.Unlock()

	return nil
}

func (p *Plugin) configuredResolver() *net.Resolver {
	effective := p.StaticConfig()
	if effective == nil || len(effective.Config.Apisix.DnsResolver) == 0 {
		return net.DefaultResolver
	}
	servers := append([]string(nil), effective.Config.Apisix.DnsResolver...)
	var next atomic.Uint64
	return &net.Resolver{
		PreferGo:     true,
		StrictErrors: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			index := next.Add(1) - 1
			address := mirrorDNSServerAddress(servers[index%uint64(len(servers))])
			return (&net.Dialer{Timeout: p.resolverTimeout}).DialContext(ctx, network, address)
		},
	}
}

func mirrorDNSServerAddress(server string) string {
	server = strings.TrimSpace(server)
	if ip := net.ParseIP(server); ip != nil {
		return net.JoinHostPort(server, "53")
	}
	if _, _, err := net.SplitHostPort(server); err == nil {
		return server
	}
	return net.JoinHostPort(server, "53")
}

func (p *Plugin) dialMirrorContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) != nil || p.lookupNetIP == nil {
		return (&net.Dialer{Timeout: mirrorTimeout}).DialContext(ctx, network, address)
	}
	lookupCtx, cancel := context.WithTimeout(ctx, p.resolverTimeout)
	addresses, err := p.lookupNetIP(lookupCtx, "ip", host)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("resolve mirror host %q: %w", host, err)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("resolve mirror host %q: no addresses", host)
	}

	var dialErrors []error
	for _, ip := range addresses {
		connection, dialErr := (&net.Dialer{Timeout: mirrorTimeout}).DialContext(
			ctx,
			network,
			net.JoinHostPort(ip.String(), port),
		)
		if dialErr == nil {
			return connection, nil
		}
		dialErrors = append(dialErrors, dialErr)
	}
	return nil, fmt.Errorf("dial mirror host %q: %w", host, errors.Join(dialErrors...))
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		r = apisixctx.WithBeforeProxyHookRegistration(r, apisixctx.BeforeProxyHookRegistration{
			Owner: name,
			Phase: "before_proxy",
			Hook:  p.mirrorFinalizedRequest,
		})
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) mirrorFinalizedRequest(r *http.Request) error {
	if !p.shouldMirror() {
		return nil
	}
	lifecycle := apisixctx.GetRequestLifecycle(r)
	if lifecycle == nil {
		return errProxyMirrorRequestLifecycle
	}
	if !p.admitMirror() {
		return nil
	}
	body, err := base.ReadRequestBodyLimited(r, p.config.MaxBodySize)
	if err != nil {
		p.releaseMirrorAdmission()
		return fmt.Errorf("proxy-mirror read request body: %w", err)
	}
	mirrorReq, err := p.buildMirrorRequest(r, body)
	if err != nil {
		p.releaseMirrorAdmission()
		logger.Errorf("proxy-mirror build request to %s: %s", p.config.Host, err)
		return nil
	}
	tasks := runtime.NewRequestTaskGroup(r.Context(), "request/proxy-mirror")
	if !lifecycle.AddFinalizer(name, tasks.Wait) {
		p.releaseMirrorAdmission()
		return errProxyMirrorRequestLifecycle
	}
	if err := tasks.Go(func(taskCtx context.Context) error {
		defer p.releaseMirrorAdmission()
		p.sendMirror(mirrorReq.WithContext(taskCtx))
		return nil
	}); err != nil {
		p.releaseMirrorAdmission()
		return err
	}
	return nil
}

func (p *Plugin) admitMirror() bool {
	p.mirrorMu.Lock()
	defer p.mirrorMu.Unlock()

	if p.mirrorStopped || p.mirrorAdmission == nil {
		return false
	}
	select {
	case p.mirrorAdmission <- struct{}{}:
		return true
	default:
		return false
	}
}

func (p *Plugin) releaseMirrorAdmission() {
	<-p.mirrorAdmission
}

func (p *Plugin) Stop() {
	p.mirrorStopOnce.Do(func() {
		p.mirrorMu.Lock()
		p.mirrorStopped = true
		client, h2cClient := p.client, p.h2cClient
		p.mirrorMu.Unlock()

		if client != nil {
			client.CloseIdleConnections()
		}
		if h2cClient != nil && h2cClient != client {
			h2cClient.CloseIdleConnections()
		}
	})
}

func (p *Plugin) shouldMirror() bool {
	if p.config.SampleRatio >= 1 {
		return true
	}
	return rand.Float64() < p.config.SampleRatio
}

func (p *Plugin) buildMirrorRequest(r *http.Request, body []byte) (*http.Request, error) {
	target, err := p.mirrorURL(r)
	if err != nil {
		return nil, err
	}

	mirrorReq, err := http.NewRequest(r.Method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	mirrorReq.Header = cloneMirrorHeaders(r.Header, p.config.KeepSensitiveHeaders)
	mirrorReq.Host = r.Host

	return mirrorReq, nil
}

func cloneMirrorHeaders(source http.Header, keepSensitive bool) http.Header {
	result := source.Clone()
	connectionTokens := make([]string, 0)
	for name, values := range source {
		lowerName := strings.ToLower(name)
		if lowerName == "connection" {
			for _, value := range values {
				for token := range strings.SplitSeq(value, ",") {
					if token = strings.TrimSpace(token); token != "" {
						connectionTokens = append(connectionTokens, token)
					}
				}
			}
		}
		if isMirrorHopByHopHeader(lowerName) || (!keepSensitive && isMirrorSensitiveHeader(lowerName)) {
			deleteHeader(result, name)
		}
	}
	for _, token := range connectionTokens {
		deleteHeader(result, token)
	}
	return result
}

func isMirrorHopByHopHeader(name string) bool {
	switch name {
	case "connection", "proxy-connection", "keep-alive", "te", "trailer",
		"transfer-encoding", "upgrade", "content-length", "host":
		return true
	default:
		return false
	}
}

func isMirrorSensitiveHeader(name string) bool {
	return name == "authorization" ||
		name == "proxy-authorization" ||
		name == "cookie" ||
		name == "set-cookie" ||
		name == "api-key" ||
		name == "apikey" ||
		name == "x-api-key" ||
		name == "x-functions-key" ||
		name == "x-goog-api-key" ||
		name == "x-rbac-token" ||
		strings.HasPrefix(name, "x-amz-")
}

func deleteHeader(headers http.Header, name string) {
	for existing := range headers {
		if strings.EqualFold(existing, name) {
			delete(headers, existing)
		}
	}
}

func (p *Plugin) mirrorURL(r *http.Request) (string, error) {
	var hostURL url.URL
	if p.baseURL != nil {
		hostURL = *p.baseURL
	} else {
		parsed, err := url.Parse(p.config.Host)
		if err != nil {
			return "", err
		}
		hostURL = *parsed
	}

	mirrorPath, rawQuery := r.URL.Path, r.URL.RawQuery
	if p.config.Path != "" {
		if p.config.PathConcatMode == "prefix" {
			mirrorPath = strings.TrimRight(p.config.Path, "/") + "/" + strings.TrimLeft(mirrorPath, "/")
		} else {
			mirrorPath = p.config.Path
		}
	}

	hostURL.Path = mirrorPath
	hostURL.RawQuery = rawQuery
	switch hostURL.Scheme {
	case "grpc":
		hostURL.Scheme = "http"
	case "grpcs":
		hostURL.Scheme = "https"
	}
	return hostURL.String(), nil
}

func (p *Plugin) sendMirror(req *http.Request) {
	client := p.client
	if strings.HasPrefix(p.config.Host, "grpc://") {
		client = p.h2cClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if req.Context().Err() != nil {
			return
		}
		logger.Errorf("proxy-mirror request to %s failed: %s", req.URL.Host, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
}
