package oas_validator

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
)

type Plugin struct {
	base.BasePlugin
	oasSecretState
	config         Config
	metadata       Metadata
	mu             sync.Mutex
	compiled       atomic.Pointer[compiledSpec]
	compiledAt     atomic.Int64
	now            func() time.Time
	refreshStart   sync.Once
	refreshStop    sync.Once
	refreshMu      sync.Mutex
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
	release, err := p.acquireOASWork()
	if err != nil {
		return err
	}
	defer release()
	if err := p.requirePreparedOASSecrets(); err != nil {
		return err
	}
	var metadata Metadata
	if _, err := p.MetadataView().Decode(name, &metadata); err != nil {
		return fmt.Errorf("oas-validator metadata decode failed: %w", err)
	}
	p.metadata = metadata
	if p.config.Timeout == 0 {
		p.config.Timeout = 10000
	}
	if p.config.RejectionStatusCode == 0 {
		p.config.RejectionStatusCode = http.StatusBadRequest
	}
	if p.config.Spec != "" {
		return p.withInlineSpec(func(plaintext string) error {
			if len(plaintext) > maxOASTotalBytes {
				return fmt.Errorf("inline openapi spec exceeds %d bytes", maxOASTotalBytes)
			}
			spec := []byte(plaintext)
			defer clear(spec)
			var raw any
			if err := json.Unmarshal(spec, &raw); err != nil {
				return fmt.Errorf("failed to parse inline openapi spec: %w", err)
			}
			return nil
		})
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		release, err := p.acquireOASWork()
		if err != nil {
			logger.Error(err.Error())
			base.WriteJSONMessage(w, http.StatusInternalServerError, "failed to parse openapi spec")
			return
		}
		proceed := func() bool {
			defer release()
			return p.validateOASRequest(w, r)
		}()
		if proceed {
			next.ServeHTTP(w, r)
		}
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) validateOASRequest(w http.ResponseWriter, r *http.Request) bool {
	validator, err := p.validator()
	if err != nil {
		logger.Error(err.Error())
		base.WriteJSONMessage(w, http.StatusInternalServerError, "failed to parse openapi spec")
		return false
	}
	if err := validateRequest(r.Context(), r, validator, p.config); err != nil {
		logger.Errorf("error occurred while validating request: %s", err)
		if p.rejectIfNotMatch() {
			msg := "failed to validate request. "
			if p.config.VerboseErrors {
				msg += err.Error()
			}
			base.WriteJSONMessage(w, p.config.RejectionStatusCode, msg)
			return false
		}
	}
	return true
}

func (p *Plugin) validator() (*compiledSpec, error) {
	release, err := p.acquireOASWork()
	if err != nil {
		return nil, err
	}
	defer release()
	if compiled := p.compiled.Load(); compiled != nil {
		if p.retired.Load() {
			return nil, secret.ErrCredentialUnavailable
		}
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
		if p.retired.Load() {
			return nil, secret.ErrCredentialUnavailable
		}
		return compiled, nil
	}
	compiled, err := p.compileValidator(context.Background())
	if err != nil {
		return nil, err
	}
	if !p.publishOASValidator(compiled) {
		return nil, secret.ErrCredentialUnavailable
	}
	return compiled, nil
}

// wakeSpecRefresh starts the plugin-owned refresher on first use and wakes it
// to re-fetch and recompile a due spec in the background. Requests never wait
// on the remote fetch; they keep validating with the last published validator.
func (p *Plugin) wakeSpecRefresh() {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
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
		p.publishOASValidator(compiled)
	}
}

// compileValidator fetches and compiles the configured spec. It performs
// network I/O and must never run while the published-validator lock is held.
func (p *Plugin) compileValidator(ctx context.Context) (*compiledSpec, error) {
	release, err := p.acquireOASWork()
	if err != nil {
		return nil, err
	}
	defer release()
	var baseURL *url.URL
	if p.config.SpecURL != "" {
		var err error
		baseURL, err = url.Parse(p.config.SpecURL)
		if err != nil {
			return nil, err
		}
	}
	var compiled *compiledSpec
	err = p.withRequestHeaders(func(headers map[string]string) error {
		client, err := newDocumentHTTPClient(
			baseURL,
			p.config.SpecURLAllowedAddresses,
			headers,
			p.config.SSLVerify,
			p.config.Timeout,
		)
		if err != nil {
			return err
		}
		compile := func(spec []byte) error {
			compiled, err = compileSpec(ctx, spec, baseURL, client, headers)
			if err == nil {
				return nil
			}
			if p.config.SpecURL != "" {
				return fmt.Errorf("failed to compile openapi spec fetched from URL: %w", err)
			}
			return fmt.Errorf("failed to compile inline openapi spec: %w", err)
		}
		if p.config.Spec != "" {
			return p.withInlineSpec(func(plaintext string) error {
				spec := []byte(plaintext)
				defer clear(spec)
				return compile(spec)
			})
		}
		spec, err := fetchDocument(ctx, client, baseURL, headers, baseURL)
		if err != nil {
			return err
		}
		defer clear(spec)
		return compile(spec)
	})
	return compiled, err
}

// Stop joins the spec refresher and cancels its remote fetches so no refresh
// worker outlives the plugin.
func (p *Plugin) Stop() {
	p.refreshStop.Do(func() {
		p.refreshStopped.Store(true)
		workWait := p.retireOASWork()
		wait := p.retireOASSecrets()
		p.refreshMu.Lock()
		cancel := p.refreshCancel
		stopRefresh := p.stopRefresh
		refreshDone := p.refreshDone
		if cancel != nil {
			cancel()
		}
		if stopRefresh != nil {
			close(stopRefresh)
		}
		p.refreshMu.Unlock()
		if refreshDone != nil {
			<-refreshDone
		}
		if wait != nil {
			<-wait
		}
		if workWait != nil {
			<-workWait
		}
		p.dropOASSecrets()
		p.compiled.Store(nil)
		p.compiledAt.Store(0)
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
