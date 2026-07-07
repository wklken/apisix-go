package forward_auth

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
	client *http.Client
}

const (
	priority = 2002
	name     = "forward-auth"
)

const schema = `
{
  "type": "object",
  "properties": {
    "uri": {
      "type": "string"
    },
    "allow_degradation": {
      "type": "boolean",
      "default": false
    },
    "status_on_error": {
      "type": "integer",
      "minimum": 200,
      "maximum": 599,
      "default": 403
    },
    "request_method": {
      "type": "string",
      "default": "GET",
      "enum": ["GET", "POST"]
    },
    "max_req_body_size": {
      "type": "integer",
      "minimum": 1,
      "default": 67108864
    },
    "request_headers": {
      "type": "array",
      "default": {},
      "items": {
        "type": "string"
      }
    },
    "extra_headers": {
      "type": "object"
    },
    "upstream_headers": {
      "type": "array",
      "default": {},
      "items": {
        "type": "string"
      }
    },
    "client_headers": {
      "type": "array",
      "default": {},
      "items": {
        "type": "string"
      }
    },
    "timeout": {
      "type": "integer",
      "minimum": 1,
      "maximum": 60000,
      "default": 3000
    }
  },
  "required": ["uri"]
}
`

type Config struct {
	URI              string            `json:"uri"`
	AllowDegradation *bool             `json:"allow_degradation,omitempty"`
	StatusOnError    int               `json:"status_on_error,omitempty"`
	RequestMethod    string            `json:"request_method,omitempty"`
	MaxReqBodySize   int64             `json:"max_req_body_size,omitempty"`
	RequestHeaders   []string          `json:"request_headers,omitempty"`
	ExtraHeaders     map[string]string `json:"extra_headers,omitempty"`
	UpstreamHeaders  []string          `json:"upstream_headers,omitempty"`
	ClientHeaders    []string          `json:"client_headers,omitempty"`
	Timeout          int               `json:"timeout,omitempty"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.AllowDegradation == nil {
		b := false
		p.config.AllowDegradation = &b
	}
	if p.config.StatusOnError == 0 {
		p.config.StatusOnError = http.StatusForbidden
	}
	if p.config.RequestMethod == "" {
		p.config.RequestMethod = http.MethodGet
	}
	if p.config.MaxReqBodySize == 0 {
		p.config.MaxReqBodySize = 64 * 1024 * 1024
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 3000
	}

	p.client = &http.Client{
		Timeout: time.Duration(p.config.Timeout) * time.Millisecond,
	}

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		authResp, err := p.authorize(r)
		if err != nil {
			if *p.config.AllowDegradation {
				next.ServeHTTP(w, r)
				return
			}
			http.Error(w, http.StatusText(p.config.StatusOnError), p.config.StatusOnError)
			return
		}
		defer authResp.Body.Close()

		if authResp.StatusCode >= http.StatusMultipleChoices {
			p.copyConfiguredHeaders(w.Header(), authResp.Header, p.config.ClientHeaders)
			w.WriteHeader(authResp.StatusCode)
			io.Copy(w, authResp.Body)
			return
		}

		p.copyConfiguredHeaders(r.Header, authResp.Header, p.config.UpstreamHeaders)
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) authorize(r *http.Request) (*http.Response, error) {
	var body io.Reader
	if p.config.RequestMethod == http.MethodPost {
		reqBody, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		r.Body = io.NopCloser(bytes.NewReader(reqBody))
		if int64(len(reqBody)) > p.config.MaxReqBodySize {
			return nil, fmt.Errorf("request body too large")
		}
		body = bytes.NewReader(reqBody)
	}

	authReq, err := http.NewRequestWithContext(r.Context(), p.config.RequestMethod, p.config.URI, body)
	if err != nil {
		return nil, err
	}

	p.setForwardedHeaders(authReq, r)
	for _, header := range p.config.RequestHeaders {
		if value := r.Header.Get(header); value != "" {
			authReq.Header.Set(header, value)
		}
	}
	for header, value := range p.config.ExtraHeaders {
		authReq.Header.Set(header, value)
	}

	return p.client.Do(authReq)
}

func (p *Plugin) setForwardedHeaders(authReq *http.Request, r *http.Request) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	authReq.Header.Set("X-Forwarded-Proto", scheme)
	authReq.Header.Set("X-Forwarded-Method", r.Method)
	authReq.Header.Set("X-Forwarded-Host", r.Host)
	authReq.Header.Set("X-Forwarded-Uri", r.URL.RequestURI())
	authReq.Header.Set("X-Forwarded-For", remoteIP(r))
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func (p *Plugin) copyConfiguredHeaders(dst, src http.Header, names []string) {
	for _, name := range names {
		dst.Del(name)
		for _, value := range src.Values(name) {
			dst.Add(name, value)
		}
	}
}
