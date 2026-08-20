package gzip

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/compression"
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
	Types          []string `json:"types"`
	MinLength      *int     `json:"min_length"`
	CompLevel      *int     `json:"comp_level"`
	HTTPVersion    *float64 `json:"http_version"`
	Buffers        *Buffers `json:"buffers"`
	Vary           *bool    `json:"vary,omitempty"`
	HTTPVersionStr string
	ConfigTypes    map[string]struct{}
	WildcardType   bool
}

func (c *Config) UnmarshalJSON(data []byte) error {
	type configJSON struct {
		Types       any      `json:"types,omitempty"`
		MinLength   *int     `json:"min_length,omitempty"`
		CompLevel   *int     `json:"comp_level,omitempty"`
		HTTPVersion *float64 `json:"http_version,omitempty"`
		Buffers     *Buffers `json:"buffers,omitempty"`
		Vary        *bool    `json:"vary,omitempty"`
	}

	var decoded configJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}

	switch types := decoded.Types.(type) {
	case nil:
	case string:
		c.Types = []string{types}
	case []any:
		c.Types = make([]string, 0, len(types))
		for _, value := range types {
			if stringValue, ok := value.(string); ok {
				c.Types = append(c.Types, stringValue)
			}
		}
	}
	c.MinLength = decoded.MinLength
	c.CompLevel = decoded.CompLevel
	c.HTTPVersion = decoded.HTTPVersion
	c.Buffers = decoded.Buffers
	c.Vary = decoded.Vary
	return nil
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
	if p.config.Vary == nil {
		defaultValue := false
		p.config.Vary = &defaultValue
	}

	contentTypes := defaultContentTypes
	if len(p.config.Types) > 0 {
		contentTypes = make(map[string]struct{}, len(p.config.Types))
		for _, t := range p.config.Types {
			if t == "*" {
				p.config.WildcardType = true
				continue
			}
			contentTypes[strings.ToLower(strings.TrimSpace(t))] = struct{}{}
		}
	}
	p.config.ConfigTypes = contentTypes

	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

// RegisterCompressionOffers exposes gzip through the shared, request-local
// negotiation state. The response metadata decides eligibility once, before
// the selected wrapper is constructed.
func (p *Plugin) RegisterCompressionOffers(r *http.Request, _ *compression.State) []compression.Offer {
	eligible := p.requestEligible(r)
	return []compression.Offer{{Coding: compression.Gzip, Rank: 995, Eligible: eligible}}
}

func (p *Plugin) WrapCompression(
	w http.ResponseWriter,
	r *http.Request,
	state *compression.State,
	decision compression.Decision,
) (http.ResponseWriter, error) {
	if decision.Coding != compression.Gzip {
		return w, nil
	}
	return &maybeCompressResponseWriter{
		ResponseWriter: w, w: w, contentTypes: p.config.ConfigTypes,
		wildcardType: p.config.WildcardType, level: *p.config.CompLevel,
		minLength: *p.config.MinLength, requestMethod: r.Method, state: state,
	}, nil
}

func (p *Plugin) RunStreamingHeaderFilter(_ *http.Request, _ *base.StreamingResponseState) error {
	return nil
}

func (p *Plugin) requestEligible(r *http.Request) func(compression.ResponseMeta) bool {
	return func(meta compression.ResponseMeta) bool {
		if r == nil || base.ProtocolVersion(r) < p.config.HTTPVersionStr {
			return false
		}
		return p.responseEligible(meta)
	}
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if base.ProtocolVersion(r) < p.config.HTTPVersionStr {
			next.ServeHTTP(w, r)
			return
		}
		eligible := func(meta compression.ResponseMeta) bool {
			return p.responseEligible(meta)
		}
		r, state := compression.Register(r,
			compression.Offer{Coding: compression.Gzip, Rank: 995, Eligible: eligible},
		)
		mcw := &maybeCompressResponseWriter{
			ResponseWriter: w,
			w:              w,
			contentTypes:   p.config.ConfigTypes,
			wildcardType:   p.config.WildcardType,
			level:          *p.config.CompLevel,
			minLength:      *p.config.MinLength,
			requestMethod:  r.Method,
			state:          state,
		}
		defer func() {
			if !mcw.hijacked && !mcw.wroteHeader {
				mcw.WriteHeader(http.StatusOK)
			}
			_ = mcw.Close()
		}()
		next.ServeHTTP(mcw, r)
	})
}

func (p *Plugin) responseEligible(meta compression.ResponseMeta) bool {
	status := meta.Status
	if status == http.StatusNotModified {
		return p.contentTypeEligible(meta.Header)
	}
	if status == http.StatusSwitchingProtocols || status == http.StatusNoContent ||
		(status >= 100 && status <= 199) {
		return false
	}
	if !base.ResponseAllowsBody(meta.Method, status) && !strings.EqualFold(meta.Method, http.MethodHead) {
		return false
	}
	if headerValue(meta.Header, "Content-Encoding") != "" {
		return false
	}
	if !p.contentTypeEligible(meta.Header) {
		return false
	}
	contentLength := headerValue(meta.Header, "Content-Length")
	if contentLength != "" {
		length, err := strconv.Atoi(strings.TrimSpace(contentLength))
		if err == nil && length < *p.config.MinLength {
			return false
		}
	}
	return true
}

func (p *Plugin) contentTypeEligible(header http.Header) bool {
	contentType := headerValue(header, "Content-Type")
	if semi := strings.IndexByte(contentType, ';'); semi >= 0 {
		contentType = contentType[:semi]
	}
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	if contentType == "" {
		return false
	}
	if p.config.WildcardType {
		return true
	}
	_, ok := p.config.ConfigTypes[contentType]
	return ok
}

func headerValue(header http.Header, name string) string {
	for actual, values := range header {
		if strings.EqualFold(actual, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}
