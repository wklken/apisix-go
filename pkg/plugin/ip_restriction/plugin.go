package ip_restriction

import (
	"fmt"
	"net"
	"net/http"

	"github.com/jpillora/ipfilter"
	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config

	filter *ipfilter.IPFilter
	body   string
}

const (
	// version  = "0.1"
	priority = 3000
	name     = "ip-restriction"
)

// FIXME: ipv4/ipv6 and cidr
//   "anyOf": [
// 	{
// 	  "type": "string",
// 	  "format": "ipv4"
// 	},
// 	{
// 	  "type": "string",
// 	  "format": "ipv6"
// 	}
//   ]

const schema = `
{
	"type": "object",
	"properties": {
	  "message": {
		"type": "string",
		"minLength": 1,
		"maxLength": 1024,
		"default": "Your IP address is not allowed"
	  },
	  "response_code": {
		"type": "integer",
		"minimum": 403,
		"maximum": 404,
		"default": 403
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
	Message      string   `json:"message"`
	ResponseCode int      `json:"response_code,omitempty"`
	Whitelist    []string `json:"whitelist,omitempty"`
	Blacklist    []string `json:"blacklist,omitempty"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.Message == "" {
		p.config.Message = "Your IP address is not allowed"
	}
	if p.config.ResponseCode == 0 {
		p.config.ResponseCode = http.StatusForbidden
	}
	if err := validateIPDefinitions(p.config.Whitelist); err != nil {
		return fmt.Errorf("invalid whitelist: %w", err)
	}
	if err := validateIPDefinitions(p.config.Blacklist); err != nil {
		return fmt.Errorf("invalid blacklist: %w", err)
	}
	body, _ := json.Marshal(map[string]string{"message": p.config.Message})
	p.body = util.BytesToString(body)

	if len(p.config.Whitelist) > 0 {
		p.filter = ipfilter.New(ipfilter.Options{
			AllowedIPs:     p.config.Whitelist,
			BlockByDefault: true,
		})
	}

	if len(p.config.Blacklist) > 0 {
		p.filter = ipfilter.New(ipfilter.Options{
			BlockedIPs:     p.config.Blacklist,
			BlockByDefault: false,
		})
	}

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		clientIP := ctx.GetString(r.Context(), "remote_addr")
		if clientIP == "" {
			clientIP, _, _ = net.SplitHostPort(r.RemoteAddr)
		}

		if p.filter != nil && !p.filter.Allowed(clientIP) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(p.config.ResponseCode)
			_, _ = w.Write([]byte(p.body))
			return
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func validateIPDefinitions(definitions []string) error {
	for _, definition := range definitions {
		if net.ParseIP(definition) != nil {
			continue
		}
		if _, _, err := net.ParseCIDR(definition); err != nil {
			return fmt.Errorf("%q is not an IP address or CIDR", definition)
		}
	}
	return nil
}
