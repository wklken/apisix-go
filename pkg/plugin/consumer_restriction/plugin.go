package consumer_restriction

import (
	"fmt"
	"net/http"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	// version  = "0.1"
	priority = 2400
	name     = "consumer-restriction"
)

// FIXME: methods schema

const schema = `
{
	"type": "object",
	"properties": {
	  "type": {
		"type": "string",
		"enum": ["consumer_name", "service_id", "route_id", "consumer_group_id"],
		"default": "consumer_name"
	  },
	  "blacklist": {
		"type": "array",
		"minItems": 1,
		"items": {
		  "type": "string"
		}
	  },
	  "whitelist": {
		"type": "array",
		"minItems": 1,
		"items": {
		  "type": "string"
		}
	  },
	  "allowed_by_methods": {
		"type": "array",
		"items": {
		  "type": "object",
		  "properties": {
			"user": {
			  "type": "string"
			},
			"methods": {
			  "type": "array",
			  "minItems": 1,
			  "items": {
				"type": "string"
			  }
			}
		  },
		  "required": ["user", "methods"]
		}
	  },
	  "rejected_code": {
		"type": "integer",
		"minimum": 200,
		"default": 403
	  },
	  "rejected_msg": {
		"type": "string"
	  }
	},
	"anyOf": [
	  {
		"required": ["blacklist"]
	  },
	  {
		"required": ["whitelist"]
	  },
	  {
		"required": ["allowed_by_methods"]
	  }
	]
}`

type Config struct {
	Type string `json:"type"`

	Blacklist        *[]string              `json:"blacklist,omitempty"`
	Whitelist        *[]string              `json:"whitelist,omitempty"`
	AllowedByMethods []AllowedByMethodsItem `json:"allowed_by_methods,omitempty"`

	RejectedCode int    `json:"rejected_code"`
	RejectedMsg  string `json:"rejected_msg,omitempty"`

	key                    string
	emptyValueErrorMessage string
	blacklistOn            bool
	blacklistMap           map[string]struct{}
	whitelistOn            bool
	whitelistMap           map[string]struct{}
	rejectBody             string
}

type AllowedByMethodsItem struct {
	User    string   `json:"user"`
	Methods []string `json:"methods"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.Type == "" {
		p.config.Type = "consumer_name"
	}
	if p.config.RejectedCode == 0 {
		p.config.RejectedCode = 403
	}

	switch p.config.Type {
	case "consumer_name":
		p.config.key = "$consumer_name"
	case "consumer_group_id":
		p.config.key = "$consumer_group_id"
	case "service_id":
		p.config.key = "$service_id"
	case "route_id":
		p.config.key = "$route_id"
	}

	p.config.emptyValueErrorMessage = util.BuildMessageResponse(
		fmt.Sprintf("The request is rejected, please check the %s for this request", p.config.Type),
	)

	if p.config.Blacklist != nil {
		p.config.blacklistMap = make(map[string]struct{})
		for _, item := range *p.config.Blacklist {
			p.config.blacklistMap[item] = struct{}{}
		}
		p.config.blacklistOn = len(p.config.blacklistMap) > 0
	}

	if p.config.Whitelist != nil {
		p.config.whitelistMap = make(map[string]struct{})
		for _, item := range *p.config.Whitelist {
			p.config.whitelistMap[item] = struct{}{}
		}
		p.config.whitelistOn = len(p.config.whitelistMap) > 0
	}

	rejectMsg := p.config.RejectedMsg
	if rejectMsg == "" {
		rejectMsg = fmt.Sprintf("The %s is forbidden", p.config.Type)
	}
	p.config.rejectBody = util.BuildMessageResponse(rejectMsg)

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		value := ctx.GetApisixVar(r, p.config.key).(string)
		if value == "" {
			http.Error(w, p.config.emptyValueErrorMessage, http.StatusUnauthorized)
			return
		}

		// check blacklist
		if p.config.blacklistOn {
			if _, ok := p.config.blacklistMap[value]; ok {
				http.Error(w, p.config.rejectBody, p.config.RejectedCode)
				return
			}
		}

		block := false
		whitelisted := false

		if p.config.whitelistOn {
			_, whitelisted = p.config.whitelistMap[value]
			if !whitelisted {
				block = true
			}
		}

		method := r.Method
		if p.config.AllowedByMethods != nil && !whitelisted {
			if !isMethodAllowed(p.config.AllowedByMethods, method, value) {
				block = true
			}
		}

		if block {
			http.Error(w, p.config.rejectBody, p.config.RejectedCode)
			return
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func isMethodAllowed(methods []AllowedByMethodsItem, method string, user string) bool {
	for _, m := range methods {
		if m.User == user {
			// FIXME: use map for better performance
			for _, v := range m.Methods {
				if v == method {
					return true
				}
			}
			return false
		}
	}

	return true
}
