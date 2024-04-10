package gzip

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

const (
	// version  = "0.1"
	priority = 995
	name     = "gzip"
)

const schema = `
{
	"type": "object",
	"properties": {
	  "types": {
		"anyOf": [
		  {
			"type": "array",
			"minItems": 1,
			"items": {
			  "type": "string",
			  "minLength": 1
			}
		  },
		  {
			"enum": ["*"]
		  }
		],
		"default": ["text/html"]
	  },
	  "min_length": {
		"type": "integer",
		"minimum": 1,
		"default": 20
	  },
	  "comp_level": {
		"type": "integer",
		"minimum": 1,
		"maximum": 9,
		"default": 1
	  },
	  "http_version": {
		"enum": [1.1, 1.0],
		"default": 1.1
	  },
	  "buffers": {
		"type": "object",
		"properties": {
		  "number": {
			"type": "integer",
			"minimum": 1,
			"default": 32
		  },
		  "size": {
			"type": "integer",
			"minimum": 1,
			"default": 4096
		  }
		},
		"default": {
		  "number": 32,
		  "size": 4096
		}
	  },
	  "vary": {
		"type": "boolean"
	  }
	}
}`

type Buffers struct {
	Number int `json:"number"`
	Size   int `json:"size"`
}

type Config struct {
	Types          []string `json:"types"` // 使用 interface{} 以支持 string 数组和 "*" 枚举两种类型
	MinLength      *int     `json:"min_length"`
	CompLevel      *int     `json:"comp_level"`
	HTTPVersion    *float64 `json:"http_version"`
	Buffers        *Buffers `json:"buffers"`
	Vary           *bool    `json:"vary,omitempty"`
	HTTPVersionStr string
	ConfigTypes    map[string]struct{}
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.Types == nil {
		p.config.Types = []string{"text/html"}
	}

	if p.config.MinLength == nil {
		defaultValue := 20
		p.config.MinLength = &defaultValue
	}
	if p.config.CompLevel == nil {
		defaultValue := 1
		p.config.CompLevel = &defaultValue
	}

	if p.config.HTTPVersion == nil {
		defaultValue := 1.1
		p.config.HTTPVersion = &defaultValue
		p.config.HTTPVersionStr = "1.1"
	} else {
		// convert float64 to string
		p.config.HTTPVersionStr = fmt.Sprintf("%g", *p.config.HTTPVersion)
	}

	if p.config.Buffers == nil {
		p.config.Buffers = &Buffers{
			Number: 32,
			Size:   4096,
		}
	}

	contentTypes := defaultContentTypes
	if len(p.config.Types) > 0 {
		contentTypes = make(map[string]struct{}, len(p.config.Types))
		for _, t := range p.config.Types {
			contentTypes[t] = struct{}{}
		}
	}
	p.config.ConfigTypes = contentTypes

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		// get the request http version like 1.0 or 1.1 or 2
		reqHttpVersion := fmt.Sprintf("%d.%d", r.ProtoMajor, r.ProtoMinor)
		// only request header Content-Type with accept-encoding: gzip will be compressed
		acceptEncoding := r.Header.Get("Accept-Encoding")
		if (strings.Contains(acceptEncoding, "gzip") || strings.Contains(acceptEncoding, "deflate")) &&
			reqHttpVersion >= p.config.HTTPVersionStr {
			mcw := &maybeCompressResponseWriter{
				ResponseWriter: w,
				w:              w,
				contentTypes:   p.config.ConfigTypes,
				encoding:       selectEncoding(r.Header),
				level:          *p.config.CompLevel,
			}
			defer mcw.Close()

			if *p.config.Vary {
				mcw.Header().Add("Vary", "Accept-Encoding")
			}

			next.ServeHTTP(mcw, r)
		} else {
			next.ServeHTTP(w, r)
		}
	}
	return http.HandlerFunc(fn)
}
