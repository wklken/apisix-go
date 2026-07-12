package cors

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/rs/cors"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
)

type Plugin struct {
	base.BasePlugin
	config   Config
	metadata Metadata

	cors              *cors.Cors
	originRegex       []*regexp.Regexp
	timingOriginRegex []*regexp.Regexp
}

const (
	// version  = "0.1"
	priority = 4000
	name     = "cors"
)

var allMethods = []string{
	http.MethodGet,
	http.MethodPost,
	http.MethodPut,
	http.MethodDelete,
	http.MethodPatch,
	http.MethodHead,
	http.MethodOptions,
	http.MethodConnect,
	http.MethodTrace,
}

const schema = `
{
	"type": "object",
	"properties": {
	  "allow_origins": {
		"description": "you can use '*' to allow all origins when no credentials, '**' to allow forcefully(it will bring some security risks, be carefully), multiple origin use ',' to split. default: *.",
		"type": "string",
		"pattern": "^(\\*|\\*\\*|null|\\w+://[^,]+(,\\w+://[^,]+)*)$",
		"default": "*"
	  },
	  "allow_methods": {
		"description": "you can use '*' to allow all methods when no credentials, '**' to allow forcefully(it will bring some security risks, be carefully), multiple method use ',' to split. default: *.",
		"type": "string",
		"default": "*"
	  },
	  "allow_headers": {
		"description": "you can use '*' to allow all header when no credentials, '**' to allow forcefully(it will bring some security risks, be carefully), multiple header use ',' to split. default: *.",
		"type": "string",
		"default": "*"
	  },
	  "expose_headers": {
		"description": "you can use '*' to expose all header when no credentials, '**' to allow forcefully(it will bring some security risks, be carefully), multiple header use ',' to split. default: *.",
		"type": "string",
		"default": "*"
	  },
	  "max_age": {
		"description": "maximum number of seconds the results can be cached. -1 means no cached, the max value is depend on browser, more details plz check MDN. default: 5.",
		"type": "integer",
		"default": 5
	  },
	  "allow_credential": {
		"description": "allow client append credential. according to CORS specification, if you set this option to 'true', you can not use '*' for other options.",
		"type": "boolean",
		"default": false
	  },
	  "allow_origins_by_regex": {
		"description": "you can use regex to allow specific origins when no credentials, for example use [.*\\.test.com$] to allow a.test.com and b.test.com",
		"type": "array",
		"items": {
		  "type": "string",
		  "minLength": 1,
		  "maxLength": 4096
		},
		"minItems": 1,
		"uniqueItems": true
	  },
	  "allow_origins_by_metadata": {
		"description": "set allowed origins by referencing origins in plugin metadata",
		"type": "array",
		"items": {
		  "type": "string",
		  "minLength": 1,
		  "maxLength": 4096
		},
		"minItems": 1,
		"uniqueItems": true
	  },
	  "timing_allow_origins": {
		"description": "you can use '*' to allow all origins which can view timing information when no credentials, '**' to allow forcefully (it will bring some security risks, be careful), multiple origin use ',' to split. default: nil",
		"type": "string",
		"pattern": "^(\\*|\\*\\*|null|\\w+://[^,]+(,\\w+://[^,]+)*)$"
	  },
	  "timing_allow_origins_by_regex": {
		"description": "you can use regex to allow specific origins which can view timing information, for example use [.*\\.test.com] to allow a.test.com and b.test.com",
		"type": "array",
		"items": {
		  "type": "string",
		  "minLength": 1,
		  "maxLength": 4096
		},
		"minItems": 1,
		"uniqueItems": true
	  }
	}
}`

type Config struct {
	AllowOrigins    string `json:"allow_origins"`
	AllowMethods    string `json:"allow_methods"`
	AllowHeaders    string `json:"allow_headers"`
	ExposeHeaders   string `json:"expose_headers"`
	MaxAge          int    `json:"max_age"`
	AllowCredential bool   `json:"allow_credential"`

	AllowOriginsByRegex []string `json:"allow_origins_by_regex"`
	// FIXME: not supported yet
	AllowOriginsByMetadata    []string `json:"allow_origins_by_metadata"`
	TimingAllowOrigins        *string  `json:"timing_allow_origins,omitempty"`
	TimingAllowOriginsByRegex []string `json:"timing_allow_origins_by_regex"`
}

type Metadata struct {
	AllowOrigins map[string]string `json:"allow_origins"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.AllowOrigins == "" {
		p.config.AllowOrigins = "*"
	}

	if p.config.AllowMethods == "" {
		p.config.AllowMethods = "*"
	}

	if p.config.AllowHeaders == "" {
		p.config.AllowHeaders = "*"
	}

	if p.config.MaxAge == 0 {
		p.config.MaxAge = 5
	}
	if p.config.AllowCredential && wildcardCredentialOption(p.config) {
		return fmt.Errorf("you can not set '*' for other CORS options when allow_credential is true")
	}
	if len(p.config.AllowOriginsByMetadata) > 0 && len(p.metadata.AllowOrigins) == 0 {
		p.metadata = loadMetadata()
	}

	for _, rule := range p.config.AllowOriginsByRegex {
		compiled, err := regexp.Compile(rule)
		if err != nil {
			return fmt.Errorf("compile allow_origins_by_regex %q: %w", rule, err)
		}
		p.originRegex = append(p.originRegex, compiled)
	}
	for _, rule := range p.config.TimingAllowOriginsByRegex {
		compiled, err := regexp.Compile(rule)
		if err != nil {
			return fmt.Errorf("compile timing_allow_origins_by_regex %q: %w", rule, err)
		}
		p.timingOriginRegex = append(p.timingOriginRegex, compiled)
	}

	var exposedHeaders []string
	if p.config.ExposeHeaders != "" {
		exposedHeaders = strings.Split(p.config.ExposeHeaders, ",")
	}
	options := cors.Options{
		AllowedOrigins:   strings.Split(p.config.AllowOrigins, ","),
		AllowedMethods:   allowedMethods(p.config.AllowMethods),
		AllowedHeaders:   allowedHeaders(p.config.AllowHeaders),
		ExposedHeaders:   exposedHeaders,
		MaxAge:           p.config.MaxAge,
		AllowCredentials: p.config.AllowCredential,
		// APISIX exits successful preflight OPTIONS requests with 200.
		OptionsSuccessStatus: http.StatusOK,
		// Enable Debugging for testing, consider disabling in production
		// Debug: true,
	}
	if p.config.AllowOrigins == "**" || len(p.originRegex) > 0 || len(p.config.AllowOriginsByMetadata) > 0 {
		options.AllowOriginFunc = p.allowOrigin
	}
	p.cors = cors.New(options)
	p.cors.Log = new(logger.DebugLogger)

	return nil
}

func wildcardCredentialOption(config Config) bool {
	if config.AllowOrigins == "*" || config.AllowMethods == "*" || config.AllowHeaders == "*" ||
		config.ExposeHeaders == "*" {
		return true
	}
	return config.TimingAllowOrigins != nil && *config.TimingAllowOrigins == "*"
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	handler := p.cors.Handler(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin, ok := p.timingAllowOrigin(r.Header.Get("Origin")); ok {
			w.Header().Set("Timing-Allow-Origin", origin)
		}
		handler.ServeHTTP(w, r)
	})
}

func (p *Plugin) allowOrigin(origin string) bool {
	if len(p.config.AllowOriginsByMetadata) > 0 {
		if p.allowOriginFromMetadata(origin) {
			return true
		}
		if p.config.AllowOrigins == "" || p.config.AllowOrigins == "*" {
			return false
		}
	}
	for _, allowedOrigin := range strings.Split(p.config.AllowOrigins, ",") {
		if allowedOrigin == "*" || allowedOrigin == origin {
			return true
		}
		if allowedOrigin == "**" && origin != "" {
			return true
		}
	}
	for _, rule := range p.originRegex {
		if rule.MatchString(origin) {
			return true
		}
	}
	return false
}

func (p *Plugin) allowOriginFromMetadata(origin string) bool {
	for _, key := range p.config.AllowOriginsByMetadata {
		configured, ok := p.metadata.AllowOrigins[key]
		if !ok {
			continue
		}
		if _, ok := matchConfiguredOrigin(origin, configured); ok {
			return true
		}
	}
	return false
}

func (p *Plugin) timingAllowOrigin(origin string) (string, bool) {
	if len(p.timingOriginRegex) > 0 {
		if origin == "" {
			return "", false
		}
		for _, rule := range p.timingOriginRegex {
			if rule.MatchString(origin) {
				return origin, true
			}
		}
		return "", false
	}
	if p.config.TimingAllowOrigins == nil {
		return "", false
	}
	return matchConfiguredOrigin(origin, *p.config.TimingAllowOrigins)
}

func matchConfiguredOrigin(origin string, configured string) (string, bool) {
	for _, allowedOrigin := range strings.Split(configured, ",") {
		switch allowedOrigin {
		case "*":
			return "*", true
		case "**":
			if origin == "" {
				return "*", true
			}
			return origin, true
		case origin:
			if origin != "" {
				return origin, true
			}
		}
	}
	return "", false
}

func loadMetadata() (metadata Metadata) {
	defer func() {
		if recover() != nil {
			metadata = Metadata{}
		}
	}()
	if err := store.GetPluginMetadata(name, &metadata); err != nil {
		return Metadata{}
	}
	return metadata
}

func allowedMethods(methods string) []string {
	if methods == "*" || methods == "**" {
		return allMethods
	}
	return strings.Split(methods, ",")
}

func allowedHeaders(headers string) []string {
	if headers == "**" {
		return []string{"*"}
	}
	return strings.Split(headers, ",")
}
