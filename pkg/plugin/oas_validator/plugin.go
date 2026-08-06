package oas_validator

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config     Config
	metadata   Metadata
	mu         sync.Mutex
	compiledAt time.Time
	now        func() time.Time
}

const (
	priority          = 512
	name              = "oas-validator"
	defaultSpecURLTTL = 3600
)

const schema = `
{
  "type": "object",
  "properties": {
    "spec": {
      "type": "string",
      "minLength": 1
    },
    "spec_url": {
      "type": "string",
      "pattern": "^https?://"
    },
    "spec_url_request_headers": {
      "type": "object",
      "additionalProperties": {
        "type": "string"
      }
    },
    "ssl_verify": {
      "type": "boolean",
      "default": false
    },
    "timeout": {
      "type": "integer",
      "minimum": 1000,
      "maximum": 60000,
      "default": 10000
    },
    "verbose_errors": {
      "type": "boolean",
      "default": false
    },
    "skip_request_body_validation": {
      "type": "boolean",
      "default": false
    },
    "skip_request_header_validation": {
      "type": "boolean",
      "default": false
    },
    "skip_query_param_validation": {
      "type": "boolean",
      "default": false
    },
    "skip_path_params_validation": {
      "type": "boolean",
      "default": false
    },
    "reject_if_not_match": {
      "type": "boolean",
      "default": true
    },
    "rejection_status_code": {
      "type": "integer",
      "minimum": 400,
      "maximum": 599,
      "default": 400
    }
  },
  "oneOf": [
    {
      "required": ["spec"]
    },
    {
      "required": ["spec_url"]
    }
  ]
}
`

const metadataSchema = `
{
  "type": "object",
  "properties": {
    "spec_url_ttl": {
      "type": "integer",
      "minimum": 1
    }
  }
}
`

type Config struct {
	Spec                        string            `json:"spec,omitempty"`
	SpecURL                     string            `json:"spec_url,omitempty"`
	SpecURLRequestHeaders       map[string]string `json:"spec_url_request_headers,omitempty"`
	SSLVerify                   bool              `json:"ssl_verify,omitempty"`
	Timeout                     int               `json:"timeout,omitempty"`
	VerboseErrors               bool              `json:"verbose_errors,omitempty"`
	SkipRequestBodyValidation   bool              `json:"skip_request_body_validation,omitempty"`
	SkipRequestHeaderValidation bool              `json:"skip_request_header_validation,omitempty"`
	SkipQueryParamValidation    bool              `json:"skip_query_param_validation,omitempty"`
	SkipPathParamsValidation    bool              `json:"skip_path_params_validation,omitempty"`
	RejectIfNotMatch            *bool             `json:"reject_if_not_match,omitempty"`
	RejectionStatusCode         int               `json:"rejection_status_code,omitempty"`

	compiled *compiledSpec
}

type Metadata struct {
	SpecURLTTL int `json:"spec_url_ttl,omitempty"`
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	p.MetadataSchema = metadataSchema
	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.Timeout == 0 {
		p.config.Timeout = 10000
	}
	if p.config.RejectionStatusCode == 0 {
		p.config.RejectionStatusCode = http.StatusBadRequest
	}
	p.metadata = base.LoadPluginMetadata[Metadata](name)
	if p.config.Spec != "" {
		var raw any
		if err := json.Unmarshal([]byte(p.config.Spec), &raw); err != nil {
			return fmt.Errorf("failed to parse inline openapi spec: %w", err)
		}
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		validator, err := p.validator()
		if err != nil {
			logger.Error(err.Error())
			base.WriteJSONMessage(w, http.StatusInternalServerError, "failed to parse openapi spec")
			return
		}

		if err := validateRequest(r.Context(), r, validator, p.config); err != nil {
			logger.Errorf("error occurred while validating request: %s", err)
			if p.rejectIfNotMatch() {
				msg := "failed to validate request. "
				if p.config.VerboseErrors {
					msg += err.Error()
				}
				base.WriteJSONMessage(w, p.config.RejectionStatusCode, msg)
				return
			}
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) validator() (*compiledSpec, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.config.compiled != nil {
		if p.config.Spec != "" || p.currentTime().Before(p.compiledAt.Add(p.specURLTTL())) {
			return p.config.compiled, nil
		}
	}

	spec := p.config.Spec
	if spec == "" {
		fetched, err := p.fetchSpec()
		if err != nil {
			return nil, err
		}
		spec = fetched
	}

	var (
		baseURL *url.URL
		err     error
	)
	if p.config.SpecURL != "" {
		baseURL, err = url.Parse(p.config.SpecURL)
		if err != nil {
			return nil, err
		}
	}
	compiled, err := compileSpec(
		context.Background(),
		[]byte(spec),
		baseURL,
		p.httpClient(),
		p.config.SpecURLRequestHeaders,
	)
	if err != nil {
		if p.config.SpecURL != "" {
			return nil, fmt.Errorf("failed to compile openapi spec fetched from URL: %w", err)
		}
		return nil, fmt.Errorf("failed to compile inline openapi spec: %w", err)
	}
	p.config.compiled = compiled
	p.compiledAt = p.currentTime()
	return compiled, nil
}

func (p *Plugin) currentTime() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *Plugin) specURLTTL() time.Duration {
	if p.metadata.SpecURLTTL > 0 {
		return time.Duration(p.metadata.SpecURLTTL) * time.Second
	}
	return defaultSpecURLTTL * time.Second
}

func (p *Plugin) fetchSpec() (string, error) {
	req, err := http.NewRequest(http.MethodGet, p.config.SpecURL, nil)
	if err != nil {
		return "", err
	}
	for name, value := range p.config.SpecURLRequestHeaders {
		req.Header.Set(name, value)
	}

	res, err := p.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("spec URL returned status %d", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func (p *Plugin) httpClient() *http.Client {
	client := &http.Client{Timeout: time.Duration(p.config.Timeout) * time.Millisecond}
	if !p.config.SSLVerify {
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	return client
}

func (p *Plugin) rejectIfNotMatch() bool {
	return p.config.RejectIfNotMatch == nil || *p.config.RejectIfNotMatch
}
