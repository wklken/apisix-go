package real_ip

import (
	"context"
	"net"
	"net/http"
	"strings"

	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config

	trustedCIDRs []*net.IPNet
}

const (
	// version  = "0.1"
	priority = 23000
	name     = "real-ip"
)

// FIXME: json schema support cidr
//
//	  "anyOf": [
//		{
//		  "type": "string",
//		  "format": "ipv4"
//		},
//		{
//		  "type": "string",
//		  "format": "ipv6"
//		}
//	  ]
const schema = `
{
	"$schema": "http://json-schema.org/draft-04/schema#",
	"type": "object",
	"properties": {
	  "trusted_addresses": {
		"type": "array",
		"items": {
		  "type": "string"
		},
		"minItems": 1
	  },
	  "source": {
		"type": "string",
		"minLength": 1
	  },
	  "recursive": {
		"type": "boolean",
		"default": false
	  }
	},
	"required": ["source"]
}`

type Config struct {
	TrustedAddresses []string `json:"trusted_addresses"`
	Source           string   `json:"source,omitempty"`
	Recursive        *bool    `json:"recursive,omitempty"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.Recursive == nil {
		defaultValue := false
		p.config.Recursive = &defaultValue

	}
	if len(p.config.TrustedAddresses) > 0 {
		var err error
		p.trustedCIDRs, err = prepareTrustedCIDRs(p.config.TrustedAddresses)
		if err != nil {
			logger.Warn("prepareTrustedCIDRs fail")
		}
	}

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if len(p.config.TrustedAddresses) > 0 {
			ip, _, _ := parseAddr(r.RemoteAddr)
			if !p.isTrustedProxy(net.ParseIP(ip)) {
				next.ServeHTTP(w, r)
				return
			}
		}

		ctx := r.Context()
		clientIP := p.sourceValue(r)
		if clientIP != "" {
			ip, port, ok := parseAddr(clientIP)
			if !ok {
				logger.Warnf("bad real address: %s", clientIP)
				next.ServeHTTP(w, r)
				return
			}

			ctx = context.WithValue(ctx, "remote_addr", ip)
			ctx = context.WithValue(ctx, "remote_port", port)
		}

		next.ServeHTTP(w, r.WithContext(ctx))
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) sourceValue(r *http.Request) string {
	source := p.config.Source
	if strings.HasPrefix(source, "arg_") {
		return r.URL.Query().Get(source[4:])
	}

	if strings.HasPrefix(source, "http_") {
		header := httpHeaderName(source[5:])
		values := r.Header.Values(header)
		if len(values) == 0 {
			return ""
		}
		value := values[len(values)-1]
		if strings.EqualFold(source, "http_x_forwarded_for") {
			return p.forwardedFor(value)
		}
		return value
	}

	return ""
}

func (p *Plugin) forwardedFor(value string) string {
	if value == "" {
		return ""
	}
	items := strings.Split(value, ",")
	if len(items) == 0 {
		return ""
	}

	if p.config.Recursive != nil && *p.config.Recursive && len(p.trustedCIDRs) > 0 {
		for i := len(items) - 1; i >= 1; i-- {
			item := strings.TrimSpace(items[i])
			ip, _, ok := parseAddr(item)
			if !ok || !p.isTrustedProxy(net.ParseIP(ip)) {
				return item
			}
		}
		return strings.TrimSpace(items[0])
	}

	return strings.TrimSpace(items[len(items)-1])
}

func httpHeaderName(name string) string {
	parts := strings.Split(name, "_")
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, "-")
}

func parseAddr(addr string) (string, string, bool) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", "", false
	}

	host, port, err := net.SplitHostPort(addr)
	if err == nil {
		host = strings.Trim(host, "[]")
		if net.ParseIP(host) == nil {
			return "", "", false
		}
		return host, port, true
	}

	host = strings.Trim(addr, "[]")
	if net.ParseIP(host) == nil {
		return "", "", false
	}
	return host, "", true
}
