package cors

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/rs/cors"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config

	cors *cors.Cors
}

const (
	// version  = "0.1"
	priority = 4000
	name     = "cors"
)

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

	// FIXME: not supported yet
	AllowOriginsByRegex       []string `json:"allow_origins_by_regex"`
	AllowOriginsByMetadata    []string `json:"allow_origins_by_metadata"`
	TimingAllowOrigins        *string  `json:"timing_allow_origins,omitempty"`
	TimingAllowOriginsByRegex []string `json:"timing_allow_origins_by_regex"`
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

	if p.config.ExposeHeaders == "" {
		p.config.ExposeHeaders = "*"
	}

	if p.config.MaxAge == 0 {
		p.config.MaxAge = 5
	}

	fmt.Printf("config: %+v\n", p.config)

	p.cors = cors.New(cors.Options{
		AllowedOrigins:   strings.Split(p.config.AllowOrigins, ","),
		AllowedMethods:   strings.Split(p.config.AllowMethods, ","),
		AllowedHeaders:   strings.Split(p.config.AllowHeaders, ","),
		ExposedHeaders:   strings.Split(p.config.ExposeHeaders, ","),
		MaxAge:           p.config.MaxAge,
		AllowCredentials: p.config.AllowCredential,
		// Enable Debugging for testing, consider disabling in production
		// Debug: true,
	})
	p.cors.Log = new(logger.DebugLogger)

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	// fn := func(w http.ResponseWriter, r *http.Request) {
	// 	fmt.Println("cors handler, do nothing")
	// 	next.ServeHTTP(w, r)
	// }
	// return http.HandlerFunc(fn)

	return p.cors.Handler(next)
}
