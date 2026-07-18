package authz_casbin

import (
	"fmt"
	"net/http"
	"sync"

	"github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/model"
	stringadapter "github.com/casbin/casbin/v2/persist/string-adapter"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config   Config
	enforcer *casbin.Enforcer
	mu       sync.Mutex
	metadata Metadata
}

const (
	priority = 2560
	name     = "authz-casbin"
)

const schema = `
{
  "type": "object",
  "properties": {
    "model_path": {
      "type": "string"
    },
    "policy_path": {
      "type": "string"
    },
    "model": {
      "type": "string"
    },
    "policy": {
      "type": "string"
    },
    "username": {
      "type": "string"
    }
  },
  "required": ["username"],
  "oneOf": [
    {
      "required": ["model_path", "policy_path"],
      "not": {
        "anyOf": [
          {"required": ["model"]},
          {"required": ["policy"]}
        ]
      }
    },
    {
      "required": ["model", "policy"],
      "not": {
        "anyOf": [
          {"required": ["model_path"]},
          {"required": ["policy_path"]}
        ]
      }
    },
    {
      "not": {
        "anyOf": [
          {"required": ["model_path"]},
          {"required": ["policy_path"]},
          {"required": ["model"]},
          {"required": ["policy"]}
        ]
      }
    }
  ]
}
`

type Config struct {
	ModelPath  string `json:"model_path,omitempty"`
	PolicyPath string `json:"policy_path,omitempty"`
	Model      string `json:"model,omitempty"`
	Policy     string `json:"policy,omitempty"`
	Username   string `json:"username"`
}

type Metadata struct {
	Model  string `json:"model"`
	Policy string `json:"policy"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.hasRouteConfig() {
		enforcer, err := p.newEnforcer()
		if err != nil {
			return err
		}
		p.enforcer = enforcer
		return nil
	}
	_, err := p.metadataEnforcer()
	return err
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		enforcer, err := p.currentEnforcer()
		if err != nil {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}

		allowed, err := enforcer.Enforce(p.username(r), r.URL.Path, r.Method)
		if err != nil {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}
		if !allowed {
			http.Error(w, util.BuildMessageResponse("Access Denied"), http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) newEnforcer() (*casbin.Enforcer, error) {
	if p.config.ModelPath != "" && p.config.PolicyPath != "" {
		return casbin.NewEnforcer(p.config.ModelPath, p.config.PolicyPath)
	}

	if p.config.Model != "" && p.config.Policy != "" {
		m, err := model.NewModelFromString(p.config.Model)
		if err != nil {
			return nil, err
		}
		return casbin.NewEnforcer(m, stringadapter.NewAdapter(p.config.Policy))
	}

	return nil, fmt.Errorf("not enough configuration to create enforcer")
}

func (p *Plugin) hasRouteConfig() bool {
	return (p.config.ModelPath != "" && p.config.PolicyPath != "") ||
		(p.config.Model != "" && p.config.Policy != "")
}

func (p *Plugin) currentEnforcer() (*casbin.Enforcer, error) {
	if p.hasRouteConfig() {
		if p.enforcer == nil {
			return nil, fmt.Errorf("casbin enforcer is not initialized")
		}
		return p.enforcer, nil
	}
	return p.metadataEnforcer()
}

func (p *Plugin) metadataEnforcer() (*casbin.Enforcer, error) {
	var metadata Metadata
	if err := store.GetPluginMetadata(name, &metadata); err != nil {
		return nil, err
	}
	if metadata.Model == "" || metadata.Policy == "" {
		return nil, fmt.Errorf("not enough configuration to create enforcer")
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.enforcer != nil && p.metadata == metadata {
		return p.enforcer, nil
	}

	m, err := model.NewModelFromString(metadata.Model)
	if err != nil {
		return nil, err
	}
	enforcer, err := casbin.NewEnforcer(m, stringadapter.NewAdapter(metadata.Policy))
	if err != nil {
		return nil, err
	}
	p.enforcer = enforcer
	p.metadata = metadata

	return enforcer, nil
}

func (p *Plugin) username(r *http.Request) string {
	if username := r.Header.Get(p.config.Username); username != "" {
		return username
	}
	return "anonymous"
}
