package uri_blocker

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	// version  = "0.1"
	priority = 2900
	name     = "uri-blocker"
)

const schema = `
{
	"$schema": "http://json-schema.org/draft-04/schema#",
	"type": "object",
	"properties": {
	  "block_rules": {
		"type": "array",
		"items": {
		  "type": "string",
		  "minLength": 1,
		  "maxLength": 4096
		},
		"uniqueItems": true
	  },
	  "rejected_code": {
		"type": "integer",
		"minimum": 200,
		"default": 403
	  },
	  "rejected_msg": {
		"type": "string",
		"minLength": 1
	  },
	  "case_insensitive": {
		"type": "boolean",
		"default": false
	  }
	},
	"required": ["block_rules"]
}`

type Config struct {
	BlockRules      []string `json:"block_rules"`
	RejectedCode    int      `json:"rejected_code,omitempty"`
	RejectedMsg     string   `json:"rejected_msg,omitempty"`
	CaseInsensitive *bool    `json:"case_insensitive,omitempty"`

	RegexRule *regexp.Regexp
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	// set the default
	if p.config.RejectedCode == 0 {
		p.config.RejectedCode = 403
	}

	if p.config.CaseInsensitive == nil {
		b := false
		p.config.CaseInsensitive = &b
	}

	// compile the regex rules
	blockRules := p.config.BlockRules
	blockRulesConcat := ""
	if len(blockRules) > 0 {
		// use '|' to concat all block rules
		if len(blockRules) == 1 {
			blockRulesConcat = blockRules[0]
		} else {
			blockRulesConcat = strings.Join(blockRules, "|")
		}

		blockRulesConcat = "(" + blockRulesConcat + ")"

		if *p.config.CaseInsensitive {
			blockRulesConcat = "(?i)" + blockRulesConcat
		}
	}

	fmt.Println("the block rules:", blockRulesConcat)

	p.config.RegexRule = regexp.MustCompile(blockRulesConcat)
	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("uri-blocker handler", r.RequestURI)
		fmt.Println("is match: ", p.config.RegexRule.MatchString(r.RequestURI))
		if p.config.RegexRule.MatchString(r.RequestURI) {
			if p.config.RejectedMsg != "" {
				http.Error(w, p.config.RejectedMsg, p.config.RejectedCode)
			} else {
				http.Error(w, http.StatusText(p.config.RejectedCode), p.config.RejectedCode)
			}
			return
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}
