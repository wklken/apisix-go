package oas_validator

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
)

type Plugin struct {
	base.BasePlugin
	config         Config
	metadata       Metadata
	mu             sync.Mutex
	compiled       atomic.Pointer[compiledSpec]
	compiledAt     atomic.Int64
	now            func() time.Time
	inlineSpec     *store.ResolvedSecret
	requestHeaders map[string]*store.ResolvedSecret

	refreshStart   sync.Once
	refreshStop    sync.Once
	refreshStopped atomic.Bool
	wakeRefresh    chan struct{}
	stopRefresh    chan struct{}
	refreshDone    chan struct{}
	refreshCtx     context.Context
	refreshCancel  context.CancelFunc
}

const (
	priority          = 512
	name              = "oas-validator"
	defaultSpecURLTTL = 3600
	maxOASTotalBytes  = 4 << 20
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
    "spec_url_allowed_addresses": {
      "type": "array",
      "items": {
        "type": "string",
        "minLength": 1
      },
      "uniqueItems": true
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
	SpecURLAllowedAddresses     []string          `json:"spec_url_allowed_addresses,omitempty"`
	SSLVerify                   bool              `json:"ssl_verify,omitempty"`
	Timeout                     int               `json:"timeout,omitempty"`
	VerboseErrors               bool              `json:"verbose_errors,omitempty"`
	SkipRequestBodyValidation   bool              `json:"skip_request_body_validation,omitempty"`
	SkipRequestHeaderValidation bool              `json:"skip_request_header_validation,omitempty"`
	SkipQueryParamValidation    bool              `json:"skip_query_param_validation,omitempty"`
	SkipPathParamsValidation    bool              `json:"skip_path_params_validation,omitempty"`
	RejectIfNotMatch            *bool             `json:"reject_if_not_match,omitempty"`
	RejectionStatusCode         int               `json:"rejection_status_code,omitempty"`
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
	if (p.config.Spec != "" && p.inlineSpec == nil) ||
		(len(p.config.SpecURLRequestHeaders) > 0 && p.requestHeaders == nil) {
		if err := p.MaterializeSecrets(); err != nil {
			return errors.New("oas-validator secret materialization failed")
		}
	}
	if p.config.Timeout == 0 {
		p.config.Timeout = 10000
	}
	if p.config.RejectionStatusCode == 0 {
		p.config.RejectionStatusCode = http.StatusBadRequest
	}
	p.metadata = base.LoadPluginMetadata[Metadata](name)
	if p.inlineSpec != nil {
		spec := p.inlineSpec.Bytes()
		defer clear(spec)
		if len(spec) > maxOASTotalBytes {
			return fmt.Errorf("inline openapi spec exceeds %d bytes", maxOASTotalBytes)
		}
		var raw any
		if err := json.Unmarshal(spec, &raw); err != nil {
			return fmt.Errorf("failed to parse inline openapi spec: %w", err)
		}
	}
	return nil
}

func (p *Plugin) MaterializeSecrets() error {
	if p.inlineSpec != nil || p.requestHeaders != nil {
		return nil
	}
	var inline *store.ResolvedSecret
	var err error
	if p.config.Spec != "" {
		inline, err = store.MaterializeSecret(p.config.Spec)
		if err != nil {
			return err
		}
	}
	headers := make(map[string]*store.ResolvedSecret, len(p.config.SpecURLRequestHeaders))
	for name, value := range p.config.SpecURLRequestHeaders {
		resolved, resolveErr := store.MaterializeSecret(value)
		if resolveErr != nil {
			inline.Destroy()
			for _, header := range headers {
				header.Destroy()
			}
			return resolveErr
		}
		headers[name] = resolved
	}
	p.inlineSpec = inline
	p.requestHeaders = headers
	if inline != nil {
		p.config.Spec = inline.Descriptor()
	}
	for name, header := range headers {
		p.config.SpecURLRequestHeaders[name] = header.Descriptor()
	}
	return nil
}

func (p *Plugin) resolvedRequestHeaders() map[string]string {
	headers := make(map[string]string, len(p.requestHeaders))
	for name, secret := range p.requestHeaders {
		value := secret.Bytes()
		headers[name] = string(value)
		clear(value)
	}
	return headers
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
	if compiled := p.compiled.Load(); compiled != nil {
		if p.config.Spec != "" || p.currentTime().Before(time.Unix(0, p.compiledAt.Load()).Add(p.specURLTTL())) {
			return compiled, nil
		}
		p.wakeSpecRefresh()
		return compiled, nil
	}

	// No validator exists yet, so the first request must wait for the
	// initial compile. A short lock keeps concurrent first requests on a
	// single compile while published validators are read atomically.
	p.mu.Lock()
	defer p.mu.Unlock()
	if compiled := p.compiled.Load(); compiled != nil {
		return compiled, nil
	}
	compiled, err := p.compileValidator(context.Background())
	if err != nil {
		return nil, err
	}
	p.publishValidator(compiled)
	return compiled, nil
}

// wakeSpecRefresh starts the plugin-owned refresher on first use and wakes it
// to re-fetch and recompile a due spec in the background. Requests never wait
// on the remote fetch; they keep validating with the last published validator.
func (p *Plugin) wakeSpecRefresh() {
	if p.refreshStopped.Load() {
		return
	}
	p.refreshStart.Do(func() {
		p.refreshCtx, p.refreshCancel = context.WithCancel(context.Background())
		p.wakeRefresh = make(chan struct{}, 1)
		p.stopRefresh = make(chan struct{})
		p.refreshDone = make(chan struct{})
		go p.specRefreshLoop()
	})
	select {
	case p.wakeRefresh <- struct{}{}:
	default:
	}
}

func (p *Plugin) specRefreshLoop() {
	defer close(p.refreshDone)
	for {
		select {
		case <-p.stopRefresh:
			return
		case <-p.wakeRefresh:
		}
		// A wake that arrives while another pass was refreshing must not
		// trigger a second fetch once the fresh validator is published.
		if p.config.Spec != "" ||
			p.currentTime().Before(time.Unix(0, p.compiledAt.Load()).Add(p.specURLTTL())) {
			continue
		}
		compiled, err := p.compileValidator(p.refreshCtx)
		if err != nil {
			logger.Errorf("failed to refresh openapi spec from URL: %s", err)
			continue
		}
		p.publishValidator(compiled)
	}
}

// compileValidator fetches and compiles the configured spec. It performs
// network I/O and must never run while the published-validator lock is held.
func (p *Plugin) compileValidator(ctx context.Context) (*compiledSpec, error) {
	var baseURL *url.URL
	if p.config.SpecURL != "" {
		var err error
		baseURL, err = url.Parse(p.config.SpecURL)
		if err != nil {
			return nil, err
		}
	}
	headers := p.resolvedRequestHeaders()
	client, err := newDocumentHTTPClient(
		baseURL,
		p.config.SpecURLAllowedAddresses,
		headers,
		p.config.SSLVerify,
		p.config.Timeout,
	)
	if err != nil {
		return nil, err
	}
	var spec []byte
	if p.inlineSpec != nil {
		spec = p.inlineSpec.Bytes()
		defer clear(spec)
	} else {
		fetched, err := fetchDocument(ctx, client, baseURL, headers, baseURL)
		if err != nil {
			return nil, err
		}
		spec = fetched
	}
	compiled, err := compileSpec(
		ctx,
		spec,
		baseURL,
		client,
		headers,
	)
	if err != nil {
		if p.config.SpecURL != "" {
			return nil, fmt.Errorf("failed to compile openapi spec fetched from URL: %w", err)
		}
		return nil, fmt.Errorf("failed to compile inline openapi spec: %w", err)
	}
	return compiled, nil
}

// publishValidator atomically swaps in a fully compiled validator.
func (p *Plugin) publishValidator(compiled *compiledSpec) {
	p.compiled.Store(compiled)
	p.compiledAt.Store(p.currentTime().UnixNano())
}

// Stop joins the spec refresher and cancels its remote fetches so no refresh
// worker outlives the plugin.
func (p *Plugin) Stop() {
	p.refreshStop.Do(func() {
		p.refreshStopped.Store(true)
		if p.refreshCancel != nil {
			p.refreshCancel()
		}
		if p.stopRefresh != nil {
			close(p.stopRefresh)
			<-p.refreshDone
		}
		p.inlineSpec.Destroy()
		for _, header := range p.requestHeaders {
			header.Destroy()
		}
	})
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

func (p *Plugin) rejectIfNotMatch() bool {
	return p.config.RejectIfNotMatch == nil || *p.config.RejectIfNotMatch
}
