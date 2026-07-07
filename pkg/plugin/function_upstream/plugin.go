package function_upstream

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Processor func(*http.Request, Config)

type Plugin struct {
	base.BasePlugin
	Config    Config
	Processor Processor
	Client    *http.Client
}

type Config struct {
	FunctionURI      string `json:"function_uri"`
	Timeout          int    `json:"timeout,omitempty"`
	SSLVerify        *bool  `json:"ssl_verify,omitempty"`
	Keepalive        *bool  `json:"keepalive,omitempty"`
	KeepaliveTimeout int    `json:"keepalive_timeout,omitempty"`
	KeepalivePool    int    `json:"keepalive_pool,omitempty"`
}

func (p *Plugin) PostInit() error {
	if p.Config.Timeout == 0 {
		p.Config.Timeout = 3000
	}
	if p.Config.KeepaliveTimeout == 0 {
		p.Config.KeepaliveTimeout = 60000
	}
	if p.Config.KeepalivePool == 0 {
		p.Config.KeepalivePool = 5
	}
	if p.Config.SSLVerify == nil {
		value := true
		p.Config.SSLVerify = &value
	}
	if p.Config.Keepalive == nil {
		value := true
		p.Config.Keepalive = &value
	}
	if p.Client == nil {
		p.Client = &http.Client{
			Timeout:   time.Duration(p.Config.Timeout) * time.Millisecond,
			Transport: p.transport(),
		}
	}

	return nil
}

func (p *Plugin) transport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = !*p.Config.Keepalive
	transport.IdleConnTimeout = time.Duration(p.Config.KeepaliveTimeout) * time.Millisecond
	transport.MaxIdleConnsPerHost = p.Config.KeepalivePool
	if !*p.Config.SSLVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return transport
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamReq, err := p.buildRequest(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if p.Processor != nil {
			p.Processor(upstreamReq, p.Config)
		}

		res, err := p.Client.Do(upstreamReq)
		if err != nil {
			http.Error(w, "failed to process "+p.Name, http.StatusServiceUnavailable)
			return
		}
		defer res.Body.Close()

		writeResponse(w, res)
	})
}

func (p *Plugin) buildRequest(r *http.Request) (*http.Request, error) {
	target, err := url.Parse(p.Config.FunctionURI)
	if err != nil {
		return nil, fmt.Errorf("invalid function_uri: %w", err)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	target.RawQuery = r.URL.RawQuery
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	upstreamReq.Header = r.Header.Clone()
	upstreamReq.Host = target.Host
	upstreamReq.Header.Set("Host", target.Host)

	return upstreamReq, nil
}

func writeResponse(w http.ResponseWriter, res *http.Response) {
	for field, values := range res.Header {
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	w.WriteHeader(res.StatusCode)
	io.Copy(w, res.Body)
}
