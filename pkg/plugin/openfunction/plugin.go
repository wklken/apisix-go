package openfunction

import (
	"context"
	"encoding/base64"
	"net/http"
	"sync"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/function_upstream"
	"github.com/wklken/apisix-go/pkg/secret"
)

type Plugin struct {
	function_upstream.Plugin
	config Config

	serviceTokenMu  sync.RWMutex
	serviceToken    secret.Value
	serviceTokenSet bool

	// testLifecycleHook is a package-local synchronization seam for lifecycle
	// tests; it is nil in production.
	testLifecycleHook func(string)
}

const (
	priority = -1902
	name     = "openfunction"

	lifecycleBeforeAuthorizationUse = "before-authorization-use"
	lifecycleAfterUpstreamStop      = "after-upstream-stop"
)

const schema = `
{
  "type": "object",
  "properties": {
    "function_uri": {
      "type": "string"
    },
    "authorization": {
      "type": "object",
      "properties": {
        "service_token": {
          "type": "string"
        }
      }
    },
    "timeout": {
      "type": "integer",
      "minimum": 100,
      "default": 3000
    },
    "ssl_verify": {
      "type": "boolean",
      "default": true
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
  "required": ["function_uri"]
}
`

type Config struct {
	FunctionURI      string         `json:"function_uri"`
	Authorization    *Authorization `json:"authorization,omitempty"`
	Timeout          int            `json:"timeout,omitempty"`
	SSLVerify        *bool          `json:"ssl_verify,omitempty"`
	Keepalive        *bool          `json:"keepalive,omitempty"`
	KeepaliveTimeout int            `json:"keepalive_timeout,omitempty"`
	KeepalivePool    int            `json:"keepalive_pool,omitempty"`
}

type Authorization struct {
	ServiceToken string `json:"service_token,omitempty"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	p.Processor = p.processRequest

	return nil
}

func (p *Plugin) PostInit() error {
	p.Plugin.Config = function_upstream.Config{
		FunctionURI:      p.config.FunctionURI,
		Timeout:          p.config.Timeout,
		SSLVerify:        p.config.SSLVerify,
		Keepalive:        p.config.Keepalive,
		KeepaliveTimeout: p.config.KeepaliveTimeout,
		KeepalivePool:    p.config.KeepalivePool,
	}
	return p.Plugin.PostInit()
}

func (p *Plugin) Config() any {
	return &p.config
}

// MaterializeScopedSecrets resolves the optional route token for one immutable
// generation attempt. The public configuration retains only a redacted
// descriptor; the resolved value remains private to this plugin instance.
func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context,
	access base.ScopedSecretAccess,
) error {
	p.serviceTokenMu.Lock()
	defer p.serviceTokenMu.Unlock()

	if p.config.Authorization == nil || p.config.Authorization.ServiceToken == "" {
		return nil
	}
	if p.serviceTokenSet {
		return nil
	}

	value, err := access.Materialize(
		ctx,
		"authorization.service_token",
		p.config.Authorization.ServiceToken,
	)
	if err != nil {
		return errOpenFunctionCredentialUnavailable
	}
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return errOpenFunctionCredentialUnavailable
	}

	p.serviceToken = value
	p.serviceTokenSet = true
	p.config.Authorization.ServiceToken = descriptor.String()
	return nil
}

func (p *Plugin) processRequest(r *http.Request, _ function_upstream.Config) {
	p.serviceTokenMu.RLock()
	defer p.serviceTokenMu.RUnlock()

	if p.serviceTokenSet {
		if hook := p.testLifecycleHook; hook != nil {
			hook(lifecycleBeforeAuthorizationUse)
		}
		_ = p.serviceToken.Use(func(value string) error {
			if value != "" {
				r.Header.Set(
					"Authorization",
					"Basic "+base64.StdEncoding.EncodeToString([]byte(value)),
				)
			}
			return nil
		})
		return
	}
}

func (p *Plugin) Stop() {
	// Release the shared upstream client before releasing credentials that may
	// still be needed by in-flight request processors.
	p.Plugin.Stop()
	if hook := p.testLifecycleHook; hook != nil {
		hook(lifecycleAfterUpstreamStop)
	}

	p.serviceTokenMu.Lock()
	defer p.serviceTokenMu.Unlock()
	p.serviceToken = secret.Value{}
	p.serviceTokenSet = false
}

var errOpenFunctionCredentialUnavailable = secret.ErrCredentialUnavailable
