package ip_restriction

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config Config

	filter *ipMatcher
	body   string
}

type ipMatcher struct {
	addresses      map[netip.Addr]struct{}
	prefixes       []netip.Prefix
	allowedOnMatch bool
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
	whitelist, err := newIPMatcher(p.config.Whitelist, true)
	if err != nil {
		return fmt.Errorf("invalid whitelist: %w", err)
	}
	blacklist, err := newIPMatcher(p.config.Blacklist, false)
	if err != nil {
		return fmt.Errorf("invalid blacklist: %w", err)
	}
	body, _ := json.Marshal(map[string]string{"message": p.config.Message})
	p.body = util.BytesToString(body)

	if len(p.config.Whitelist) > 0 {
		p.filter = whitelist
	}

	if len(p.config.Blacklist) > 0 {
		p.filter = blacklist
	}

	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		clientIP := ctx.GetString(r.Context(), "remote_addr")
		if clientIP == "" {
			clientIP, _, _ = net.SplitHostPort(r.RemoteAddr)
		}

		if p.filter != nil && !p.filter.Allowed(clientIP) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(p.config.ResponseCode)
			_, _ = w.Write([]byte(p.body + "\n"))
			return
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func newIPMatcher(definitions []string, allowedOnMatch bool) (*ipMatcher, error) {
	matcher := &ipMatcher{
		addresses:      make(map[netip.Addr]struct{}, len(definitions)),
		prefixes:       make([]netip.Prefix, 0, len(definitions)),
		allowedOnMatch: allowedOnMatch,
	}
	for _, definition := range definitions {
		if address, err := netip.ParseAddr(definition); err == nil && address.Zone() == "" {
			matcher.addresses[address.Unmap()] = struct{}{}
			continue
		}
		prefix, err := netip.ParsePrefix(definition)
		if err != nil || prefix.Addr().Zone() != "" {
			return nil, fmt.Errorf("%q is not an IP address or CIDR", definition)
		}
		matcher.prefixes = append(matcher.prefixes, prefix.Masked())
	}
	return matcher, nil
}

func (m *ipMatcher) Allowed(value string) bool {
	address, err := netip.ParseAddr(value)
	if err != nil || address.Zone() != "" {
		return false
	}
	if _, ok := m.addresses[address.Unmap()]; ok {
		return m.allowedOnMatch
	}
	for _, prefix := range m.prefixes {
		if prefixContains(prefix, address) {
			return m.allowedOnMatch
		}
	}
	return !m.allowedOnMatch
}

func prefixContains(prefix netip.Prefix, address netip.Addr) bool {
	if prefix.Contains(address) {
		return true
	}
	if address.Is4In6() {
		return prefix.Contains(address.Unmap())
	}
	if address.Is4() {
		return prefix.Contains(netip.AddrFrom16(address.As16()))
	}
	return false
}
