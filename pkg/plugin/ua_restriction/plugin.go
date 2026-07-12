package ua_restriction

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config

	allowList []*regexp.Regexp
	denyList  []*regexp.Regexp
	message   string
}

const (
	// version  = "0.1"
	priority = 2999
	name     = "ua-restriction"
)

const schema = `
{
	"type": "object",
	"properties": {
	  "bypass_missing": {
		"type": "boolean",
		"default": false
	  },
	  "allowlist": {
		"type": "array",
		"items": {
		  "type": "string",
		  "minLength": 1
		},
		"minItems": 1
	  },
	  "denylist": {
		"type": "array",
		"items": {
		  "type": "string",
		  "minLength": 1
		},
		"minItems": 1
	  },
	  "message": {
		"type": "string",
		"minLength": 1,
		"maxLength": 1024,
		"default": "Not allowed"
	  }
	},
	"oneOf": [
	  {"required": ["allowlist"]},
	  {"required": ["denylist"]}
	]
}`

type Config struct {
	BypassMissing *bool    `json:"bypass_missing"`
	AllowList     []string `json:"allowlist,omitempty"`
	DenyList      []string `json:"denylist,omitempty"`
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
		p.config.Message = "Not allowed"
	}

	if len(p.config.AllowList) > 0 {
		p.allowList = make([]*regexp.Regexp, 0, len(p.config.AllowList))
		for _, pattern := range p.config.AllowList {
			g, err := regexp.Compile(pattern)
			if err != nil {
				logger.Warnf("failed to compile allowList pattern: %s", pattern)
				continue
				// return err
			}
			p.allowList = append(p.allowList, g)
		}
	}
	if len(p.config.DenyList) > 0 {
		p.denyList = make([]*regexp.Regexp, 0, len(p.config.DenyList))
		for _, pattern := range p.config.DenyList {
			g, err := regexp.Compile(pattern)
			if err != nil {
				logger.Warnf("failed to compile denyList pattern: %s", pattern)
				continue
				// return err
			}
			p.denyList = append(p.denyList, g)
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
func (p *Plugin) inDenyList(host string) bool {
	for _, g := range p.denyList {
		if g.MatchString(host) {
			return true
		}
	}
	return false
}

func (p *Plugin) inAllowList(host string) bool {
	for _, g := range p.allowList {
		if g.MatchString(host) {
			return true
		}
	}
	return false
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		userAgents := r.Header.Values("User-Agent")
		if len(userAgents) == 0 {
			if !*p.config.BypassMissing {
				writeJSON(w, p.message)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		for _, ua := range userAgents {
			match := p.matchUserAgent(ua)
			if match == matchDeny {
				writeJSON(w, p.message)
				return
			}
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

const (
	matchNone = iota
	matchAllow
	matchDeny
)

func (p *Plugin) matchUserAgent(userAgent string) int {
	userAgent = strings.TrimSpace(userAgent)
	if len(p.config.AllowList) > 0 && p.inAllowList(userAgent) {
		return matchAllow
	}
	if len(p.config.DenyList) > 0 && p.inDenyList(userAgent) {
		return matchDeny
	}
	return matchNone
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(body))
}
