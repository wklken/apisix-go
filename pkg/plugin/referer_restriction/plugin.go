package referer_restriction

import (
	"net/http"
	"strings"

	"github.com/Shopify/goreferrer"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config

	whitelist hostMatcher
	blacklist hostMatcher
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
		  "type": "string",
		  "pattern": "^\\*$|^\\*?[0-9a-zA-Z-._\\[\\]:]+$"
		},
		"minItems": 1
	  },
	  "blacklist": {
		"type": "array",
		"items": {
		  "type": "string",
		  "pattern": "^\\*$|^\\*?[0-9a-zA-Z-._\\[\\]:]+$"
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
		p.whitelist = newHostMatcher(p.config.Whitelist)
	}
	if len(p.config.Blacklist) > 0 {
		p.blacklist = newHostMatcher(p.config.Blacklist)
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
	return p.blacklist.match(host)
}

func (p *Plugin) inWhiteList(host string) bool {
	return p.whitelist.match(host)
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		// get the referer
		referer := goreferrer.DefaultRules.Parse(r.Header.Get("Referer"))

		host := referer.Host()
		if host == "" {
			if !*p.config.BypassMissing {
				writeJSON(w, p.message)
				return
			} else {
				// do nothing
				next.ServeHTTP(w, r)
			}
		} else {
			if len(p.config.Whitelist) > 0 {
				if !p.inWhiteList(host) {
					writeJSON(w, p.message)
					return
				}
			}

			if len(p.config.Blacklist) > 0 {
				if p.inBlackList(host) {
					writeJSON(w, p.message)
					return
				}
			}

			next.ServeHTTP(w, r)
		}
	}
	return http.HandlerFunc(fn)
}

type hostMatcher struct {
	exact    map[string]struct{}
	suffixes []string
}

func newHostMatcher(hosts []string) hostMatcher {
	matcher := hostMatcher{
		exact: make(map[string]struct{}, len(hosts)),
	}
	for _, host := range hosts {
		if strings.HasPrefix(host, "*") {
			matcher.suffixes = append(matcher.suffixes, strings.TrimPrefix(host, "*"))
			continue
		}
		matcher.exact[host] = struct{}{}
	}
	return matcher
}

func (m hostMatcher) match(host string) bool {
	if _, ok := m.exact[host]; ok {
		return true
	}
	for _, suffix := range m.suffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(body))
}
