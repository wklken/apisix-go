package proxy_rewrite

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"golang.org/x/net/http/httpguts"
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
	  },
	  "use_real_request_uri_unsafe": {
		"type": "boolean",
		"default": false
	  }
	}
  }
`

type Headers struct {
	Add       HeaderValues `json:"add"`
	Set       HeaderValues `json:"set"`
	Remove    []string     `json:"remove"`
	LegacySet HeaderValues `json:"-"`
}

type Config struct {
	Uri                     string   `json:"uri"`
	RegexURI                []string `json:"regex_uri"`
	Method                  string   `json:"method"`
	Host                    string   `json:"host"`
	Scheme                  string   `json:"scheme"`
	Headers                 Headers  `json:"headers"`
	UseRealRequestURIUnsafe bool     `json:"use_real_request_uri_unsafe"`

	regexURIPairs []regexURIPair
}

type regexURIPair struct {
	pattern     *regexp.Regexp
	replacement string
}

type HeaderValues map[string]string

func (h *HeaderValues) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	values := make(HeaderValues, len(raw))
	for key, value := range raw {
		switch v := value.(type) {
		case string:
			values[key] = v
		case float64:
			values[key] = strconv.FormatFloat(v, 'f', -1, 64)
		default:
			return fmt.Errorf("invalid header value type for %q", key)
		}
	}
	*h = values
	return nil
}

func (h *Headers) UnmarshalJSON(data []byte) error {
	type headerOperations Headers
	var operations headerOperations
	if err := json.Unmarshal(data, &operations); err != nil {
		return err
	}
	if len(operations.Add) > 0 || len(operations.Set) > 0 || len(operations.Remove) > 0 {
		*h = Headers(operations)
		return nil
	}

	var legacy HeaderValues
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	h.LegacySet = legacy
	return nil
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.Uri != "" && !strings.HasPrefix(p.config.Uri, "/") {
		return fmt.Errorf("uri %q must begin with /", p.config.Uri)
	}
	if len(p.config.RegexURI)%2 != 0 {
		return fmt.Errorf("regex_uri length should be even")
	}
	p.config.regexURIPairs = p.config.regexURIPairs[:0]
	for i := 0; i < len(p.config.RegexURI); i += 2 {
		pattern, err := regexp.Compile(p.config.RegexURI[i])
		if err != nil {
			return fmt.Errorf("invalid regex_uri pattern %q: %w", p.config.RegexURI[i], err)
		}
		if err := validateRegexReplacement(p.config.RegexURI[i+1]); err != nil {
			return fmt.Errorf("invalid regex_uri replacement %q: %w", p.config.RegexURI[i+1], err)
		}
		p.config.regexURIPairs = append(p.config.regexURIPairs, regexURIPair{
			pattern:     pattern,
			replacement: p.config.RegexURI[i+1],
		})
	}
	if err := p.config.Headers.validate(); err != nil {
		return err
	}
	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		uri, captures := p.rewriteURI(p.rewriteSourceURI(r))
		if p.config.Uri != "" {
			uri = appendRequestQuery(resolveHeaderValue(r, p.config.Uri, nil), r.URL.RawQuery)
		}
		p.config.Headers.apply(r, captures)

		data := map[string]any{
			"uri":     uri,
			"method":  p.config.Method,
			"host":    p.config.Host,
			"scheme":  p.config.Scheme,
			"headers": p.config.Headers,
		}

		ctx = context.WithValue(ctx, apisixctx.ProxyRewriteKey, data)

		next.ServeHTTP(w, r.WithContext(ctx))
	}
	return http.HandlerFunc(fn)
}

func (h Headers) apply(r *http.Request, captures []string) {
	for name, value := range h.LegacySet {
		resolved := resolveHeaderValue(r, value, captures)
		if resolved == "" {
			r.Header.Del(name)
			continue
		}
		r.Header.Set(name, resolved)
	}
	for name, value := range h.Add {
		r.Header.Add(name, resolveHeaderValue(r, value, captures))
	}
	for name, value := range h.Set {
		r.Header.Set(name, resolveHeaderValue(r, value, captures))
	}
	for _, name := range h.Remove {
		r.Header.Del(name)
	}
}

func (p *Plugin) rewriteSourceURI(r *http.Request) string {
	if p.config.UseRealRequestURIUnsafe {
		return r.URL.RequestURI()
	}
	return r.URL.Path
}

func (p *Plugin) rewriteURI(path string) (string, []string) {
	if p.config.Uri != "" {
		return p.config.Uri, nil
	}
	for _, pair := range p.config.regexURIPairs {
		if matches := pair.pattern.FindStringSubmatch(path); matches != nil {
			rewritten := pair.pattern.ReplaceAllStringFunc(path, func(match string) string {
				return resolveCaptureValue(pair.replacement, pair.pattern.FindStringSubmatch(match))
			})
			return rewritten, matches
		}
	}
	if p.config.UseRealRequestURIUnsafe {
		return path, nil
	}
	return "", nil
}

func appendRequestQuery(uri string, rawQuery string) string {
	if rawQuery == "" {
		return uri
	}
	if strings.Contains(uri, "?") {
		return uri + "&" + rawQuery
	}
	return uri + "?" + rawQuery
}

func validateRegexReplacement(replacement string) error {
	for position := 0; position < len(replacement); position++ {
		if replacement[position] != '$' {
			continue
		}
		position++
		if position >= len(replacement) {
			return fmt.Errorf("capture number is missing")
		}
		if replacement[position] == '{' {
			position++
			start := position
			for position < len(replacement) && replacement[position] >= '0' && replacement[position] <= '9' {
				position++
			}
			if position == start || position >= len(replacement) || replacement[position] != '}' {
				return fmt.Errorf("invalid braced capture")
			}
			continue
		}
		if replacement[position] < '0' || replacement[position] > '9' {
			return fmt.Errorf("invalid capture name")
		}
		for position+1 < len(replacement) && replacement[position+1] >= '0' && replacement[position+1] <= '9' {
			position++
		}
	}
	return nil
}

func (h Headers) validate() error {
	for _, values := range []HeaderValues{h.LegacySet, h.Add, h.Set} {
		for name, value := range values {
			if !httpguts.ValidHeaderFieldName(name) {
				return fmt.Errorf("invalid header field %q", name)
			}
			if !httpguts.ValidHeaderFieldValue(value) {
				return fmt.Errorf("invalid header value for %q", name)
			}
		}
	}
	for _, name := range h.Remove {
		if !httpguts.ValidHeaderFieldName(name) {
			return fmt.Errorf("invalid header field %q", name)
		}
	}
	return nil
}

var (
	variablePattern = regexp.MustCompile(`\$[A-Za-z0-9_]+`)
	capturePattern  = regexp.MustCompile(`\$\{?([0-9]+)\}?`)
)

func resolveHeaderValue(r *http.Request, value string, captures []string) string {
	value = resolveCaptureValue(value, captures)
	return variablePattern.ReplaceAllStringFunc(value, func(variable string) string {
		return base.RequestVar(r, strings.TrimPrefix(variable, "$"), 0)
	})
}

func resolveCaptureValue(value string, captures []string) string {
	if len(captures) == 0 {
		return value
	}
	return capturePattern.ReplaceAllStringFunc(value, func(variable string) string {
		matches := capturePattern.FindStringSubmatch(variable)
		if len(matches) != 2 {
			return variable
		}
		index, err := strconv.Atoi(matches[1])
		if err != nil || index <= 0 || index >= len(captures) {
			return ""
		}
		return captures[index]
	})
}
