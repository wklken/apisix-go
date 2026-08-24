package azure_functions

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"sync"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/function_upstream"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/store"
)

type Plugin struct {
	function_upstream.Plugin
	config   Config
	metadata Metadata

	routeSecretsMu    sync.RWMutex
	routeAPIKey       secret.Value
	routeAPIKeySet    bool
	legacyRouteAPIKey *store.ResolvedSecret
}

const (
	priority = -1900
	name     = "azure-functions"
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
        "apikey": {
          "type": "string"
        },
        "clientid": {
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

const metadataSchema = `
{
  "type": "object",
  "properties": {
    "master_apikey": {"type": "string"},
    "master_clientid": {"type": "string"}
  }
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
	APIKey   string `json:"apikey,omitempty"`
	ClientID string `json:"clientid,omitempty"`
}

type Metadata struct {
	MasterAPIKey   string `json:"master_apikey,omitempty"`
	MasterClientID string `json:"master_clientid,omitempty"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	p.MetadataSchema = metadataSchema
	p.Processor = p.processRequest

	return nil
}

func (p *Plugin) PostInit() error {
	if err := p.loadMetadata(); err != nil {
		return err
	}
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

// MaterializeScopedSecrets resolves the route API key for one immutable
// generation attempt. The public config is replaced with a descriptor only
// after both resolution and descriptor construction succeed.
func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context,
	access base.ScopedSecretAccess,
) error {
	p.routeSecretsMu.Lock()
	defer p.routeSecretsMu.Unlock()
	if p.config.Authorization == nil || p.config.Authorization.APIKey == "" {
		return nil
	}
	installed := p.routeAPIKeySet || p.legacyRouteAPIKey != nil
	raw := p.config.Authorization.APIKey
	if installed {
		return nil
	}

	value, err := access.Materialize(ctx, "authorization.apikey", raw)
	if err != nil {
		return err
	}
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return err
	}

	if p.routeAPIKeySet || p.legacyRouteAPIKey != nil {
		return nil
	}
	p.routeAPIKey = value
	p.routeAPIKeySet = true
	p.config.Authorization.APIKey = descriptor.String()
	return nil
}

// MaterializeSecrets is the transitional process-local compatibility path.
// New generation preparation uses MaterializeScopedSecrets instead.
func (p *Plugin) MaterializeSecrets() error {
	p.routeSecretsMu.Lock()
	defer p.routeSecretsMu.Unlock()
	if p.config.Authorization == nil || p.config.Authorization.APIKey == "" {
		return nil
	}
	installed := p.routeAPIKeySet || p.legacyRouteAPIKey != nil
	raw := p.config.Authorization.APIKey
	if installed {
		return nil
	}

	resolved, err := store.MaterializeSecret(raw)
	if err != nil {
		return err
	}
	bytes := resolved.Bytes()
	digest := sha256.Sum256(bytes)
	clear(bytes)
	descriptor, err := secret.NewDescriptor(capability.SecretPluginConfig, digest)
	if err != nil {
		resolved.Destroy()
		return err
	}

	if p.routeAPIKeySet || p.legacyRouteAPIKey != nil {
		resolved.Destroy()
		return nil
	}
	p.legacyRouteAPIKey = resolved
	p.config.Authorization.APIKey = descriptor.String()
	return nil
}

func (p *Plugin) processRequest(r *http.Request, _ function_upstream.Config) {
	if _, ok := r.Header["X-Functions-Key"]; ok {
		return
	}
	if _, ok := r.Header["X-Functions-Clientid"]; ok {
		return
	}

	p.routeSecretsMu.RLock()
	defer p.routeSecretsMu.RUnlock()
	if p.config.Authorization == nil {
		if p.metadata.MasterAPIKey != "" {
			r.Header.Set("X-Functions-Key", p.metadata.MasterAPIKey)
		}
		if p.metadata.MasterClientID != "" {
			r.Header.Set("X-Functions-Clientid", p.metadata.MasterClientID)
		}
		return
	}

	if p.routeAPIKeySet {
		_ = p.routeAPIKey.Use(func(value string) error {
			if value != "" {
				r.Header.Set("X-Functions-Key", value)
			}
			return nil
		})
	} else if p.legacyRouteAPIKey != nil {
		value := p.legacyRouteAPIKey.Bytes()
		if len(value) != 0 {
			r.Header.Set("X-Functions-Key", string(value))
		}
		clear(value)
	}
	if p.config.Authorization.ClientID != "" {
		r.Header.Set("X-Functions-Clientid", p.config.Authorization.ClientID)
	}
}

func (p *Plugin) Stop() {
	p.Plugin.Stop()

	p.routeSecretsMu.Lock()
	defer p.routeSecretsMu.Unlock()
	if p.legacyRouteAPIKey != nil {
		p.legacyRouteAPIKey.Destroy()
		p.legacyRouteAPIKey = nil
	}
	p.routeAPIKey = secret.Value{}
	p.routeAPIKeySet = false
}

func (p *Plugin) loadMetadata() error {
	var metadata Metadata
	if _, err := p.MetadataView().Decode(name, &metadata); err != nil {
		return fmt.Errorf("%s metadata decode failed: %w", name, err)
	}
	p.metadata = metadata
	return nil
}
