package proxy_rewrite

import (
	"context"
	"fmt"
	"net/http"
	"regexp"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	// version  = "0.1"
	priority = 1008
	name     = "proxy-rewrite"
)

const schema = `
{
	"$schema": "http://json-schema.org/draft-04/schema#",
	"type": "object",
	"properties": {
	  "uri": {
		"type": "string"
	  },
	  "regex_uri": {
		"type": "array",
		"minItems": 2,
		"items": {
			"type": "string"
		}
	  },
	  "method": {
		"type": "string"
	  },
	  "host": {
		"type": "string"
	  },
	  "scheme": {
		"type": "string"
	  },
	  "headers": {
		"type": "object",
		"properties": {
			"add": {
				"type": "object"
			},
			"set": {
				"type": "object"
			},
			"remove": {
				"type": "array"
			}
		}
	  }
	}
  }
`

type Headers struct {
	Add    map[string]string `json:"add"`
	Set    map[string]string `json:"set"`
	Remove []string          `json:"remove"`
}

type Config struct {
	Uri      string   `json:"uri"`
	RegexURI []string `json:"regex_uri"`
	Method   string   `json:"method"`
	Host     string   `json:"host"`
	Scheme   string   `json:"scheme"`
	Headers  Headers  `json:"headers"`

	regexURIPairs []regexURIPair
}

type regexURIPair struct {
	pattern     *regexp.Regexp
	replacement string
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if len(p.config.RegexURI)%2 != 0 {
		return fmt.Errorf("regex_uri length should be even")
	}
	p.config.regexURIPairs = p.config.regexURIPairs[:0]
	for i := 0; i < len(p.config.RegexURI); i += 2 {
		pattern, err := regexp.Compile(p.config.RegexURI[i])
		if err != nil {
			return fmt.Errorf("invalid regex_uri pattern %q: %w", p.config.RegexURI[i], err)
		}
		p.config.regexURIPairs = append(p.config.regexURIPairs, regexURIPair{
			pattern:     pattern,
			replacement: p.config.RegexURI[i+1],
		})
	}
	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		uri := p.rewriteURI(r.URL.Path)

		data := map[string]interface{}{
			"uri":     uri,
			"method":  p.config.Method,
			"host":    p.config.Host,
			"scheme":  p.config.Scheme,
			"headers": p.config.Headers,
		}

		ctx = context.WithValue(ctx, "proxy-rewrite", data)

		next.ServeHTTP(w, r.WithContext(ctx))
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) rewriteURI(path string) string {
	if p.config.Uri != "" {
		return p.config.Uri
	}
	for _, pair := range p.config.regexURIPairs {
		if pair.pattern.MatchString(path) {
			return pair.pattern.ReplaceAllString(path, pair.replacement)
		}
	}
	return ""
}
