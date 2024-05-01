package referer_restriction

import (
	"net/http"

	"github.com/Shopify/goreferrer"
	"github.com/gobwas/glob"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config

	whitelist []glob.Glob
	blacklist []glob.Glob
	message   string
}

const (
	// version  = "0.1"
	priority = 2990
	name     = "referer-restriction"
)

const schema = `
{
	"type": "object",
	"properties": {
	  "bypass_missing": {
		"type": "boolean",
		"default": false
	  },
	  "whitelist": {
		"type": "array",
		"items": {
		  "type": "string"
		},
		"minItems": 1
	  },
	  "blacklist": {
		"type": "array",
		"items": {
		  "type": "string"
		},
		"minItems": 1
	  },
	  "message": {
		"type": "string",
		"minLength": 1,
		"maxLength": 1024,
		"default": "Your referer host is not allowed"
	  }
	},
	"oneOf": [
	  {
		"required": ["whitelist"]
	  },
	  {
		"required": ["blacklist"]
	  }
	]
}`

type Config struct {
	BypassMissing *bool    `json:"bypass_missing"`
	Whitelist     []string `json:"whitelist,omitempty"`
	Blacklist     []string `json:"blacklist,omitempty"`
	Message       string   `json:"message"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.BypassMissing == nil {
		b := false
		p.config.BypassMissing = &b
	}

	if p.config.Message == "" {
		p.config.Message = "Your referer host is not allowed"
	}

	if len(p.config.Whitelist) > 0 {
		p.whitelist = make([]glob.Glob, 0, len(p.config.Whitelist))
		for _, pattern := range p.config.Whitelist {
			g, err := glob.Compile(pattern)
			if err != nil {
				logger.Warnf("failed to compile whitelist pattern: %s", pattern)
				continue
				// return err
			}
			p.whitelist = append(p.whitelist, g)
		}
	}
	if len(p.config.Blacklist) > 0 {
		p.blacklist = make([]glob.Glob, 0, len(p.config.Blacklist))
		for _, pattern := range p.config.Blacklist {
			g, err := glob.Compile(pattern)
			if err != nil {
				logger.Warnf("failed to compile blacklist pattern: %s", pattern)
				continue
				// return err
			}
			p.blacklist = append(p.blacklist, g)
		}
	}

	message, _ := json.Marshal(map[string]string{"message": p.config.Message})
	p.message = util.BytesToString(message)

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

// FIXME: add lrucache here? it's O(n)
func (p *Plugin) inBlackList(host string) bool {
	for _, g := range p.blacklist {
		if g.Match(host) {
			return true
		}
	}
	return false
}

func (p *Plugin) inWhiteList(host string) bool {
	for _, g := range p.whitelist {
		if g.Match(host) {
			return true
		}
	}
	return false
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		// get the referer
		referer := goreferrer.DefaultRules.Parse(r.Header.Get("Referer"))

		host := referer.Host()
		if host == "" {
			if !*p.config.BypassMissing {
				http.Error(w, p.message, http.StatusForbidden)
				return
			} else {
				// do nothing
				next.ServeHTTP(w, r)
			}
		} else {
			if len(p.config.Whitelist) > 0 {
				if !p.inWhiteList(host) {
					http.Error(w, p.message, http.StatusForbidden)
					return
				}
			}

			if len(p.config.Blacklist) > 0 {
				if p.inBlackList(host) {
					http.Error(w, p.message, http.StatusForbidden)
					return
				}
			}

			next.ServeHTTP(w, r)
		}
	}
	return http.HandlerFunc(fn)
}
