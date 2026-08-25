package proxy_mirror

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/proxy"
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

	mirrorMu        sync.Mutex
	mirrorAdmission chan struct{}
	mirrorCtx       context.Context
	mirrorCancel    context.CancelFunc
	mirrorWG        sync.WaitGroup
	mirrorStopDone  chan struct{}
	mirrorStopped   bool
}

const (
	priority = 1010
	name     = "proxy-mirror"

	// maxInFlightMirrors bounds best-effort detached mirror requests per plugin.
	// Admission is non-blocking so a saturated mirror never delays the primary.
	maxInFlightMirrors = 16
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
	p.client = &http.Client{
		Timeout:   5 * time.Second,
		Transport: proxy.NewTransport((&proxy.TransportOptionBuilder{}).Build()),
	}
	p.h2cClient = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, address string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, address)
			},
		},
	}
	if baseURL, err := url.Parse(p.config.Host); err == nil {
		p.baseURL = baseURL
	}
	p.mirrorMu.Lock()
	if p.mirrorCtx == nil && !p.mirrorStopped {
		p.mirrorCtx, p.mirrorCancel = context.WithCancel(context.Background())
		p.mirrorAdmission = make(chan struct{}, maxInFlightMirrors)
		p.mirrorStopDone = make(chan struct{})
	}
	p.mirrorMu.Unlock()

	return nil
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
	ctx, admitted := p.admitMirror()
	if !admitted {
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
	p.startMirror(ctx, mirrorReq)
	return nil
}

func (p *Plugin) admitMirror() (context.Context, bool) {
	p.mirrorMu.Lock()
	defer p.mirrorMu.Unlock()

	if p.mirrorStopped || p.mirrorCtx == nil || p.mirrorAdmission == nil {
		return nil, false
	}
	select {
	case p.mirrorAdmission <- struct{}{}:
		return p.mirrorCtx, true
	default:
		return nil, false
	}
}

func (p *Plugin) startMirror(ctx context.Context, req *http.Request) {
	p.mirrorMu.Lock()
	if p.mirrorStopped {
		p.mirrorMu.Unlock()
		p.releaseMirrorAdmission()
		return
	}
	p.mirrorWG.Add(1)
	req = req.WithContext(ctx)
	go func() {
		defer p.mirrorWG.Done()
		defer p.releaseMirrorAdmission()
		p.sendMirror(req)
	}()
	p.mirrorMu.Unlock()
}

func (p *Plugin) releaseMirrorAdmission() {
	<-p.mirrorAdmission
}

func (p *Plugin) Stop() {
	p.mirrorMu.Lock()
	if p.mirrorStopped {
		done := p.mirrorStopDone
		p.mirrorMu.Unlock()
		if done != nil {
			<-done
		}
		return
	}
	p.mirrorStopped = true
	if p.mirrorStopDone == nil {
		p.mirrorStopDone = make(chan struct{})
	}
	done := p.mirrorStopDone
	cancel := p.mirrorCancel
	p.mirrorMu.Unlock()

	if cancel != nil {
		cancel()
	}
	p.mirrorWG.Wait()
	if p.client != nil {
		p.client.CloseIdleConnections()
	}
	if p.h2cClient != nil && p.h2cClient != p.client {
		p.h2cClient.CloseIdleConnections()
	}
	close(done)
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
