package proxy_mirror

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
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
}

const (
	priority = 1010
	name     = "proxy-mirror"
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
    }
  },
  "required": ["host"]
}
`

type Config struct {
	Host           string  `json:"host"`
	Path           string  `json:"path,omitempty"`
	PathConcatMode string  `json:"path_concat_mode,omitempty"`
	SampleRatio    float64 `json:"sample_ratio,omitempty"`
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

	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		r = apisixctx.WithBeforeProxyHook(r, p.mirrorFinalizedRequest)
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) mirrorFinalizedRequest(r *http.Request) {
	if !p.shouldMirror() {
		return
	}
	body, err := readAndRestoreBody(r)
	if err != nil {
		logger.Errorf("proxy-mirror read request body: %s", err)
		return
	}
	mirrorReq, err := p.buildMirrorRequest(r, body)
	if err != nil {
		logger.Errorf("proxy-mirror build request to %s: %s", p.config.Host, err)
		return
	}
	go p.sendMirror(mirrorReq)
}

func readAndRestoreBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
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
	mirrorReq.Header = r.Header.Clone()
	mirrorReq.Host = r.Host

	return mirrorReq, nil
}

func (p *Plugin) mirrorURL(r *http.Request) (string, error) {
	hostURL, err := url.Parse(p.config.Host)
	if err != nil {
		return "", err
	}

	mirrorPath, rawQuery := effectiveRequestTarget(r)
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

func effectiveRequestTarget(r *http.Request) (string, string) {
	path, rawQuery := r.URL.Path, r.URL.RawQuery
	rewrite, _ := r.Context().Value(apisixctx.ProxyRewriteKey).(map[string]any)
	uri, _ := rewrite["uri"].(string)
	if uri == "" {
		return path, rawQuery
	}
	rewritten, err := url.ParseRequestURI(uri)
	if err != nil {
		return path, rawQuery
	}
	return rewritten.Path, rewritten.RawQuery
}

func (p *Plugin) sendMirror(req *http.Request) {
	client := p.client
	if strings.HasPrefix(p.config.Host, "grpc://") {
		client = p.h2cClient
	}
	resp, err := client.Do(req)
	if err != nil {
		logger.Errorf("proxy-mirror request to %s failed: %s", req.URL.Host, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
}
