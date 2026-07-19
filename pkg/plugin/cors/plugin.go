package cors

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/rs/cors"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
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
	  "allow_private_network": {
		"description": "allow private-network preflight requests",
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

const metadataSchema = `
{
	"type": "object",
	"properties": {
	  "allow_origins": {
		"type": "object",
		"additionalProperties": {
		  "type": "string",
		  "pattern": "^(\\*|\\*\\*|null|\\w+://[^,]+(,\\w+://[^,]+)*)$"
		}
	  }
	}
}`

type Config struct {
	AllowOrigins        string `json:"allow_origins"`
	AllowMethods        string `json:"allow_methods"`
	AllowHeaders        string `json:"allow_headers"`
	ExposeHeaders       string `json:"expose_headers"`
	MaxAge              int    `json:"max_age"`
	AllowCredential     bool   `json:"allow_credential"`
	AllowPrivateNetwork bool   `json:"allow_private_network"`

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
	p.MetadataSchema = metadataSchema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.AllowCredential && wildcardCredentialOption(p.config) {
		return fmt.Errorf("you can not set '*' for other CORS options when allow_credential is true")
	}
	if p.config.AllowCredential && p.config.AllowOrigins == "" && p.config.AllowMethods == "" &&
		p.config.AllowHeaders == "" && p.config.ExposeHeaders == "" && p.config.MaxAge == 0 &&
		p.config.TimingAllowOrigins == nil {
		return fmt.Errorf("you can not set '*' for other CORS options when allow_credential is true")
	}

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
	if len(p.config.AllowOriginsByMetadata) > 0 && len(p.metadata.AllowOrigins) == 0 {
		p.metadata = base.LoadPluginMetadata[Metadata](name)
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

	options := cors.Options{
		AllowedOrigins:   strings.Split(p.config.AllowOrigins, ","),
		AllowedMethods:   allowedMethods(p.config.AllowMethods),
		AllowedHeaders:   allowedHeaders(p.config.AllowHeaders),
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

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	handler := p.cors.Handler(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		responseWriter := &varyResponseWriter{ResponseWriter: w}
		p.setAPISIXResponseHeaders(responseWriter.Header(), r)
		if origin, ok := p.timingAllowOrigin(r.Header.Get("Origin")); ok {
			responseWriter.Header().Set("Timing-Allow-Origin", origin)
		}
		if p.config.AllowPrivateNetwork && r.Method == http.MethodOptions &&
			strings.EqualFold(r.Header.Get("Access-Control-Request-Private-Network"), "true") {
			responseWriter.Header().Set("Access-Control-Allow-Private-Network", "true")
		}
		if r.Method == http.MethodOptions {
			responseWriter.WriteHeader(http.StatusOK)
			return
		}
		handler.ServeHTTP(responseWriter, r)
	})
}

type varyResponseWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *varyResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	normalizeVary(w.Header())
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *varyResponseWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *varyResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *varyResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *varyResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hijacker.Hijack()
}

func (w *varyResponseWriter) Push(target string, options *http.PushOptions) error {
	pusher, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, options)
}

func normalizeVary(header http.Header) {
	values := header.Values("Vary")
	if len(values) < 2 {
		return
	}

	var entries []string
	originPresent := false
	for _, value := range values {
		for entry := range strings.SplitSeq(value, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			if strings.EqualFold(entry, "Origin") {
				originPresent = true
				continue
			}
			entries = append(entries, entry)
		}
	}
	if originPresent {
		entries = append(entries, "Origin")
	}
	header.Set("Vary", strings.Join(entries, ", "))
}

func (p *Plugin) allowOrigin(origin string) bool {
	_, ok := p.responseOrigin(origin)
	return ok
}

func (p *Plugin) responseOrigin(origin string) (string, bool) {
	if len(p.config.AllowOriginsByMetadata) > 0 {
		if p.allowOriginFromMetadata(origin) {
			if origin != "" {
				return origin, true
			}
			return "*", true
		}
		if p.config.AllowOrigins == "" || p.config.AllowOrigins == "*" {
			return "", false
		}
	}
	if p.config.AllowOrigins == "**" {
		if origin == "" {
			return "*", true
		}
		return origin, true
	}
	if p.config.AllowOrigins == "*" && len(p.originRegex) == 0 {
		return "*", true
	}
	for allowedOrigin := range strings.SplitSeq(p.config.AllowOrigins, ",") {
		if allowedOrigin == "*" && len(p.originRegex) > 0 {
			continue
		}
		if allowedOrigin == origin && origin != "" {
			return origin, true
		}
	}
	for _, rule := range p.originRegex {
		if origin != "" && rule.MatchString(origin) {
			return origin, true
		}
	}
	return "", false
}

func (p *Plugin) setAPISIXResponseHeaders(header http.Header, request *http.Request) {
	origin, ok := p.responseOrigin(request.Header.Get("Origin"))
	if !ok {
		return
	}
	header.Set("Access-Control-Allow-Origin", origin)
	header.Set("Access-Control-Allow-Methods", responseMethods(p.config.AllowMethods))
	if p.config.AllowHeaders == "**" {
		if requestedHeaders := request.Header.Get("Access-Control-Request-Headers"); requestedHeaders != "" {
			header.Set("Access-Control-Allow-Headers", requestedHeaders)
		} else {
			header.Del("Access-Control-Allow-Headers")
		}
	} else {
		header.Set("Access-Control-Allow-Headers", p.config.AllowHeaders)
	}
	if p.config.ExposeHeaders == "" {
		header.Del("Access-Control-Expose-Headers")
	} else {
		header.Set("Access-Control-Expose-Headers", p.config.ExposeHeaders)
	}
	header.Set("Access-Control-Max-Age", strconv.Itoa(p.config.MaxAge))
	if p.config.AllowCredential {
		header.Set("Access-Control-Allow-Credentials", "true")
	} else {
		header.Del("Access-Control-Allow-Credentials")
	}
}

func responseMethods(methods string) string {
	if methods == "**" {
		return strings.Join(allMethods, ",")
	}
	return methods
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
	for allowedOrigin := range strings.SplitSeq(configured, ",") {
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
