package forward_auth

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/logger"
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
    "ssl_verify": {
      "type": "boolean",
      "default": true
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
      "type": "object",
      "minProperties": 1,
      "patternProperties": {
        "^[^:]+$": {
          "type": "string"
        }
      },
      "additionalProperties": false
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
    },
    "keepalive": {
      "type": "boolean",
      "default": true
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
    }
  },
  "required": ["uri"]
}
`

type Config struct {
	URI              string            `json:"uri"`
	AllowDegradation *bool             `json:"allow_degradation,omitempty"`
	StatusOnError    int               `json:"status_on_error,omitempty"`
	SSLVerify        *bool             `json:"ssl_verify,omitempty"`
	RequestMethod    string            `json:"request_method,omitempty"`
	MaxReqBodySize   int64             `json:"max_req_body_size,omitempty"`
	RequestHeaders   []string          `json:"request_headers,omitempty"`
	ExtraHeaders     map[string]string `json:"extra_headers,omitempty"`
	UpstreamHeaders  []string          `json:"upstream_headers,omitempty"`
	ClientHeaders    []string          `json:"client_headers,omitempty"`
	Timeout          int               `json:"timeout,omitempty"`
	Keepalive        *bool             `json:"keepalive,omitempty"`
	KeepaliveTimeout int               `json:"keepalive_timeout,omitempty"`
	KeepalivePool    int               `json:"keepalive_pool,omitempty"`
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
	if p.config.SSLVerify == nil {
		b := true
		p.config.SSLVerify = &b
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

func (p *Plugin) transport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = !*p.config.Keepalive
	transport.IdleConnTimeout = time.Duration(p.config.KeepaliveTimeout) * time.Millisecond
	transport.MaxIdleConnsPerHost = p.config.KeepalivePool
	if !*p.config.SSLVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	return transport
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		authResp, err := p.authorize(r)
		if err != nil {
			if errors.Is(err, errRequestBodyTooLarge) {
				logger.Errorf("failed to read request body: %s", err)
				http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
				return
			}
			if *p.config.AllowDegradation {
				next.ServeHTTP(w, r)
				return
			}
			logger.Warnf("failed to process forward auth, err: %s", err)
			http.Error(w, http.StatusText(p.config.StatusOnError), p.config.StatusOnError)
			return
		}
		defer func() { _ = authResp.Body.Close() }()

		if authResp.StatusCode >= http.StatusMultipleChoices {
			p.copyConfiguredHeaders(w.Header(), authResp.Header, p.config.ClientHeaders)
			w.WriteHeader(authResp.StatusCode)
			_, _ = io.Copy(w, authResp.Body)
			return
		}

		p.copyConfiguredHeaders(r.Header, authResp.Header, p.config.UpstreamHeaders)
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

var errRequestBodyTooLarge = errors.New("request body too large")

func (p *Plugin) authorize(r *http.Request) (*http.Response, error) {
	var body io.Reader
	if p.config.RequestMethod == http.MethodPost {
		reqBody, err := io.ReadAll(io.LimitReader(r.Body, p.config.MaxReqBodySize+1))
		if err != nil {
			return nil, err
		}
		r.Body = io.NopCloser(bytes.NewReader(reqBody))
		if int64(len(reqBody)) > p.config.MaxReqBodySize {
			return nil, errRequestBodyTooLarge
		}
		body = bytes.NewReader(reqBody)
	}

	authReq, err := http.NewRequestWithContext(r.Context(), p.config.RequestMethod, p.config.URI, body)
	if err != nil {
		return nil, err
	}

	p.setForwardedHeaders(authReq, r)
	if p.config.RequestMethod == http.MethodPost {
		if values := r.Header.Values("Content-Encoding"); len(values) > 0 {
			authReq.Header["Content-Encoding"] = append([]string(nil), values...)
		}
	}
	for _, header := range p.config.RequestHeaders {
		if _, generated := authReq.Header[http.CanonicalHeaderKey(header)]; generated {
			continue
		}
		if value := r.Header.Get(header); value != "" {
			authReq.Header.Set(header, value)
		}
	}
	postArgs := postArgumentCache{}
	for header, value := range p.config.ExtraHeaders {
		authReq.Header.Set(header, resolveValue(r, value, &postArgs))
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
	authReq.Header.Set("X-Forwarded-For", base.RemoteIP(r.RemoteAddr))
}

var variablePattern = regexp.MustCompile(`\$post_arg\.[A-Za-z0-9_]+|\$[A-Za-z0-9_]+`)

type postArgumentCache struct {
	loaded bool
	values map[string]any
}

func resolveValue(r *http.Request, value string, postArgs *postArgumentCache) string {
	return variablePattern.ReplaceAllStringFunc(value, func(variable string) string {
		if name, ok := strings.CutPrefix(variable, "$post_arg."); ok {
			return postArgument(r, name, postArgs)
		}
		return base.RequestVar(r, strings.TrimPrefix(variable, "$"), 0)
	})
}

func postArgument(r *http.Request, name string, cache *postArgumentCache) string {
	if !cache.loaded {
		cache.loaded = true
		body, err := base.ReadRequestBody(r)
		if err != nil {
			return ""
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		_ = decoder.Decode(&cache.values)
	}
	value, ok := cache.values[name]
	if !ok || value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func (p *Plugin) copyConfiguredHeaders(dst, src http.Header, names []string) {
	for _, name := range names {
		dst.Del(name)
		for _, value := range src.Values(name) {
			dst.Add(name, value)
		}
	}
}
