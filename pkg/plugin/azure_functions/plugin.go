package azure_functions

import (
	"net/http"

	"github.com/wklken/apisix-go/pkg/plugin/function_upstream"
	"github.com/wklken/apisix-go/pkg/store"
)

type Plugin struct {
	function_upstream.Plugin
	config   Config
	metadata Metadata
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
	p.Processor = p.processRequest

	return nil
}

func (p *Plugin) PostInit() error {
	p.loadMetadata()
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

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) processRequest(r *http.Request, _ function_upstream.Config) {
	if r.Header.Get("X-Functions-Key") != "" || r.Header.Get("X-Functions-Clientid") != "" {
		return
	}
	if p.config.Authorization == nil {
		if p.metadata.MasterAPIKey != "" {
			r.Header.Set("X-Functions-Key", p.metadata.MasterAPIKey)
		}
		if p.metadata.MasterClientID != "" {
			r.Header.Set("X-Functions-Clientid", p.metadata.MasterClientID)
		}
		return
	}

	if p.config.Authorization.APIKey != "" {
		r.Header.Set("X-Functions-Key", p.config.Authorization.APIKey)
	}
	if p.config.Authorization.ClientID != "" {
		r.Header.Set("X-Functions-Clientid", p.config.Authorization.ClientID)
	}
}

func (p *Plugin) loadMetadata() {
	var metadata Metadata
	if err := safeGetPluginMetadata(name, &metadata); err == nil {
		p.metadata = metadata
	}
}

func safeGetPluginMetadata(id string, target any) (err error) {
	defer func() {
		if recover() != nil {
			err = store.ErrNotFound
		}
	}()
	return store.GetPluginMetadata(id, target)
}
