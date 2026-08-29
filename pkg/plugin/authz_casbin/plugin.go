package authz_casbin

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/model"
	stringadapter "github.com/casbin/casbin/v2/persist/string-adapter"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config   Config
	enforcer *casbin.SyncedEnforcer
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
      "required": ["model_path", "policy_path"]
    },
    {
      "required": ["model", "policy"]
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

const metadataSchema = `
{
  "type": "object",
  "properties": {
    "model": {
      "type": "string",
      "minLength": 1
    },
    "policy": {
      "type": "string",
      "minLength": 1
    }
  },
  "required": ["model", "policy"]
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
	p.MetadataSchema = metadataSchema

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

	var metadata Metadata
	found, err := p.MetadataView().Decode(name, &metadata)
	if err != nil {
		return err
	}
	if !found || metadata.Model == "" || metadata.Policy == "" {
		return fmt.Errorf("not enough configuration to create enforcer")
	}
	enforcer, err := newInlineEnforcer(metadata.Model, metadata.Policy)
	if err != nil {
		return err
	}
	p.enforcer = enforcer
	return nil
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
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprintln(w, util.BuildMessageResponse("Access Denied"))
			return
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) newEnforcer() (*casbin.SyncedEnforcer, error) {
	if p.config.ModelPath != "" && p.config.PolicyPath != "" {
		return casbin.NewSyncedEnforcer(p.config.ModelPath, p.config.PolicyPath)
	}

	if p.config.Model != "" && p.config.Policy != "" {
		return newInlineEnforcer(p.config.Model, p.config.Policy)
	}

	return nil, fmt.Errorf("not enough configuration to create enforcer")
}

func newInlineEnforcer(modelText, policyText string) (*casbin.SyncedEnforcer, error) {
	m, err := model.NewModelFromString(modelText)
	if err != nil {
		return nil, err
	}
	return casbin.NewSyncedEnforcer(m, stringadapter.NewAdapter(policyText))
}

func (p *Plugin) hasRouteConfig() bool {
	return (p.config.ModelPath != "" && p.config.PolicyPath != "") ||
		(p.config.Model != "" && p.config.Policy != "")
}

func (p *Plugin) currentEnforcer() (*casbin.SyncedEnforcer, error) {
	if p.enforcer == nil {
		return nil, errors.New("casbin enforcer is not initialized")
	}
	return p.enforcer, nil
}

func (p *Plugin) username(r *http.Request) string {
	if username := r.Header.Get(p.config.Username); username != "" {
		return username
	}
	return "anonymous"
}
