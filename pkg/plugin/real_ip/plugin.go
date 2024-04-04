package real_ip

import (
	"context"
	"fmt"
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
		fmt.Println("1")
		if len(p.config.TrustedAddresses) > 0 {
			remoteAddr := r.RemoteAddr

			ip, _, err := net.SplitHostPort(remoteAddr)
			if err != nil {
				// do nothing
			}
			if !p.isTrustedProxy(net.ParseIP(ip)) {
				fmt.Println("1-a")
				next.ServeHTTP(w, r)
				return
			}
		}

		fmt.Println("2")
		ctx := r.Context()
		// "X-Forwarded-For", "X-Real-IP"
		var clientIP string
		if p.config.Source == "http_x_forwarded_for" {
			headerClientIP, valid := p.validateHeader("X-Forwarded-For")
			if valid {
				clientIP = headerClientIP
			}
			fmt.Println("31")
		} else {
			// arg_realip
			if strings.HasPrefix(p.config.Source, "arg_") {
				clientIP = r.URL.Query().Get(p.config.Source[4:])
			}
			fmt.Println("32")
		}
		if clientIP != "" {
			fmt.Println("clientIP", clientIP)
			// parse to ip and port
			ip, port, err := net.SplitHostPort(clientIP)
			if err != nil {
				logger.Warnf("Failed to parse client IP: %s", err)
			}
			fmt.Println("ip", ip)
			fmt.Println("port", port)

			// TODO
			// local ok, err = client.set_real_ip(ip, port)
			ctx = context.WithValue(ctx, "remote_addr", ip)
			ctx = context.WithValue(ctx, "remote_port", port)

			fmt.Println("4")
		} else {
			fmt.Println("missing real address")
		}

		// next.ServeHTTP(w, r)
		next.ServeHTTP(w, r.WithContext(ctx))
	}
	return http.HandlerFunc(fn)
}
