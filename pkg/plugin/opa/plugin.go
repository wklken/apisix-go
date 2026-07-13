package opa

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
)

type Plugin struct {
	base.BasePlugin
	config  Config
	client  *http.Client
	route   resource.Route
	service resource.Service
}

const (
	priority = 2001
	name     = "opa"
)

const schema = `
{
  "type": "object",
  "properties": {
    "host": {
      "type": "string"
    },
    "ssl_verify": {
      "type": "boolean",
      "default": true
    },
    "policy": {
      "type": "string"
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "maximum": 60000,
      "default": 3000
    },
    "keepalive": {
      "type": "boolean",
      "default": true
    },
    "send_headers_upstream": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "string"
      }
    },
    "keepalive_timeout": {
      "type": "integer",
      "minimum": 1000,
      "default": 60000
    },
    "keepalive_pool": {
      "type": "integer",
      "minimum": 1,
      "default": 5
    },
    "with_route": {
      "type": "boolean",
      "default": false
    },
    "with_service": {
      "type": "boolean",
      "default": false
    },
    "with_consumer": {
      "type": "boolean",
      "default": false
    }
  },
  "required": ["host", "policy"]
}
`

type Config struct {
	Host                string   `json:"host"`
	SSLVerify           *bool    `json:"ssl_verify,omitempty"`
	Policy              string   `json:"policy"`
	Timeout             int      `json:"timeout,omitempty"`
	Keepalive           *bool    `json:"keepalive,omitempty"`
	SendHeadersUpstream []string `json:"send_headers_upstream,omitempty"`
	KeepaliveTimeout    int      `json:"keepalive_timeout,omitempty"`
	KeepalivePool       int      `json:"keepalive_pool,omitempty"`
	WithRoute           bool     `json:"with_route,omitempty"`
	WithService         bool     `json:"with_service,omitempty"`
	WithConsumer        bool     `json:"with_consumer,omitempty"`
}

type opaRequest struct {
	Input opaInput `json:"input"`
}

type opaInput struct {
	Type     string         `json:"type"`
	Request  opaHTTPRequest `json:"request"`
	Vars     map[string]any `json:"var"`
	Route    any            `json:"route,omitempty"`
	Service  any            `json:"service,omitempty"`
	Consumer any            `json:"consumer,omitempty"`
}

type opaHTTPRequest struct {
	Scheme  string            `json:"scheme"`
	Method  string            `json:"method"`
	Host    string            `json:"host"`
	Port    int               `json:"port"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Query   url.Values        `json:"query"`
}

type opaResponse struct {
	Result *opaDecision `json:"result"`
}

type opaDecision struct {
	Allow      bool              `json:"allow"`
	Reason     any               `json:"reason,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	StatusCode int               `json:"status_code,omitempty"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.SSLVerify == nil {
		b := true
		p.config.SSLVerify = &b
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 3000
	}
	if p.config.Keepalive == nil {
		b := true
		p.config.Keepalive = &b
	}
	if p.config.KeepaliveTimeout == 0 {
		p.config.KeepaliveTimeout = 60000
	}
	if p.config.KeepalivePool == 0 {
		p.config.KeepalivePool = 5
	}

	p.client = &http.Client{
		Timeout:   time.Duration(p.config.Timeout) * time.Millisecond,
		Transport: p.transport(),
	}

	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) SetResourceContext(route resource.Route, service resource.Service) {
	p.route = route
	p.service = service
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		decision, statusCode, err := p.queryOPA(r)
		if err != nil {
			http.Error(w, http.StatusText(statusCode), statusCode)
			return
		}

		if !decision.Allow {
			for key, value := range decision.Headers {
				w.Header().Set(key, value)
			}
			if decision.StatusCode == 0 {
				decision.StatusCode = http.StatusForbidden
			}
			w.WriteHeader(decision.StatusCode)
			if decision.Reason != nil {
				_, _ = w.Write([]byte(reasonString(decision.Reason)))
			}
			return
		}

		p.copyHeadersToUpstream(r, decision.Headers)
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) queryOPA(r *http.Request) (*opaDecision, int, error) {
	body, err := json.Marshal(p.buildOPARequest(r))
	if err != nil {
		return nil, http.StatusServiceUnavailable, err
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, p.endpoint(), bytes.NewReader(body))
	if err != nil {
		return nil, http.StatusServiceUnavailable, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, http.StatusForbidden, err
	}
	defer func() { _ = resp.Body.Close() }()

	var opaResp opaResponse
	if err := json.NewDecoder(resp.Body).Decode(&opaResp); err != nil {
		return nil, http.StatusServiceUnavailable, err
	}
	if opaResp.Result == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("OPA result is missing")
	}

	return opaResp.Result, 0, nil
}

func (p *Plugin) buildOPARequest(r *http.Request) opaRequest {
	host, port := splitHostPort(r)
	input := opaInput{
		Type: "http",
		Request: opaHTTPRequest{
			Scheme:  scheme(r),
			Method:  r.Method,
			Host:    host,
			Port:    port,
			Path:    r.URL.Path,
			Headers: headers(r.Header),
			Query:   r.URL.Query(),
		},
		Vars: map[string]any{
			"server_addr": "",
			"server_port": "",
			"remote_addr": remoteAddr(r),
			"remote_port": remotePort(r),
			"timestamp":   time.Now().Unix(),
		},
	}

	if p.config.WithConsumer {
		if consumer := ctx.GetApisixVar(r, "$consumer"); consumer != "" {
			input.Consumer = consumer
		}
	}
	if p.config.WithRoute {
		if route := p.opaRoute(r); route != nil {
			input.Route = route
		}
	}
	if p.config.WithService {
		if service := p.opaService(r); service != nil {
			input.Service = service
		}
	}

	return opaRequest{Input: input}
}

func (p *Plugin) opaRoute(r *http.Request) any {
	if p.route.ID != "" {
		return p.route
	}
	if route := localRoute(r); len(route) > 0 {
		return route
	}
	return nil
}

func (p *Plugin) opaService(r *http.Request) any {
	if p.service.ID != "" {
		return p.service
	}
	if service := localService(r); len(service) > 0 {
		return service
	}
	return nil
}

func localRoute(r *http.Request) map[string]string {
	route := map[string]string{}
	for output, key := range map[string]string{
		"id":   "$route_id",
		"name": "$route_name",
		"uri":  "$matched_uri",
	} {
		if value, ok := ctx.GetApisixVar(r, key).(string); ok && value != "" {
			route[output] = value
		}
	}
	return route
}

func localService(r *http.Request) map[string]string {
	service := map[string]string{}
	for output, key := range map[string]string{
		"id":   "$service_id",
		"name": "$service_name",
	} {
		if value, ok := ctx.GetApisixVar(r, key).(string); ok && value != "" {
			service[output] = value
		}
	}
	return service
}

func (p *Plugin) endpoint() string {
	return strings.TrimRight(p.config.Host, "/") + "/v1/data/" + strings.TrimLeft(p.config.Policy, "/")
}

func (p *Plugin) transport() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = !*p.config.Keepalive
	transport.MaxIdleConnsPerHost = p.config.KeepalivePool
	transport.IdleConnTimeout = time.Duration(p.config.KeepaliveTimeout) * time.Millisecond
	if !*p.config.SSLVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return transport
}

func (p *Plugin) copyHeadersToUpstream(r *http.Request, headers map[string]string) {
	for _, name := range p.config.SendHeadersUpstream {
		value, ok := headers[name]
		if !ok {
			r.Header.Del(name)
			continue
		}
		r.Header.Set(name, value)
	}
}

func splitHostPort(r *http.Request) (string, int) {
	host := r.Host
	if host == "" && r.URL != nil {
		host = r.URL.Host
	}

	hostname, portValue, err := net.SplitHostPort(host)
	if err == nil {
		port, _ := strconv.Atoi(portValue)
		return hostname, port
	}

	if strings.Contains(host, ":") {
		return host, 0
	}

	if r.TLS != nil {
		return host, 443
	}
	return host, 80
}

func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if r.URL != nil && r.URL.Scheme != "" {
		return r.URL.Scheme
	}
	return "http"
}

func headers(src http.Header) map[string]string {
	dst := make(map[string]string, len(src))
	for key := range src {
		dst[key] = src.Get(key)
	}
	return dst
}

func remoteAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func remotePort(r *http.Request) string {
	_, port, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return port
	}
	return ""
}

func reasonString(reason any) string {
	switch v := reason.(type) {
	case string:
		return v
	default:
		body, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(body)
	}
}
