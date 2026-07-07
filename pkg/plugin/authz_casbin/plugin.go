package authz_casbin

import (
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
	enforcer *casbin.Enforcer
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
  "oneOf": [
    {
      "required": ["model_path", "policy_path", "username"]
    },
    {
      "required": ["model", "policy", "username"]
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

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	enforcer, err := p.newEnforcer()
	if err != nil {
		return err
	}
	p.enforcer = enforcer

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if p.enforcer == nil {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
			return
		}

		allowed, err := p.enforcer.Enforce(p.username(r), r.URL.Path, r.Method)
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

func (p *Plugin) username(r *http.Request) string {
	if username := r.Header.Get(p.config.Username); username != "" {
		return username
	}
	return "anonymous"
}
