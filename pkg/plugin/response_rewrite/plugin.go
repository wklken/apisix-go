package response_rewrite

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	brotlidec "github.com/andybalholm/brotli"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/ai_runtime"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	pluginexpr "github.com/wklken/apisix-go/pkg/plugin/expr"
	"github.com/wklken/apisix-go/pkg/secret"
)

type Plugin struct {
	base.BasePlugin
	config Config
	expr   *pluginexpr.Expression

	secretMu   sync.RWMutex
	body       *secret.Value
	legacyBody *string
	stopped    bool
}

const (
	priority = 899
	name     = "response-rewrite"
)

const schema = `
{
  "type": "object",
  "properties": {
    "headers": {
      "type": "object",
      "minProperties": 1,
      "anyOf": [
        {
          "patternProperties": {
            "^[^:]+$": {
              "oneOf": [
                {"type": "string"},
                {"type": "number"}
              ]
            }
          },
          "additionalProperties": false
        },
        {
          "properties": {
            "add": {
              "type": "array",
              "minItems": 1,
              "items": {
                "type": "string",
                "pattern": "^[^:]+:[^:]*[^/]$"
              }
            },
            "set": {
              "type": "object",
              "minProperties": 1,
              "patternProperties": {
                "^[^:]+$": {
                  "oneOf": [
                    {"type": "string"},
                    {"type": "number"}
                  ]
                }
              },
              "additionalProperties": false
            },
            "remove": {
              "type": "array",
              "minItems": 1,
              "items": {
                "type": "string",
                "pattern": "^[^:]+$"
              }
            }
          },
          "additionalProperties": false
        }
      ]
    },
	    "body": {
	      "type": "string"
	    },
	    "body_secret": {
	      "type": "string",
	      "minLength": 1,
	      "description": "Go extension: explicitly opted-in APISIX data-encryption ciphertext"
	    },
    "body_base64": {
      "type": "boolean",
      "default": false
    },
    "status_code": {
      "type": "integer",
      "minimum": 200,
      "maximum": 598
    },
    "vars": {
      "type": "array"
    },
    "filters": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "required": ["regex", "replace"],
        "properties": {
          "regex": {
            "type": "string",
            "minLength": 1
          },
          "scope": {
            "type": "string",
            "enum": ["once", "global"],
            "default": "once"
          },
          "replace": {
            "type": "string"
          },
          "options": {
            "type": "string",
            "default": "jo"
          }
        }
      }
    }
  },
  "dependencies": {
    "body": {
      "not": { "required": ["filters"] }
    },
    "filters": {
      "not": { "required": ["body"] }
    }
  }
}
`

type Config struct {
	Headers    Headers  `json:"headers"`
	Body       *string  `json:"body,omitempty"`
	BodySecret *string  `json:"body_secret,omitempty"`
	BodyBase64 *bool    `json:"body_base64,omitempty"`
	StatusCode int      `json:"status_code,omitempty"`
	Vars       []any    `json:"vars,omitempty"`
	Filters    []Filter `json:"filters,omitempty"`
}

type Filter struct {
	Regex   string `json:"regex,omitempty"`
	Scope   string `json:"scope,omitempty"`
	Replace string `json:"replace,omitempty"`
	Options string `json:"options,omitempty"`

	pattern *regexp.Regexp
}

type Headers struct {
	Add       []string          `json:"add,omitempty"`
	Set       map[string]string `json:"set,omitempty"`
	Remove    []string          `json:"remove,omitempty"`
	LegacySet map[string]string `json:"-"`
}

func (h *Headers) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := jsonUnmarshal(data, &raw); err != nil {
		return err
	}
	_, addConfigured := raw["add"].([]any)
	_, setConfigured := raw["set"].(map[string]any)
	_, removeConfigured := raw["remove"].([]any)
	if addConfigured || setConfigured || removeConfigured {
		var err error
		h.Add, err = stringValues(raw["add"], "headers.add")
		if err != nil {
			return err
		}
		h.Set, err = headerValues(raw["set"], "headers.set")
		if err != nil {
			return err
		}
		h.Remove, err = stringValues(raw["remove"], "headers.remove")
		if err != nil {
			return err
		}
		return nil
	}

	legacy, err := headerValues(raw, "headers")
	if err != nil {
		return err
	}
	h.LegacySet = legacy
	return nil
}

func stringValues(value any, name string) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array", name)
	}
	values := make([]string, len(items))
	for i, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%s item %d must be a string", name, i)
		}
		values[i] = text
	}
	return values, nil
}

func headerValues(value any, name string) (map[string]string, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", name)
	}
	values := make(map[string]string, len(items))
	for field, value := range items {
		switch typed := value.(type) {
		case string:
			values[field] = typed
		case float64:
			values[field] = strconv.FormatFloat(typed, 'f', -1, 64)
		default:
			return nil, fmt.Errorf("%s.%s must be a string or number", name, field)
		}
	}
	return values, nil
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.config.BodyBase64 == nil {
		b := false
		p.config.BodyBase64 = &b
	}
	if err := p.ValidatePreMaterialization(); err != nil {
		return err
	}
	if _, err := p.useBody(func(body string) error {
		return p.validateEffectiveBody(body)
	}); err != nil {
		return err
	}
	if len(p.config.Vars) > 0 {
		expr, err := pluginexpr.Compile(p.config.Vars)
		if err != nil {
			return fmt.Errorf("response-rewrite vars validation failed: %w", err)
		}
		p.expr = expr
	}
	for i := range p.config.Filters {
		if p.config.Filters[i].Scope == "" {
			p.config.Filters[i].Scope = "once"
		}
		if p.config.Filters[i].Scope != "once" && p.config.Filters[i].Scope != "global" {
			return fmt.Errorf("response-rewrite filter scope %q is not supported", p.config.Filters[i].Scope)
		}
		if p.config.Filters[i].Options == "" {
			p.config.Filters[i].Options = "jo"
		}
		pattern, err := compileFilterPattern(p.config.Filters[i].Regex, p.config.Filters[i].Options)
		if err != nil {
			return fmt.Errorf("response-rewrite filter regex %q validation failed: %w", p.config.Filters[i].Regex, err)
		}
		p.config.Filters[i].pattern = pattern
	}

	return nil
}

func (p *Plugin) ValidatePreMaterialization() error {
	if p.config.Body != nil && p.config.BodySecret != nil {
		return fmt.Errorf("response-rewrite body and body_secret cannot be configured together")
	}
	if p.config.BodySecret != nil && len(p.config.Filters) > 0 {
		return fmt.Errorf("response-rewrite body_secret and filters cannot be configured together")
	}
	if p.config.Body != nil && len(p.config.Filters) > 0 {
		return fmt.Errorf("response-rewrite body and filters cannot be configured together")
	}
	return nil
}

// MaterializeSecrets is the transitional process-local compatibility path.
// Scoped generation preparation uses MaterializeScopedSecrets instead.
func (p *Plugin) MaterializeSecrets() error {
	p.secretMu.Lock()
	defer p.secretMu.Unlock()
	if p.stopped {
		return secret.ErrCredentialUnavailable
	}
	if p.body != nil || p.legacyBody != nil {
		return nil
	}
	field, raw, present, err := p.selectedBody()
	if err != nil {
		return err
	}
	if !present || (field == "body" && raw == "") {
		return p.validateEffectiveBody(raw)
	}
	resolver := p.DataEncryption()
	if !resolver.Configured() {
		return errors.New("data-encryption resolver is required")
	}
	var resolved string
	if field == "body_secret" {
		resolved, err = resolver.ResolveForContext(raw, name+"."+field)
	} else {
		resolved = resolver.ResolveOptionalForContext(raw, name+"."+field)
	}
	if err != nil {
		return fmt.Errorf("%s %s: %w", name, field, secret.ErrCredentialUnavailable)
	}
	if err := p.validateEffectiveBody(resolved); err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(resolved))
	descriptor, err := secret.NewDescriptor(capability.SecretPluginConfig, digest)
	if err != nil {
		return fmt.Errorf("%s %s: %w", name, field, secret.ErrCredentialUnavailable)
	}
	owned := resolved
	p.installBody(field, descriptor.String())
	p.legacyBody = &owned
	return nil
}

// MaterializeScopedSecrets admits the selected manifest-owned response body
// for this immutable attempt. Config retains only its content descriptor.
func (p *Plugin) MaterializeScopedSecrets(
	ctx context.Context, access base.ScopedSecretAccess,
) error {
	p.secretMu.Lock()
	defer p.secretMu.Unlock()
	if p.stopped {
		return secret.ErrCredentialUnavailable
	}
	if p.body != nil || p.legacyBody != nil {
		return nil
	}
	field, raw, present, err := p.selectedBody()
	if err != nil {
		return err
	}
	if !present || (field == "body" && raw == "") {
		return p.validateEffectiveBody(raw)
	}
	value, err := access.Materialize(ctx, field, raw)
	if err != nil {
		return fmt.Errorf("%s %s: %w", name, field, secret.ErrCredentialUnavailable)
	}
	if err := value.Use(func(plaintext string) error {
		return p.validateEffectiveBody(plaintext)
	}); err != nil {
		return fmt.Errorf("%s %s: %w", name, field, secret.ErrCredentialUnavailable)
	}
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil {
		return fmt.Errorf("%s %s: %w", name, field, secret.ErrCredentialUnavailable)
	}
	p.installBody(field, descriptor.String())
	p.body = &value
	return nil
}

func (p *Plugin) selectedBody() (field, raw string, present bool, err error) {
	if err := p.ValidatePreMaterialization(); err != nil {
		return "", "", false, err
	}
	if p.config.BodySecret != nil {
		if *p.config.BodySecret == "" {
			return "", "", false, fmt.Errorf("response-rewrite body_secret must not be empty")
		}
		return "body_secret", *p.config.BodySecret, true, nil
	}
	if p.config.Body != nil {
		return "body", *p.config.Body, true, nil
	}
	return "", "", false, nil
}

func (p *Plugin) installBody(field, descriptor string) {
	if field == "body_secret" {
		p.config.BodySecret = &descriptor
		return
	}
	p.config.Body = &descriptor
}

func (p *Plugin) validateEffectiveBody(body string) error {
	if p.config.BodyBase64 == nil || !*p.config.BodyBase64 {
		return nil
	}
	if body == "" {
		return fmt.Errorf("response-rewrite body_base64 requires a non-empty body")
	}
	if _, err := base64.StdEncoding.DecodeString(body); err != nil {
		return fmt.Errorf("response-rewrite body is not valid base64: %w", err)
	}
	return nil
}

func (p *Plugin) useBody(use func(string) error) (bool, error) {
	if use == nil {
		return false, secret.ErrCredentialUnavailable
	}
	p.secretMu.RLock()
	defer p.secretMu.RUnlock()
	field, raw, present, err := p.selectedBody()
	if err != nil || !present {
		return present, err
	}
	if p.stopped {
		return true, secret.ErrCredentialUnavailable
	}
	if p.body != nil {
		return true, p.body.Use(use)
	}
	if p.legacyBody != nil {
		return true, use(*p.legacyBody)
	}
	if field == "body" && raw == "" {
		return true, use("")
	}
	return true, secret.ErrCredentialUnavailable
}

func (p *Plugin) Stop() {
	p.secretMu.Lock()
	defer p.secretMu.Unlock()
	if p.stopped {
		return
	}
	p.stopped = true
	if p.body != nil {
		*p.body = secret.Value{}
		p.body = nil
	}
	if p.legacyBody != nil {
		*p.legacyBody = ""
		p.legacyBody = nil
	}
}

func (p *Plugin) Config() any {
	return &p.config
}

// DescribeBindingPhases selects the response owner from the initialized
// configuration. Header-only rewrites can run at final header commit; every
// other configuration keeps the bounded body callback because it may change
// status or need the complete response body.
func (p Config) DescribeBindingPhases() (base.BindingPhaseDescriptor, error) {
	if p.pureHeaderOnly() {
		return base.BindingPhaseDescriptor{RequestStage: "none", StreamingHeader: true}, nil
	}
	return base.BindingPhaseDescriptor{RequestStage: "none", BufferedBody: true}, nil
}

func (p Config) DescribeResponseMode() (base.ResponseModeDescriptor, error) {
	if p.pureHeaderOnly() {
		return base.ResponseModeDescriptor{
			Modes: base.ResponseModeBounded | base.ResponseModeStreaming,
		}, nil
	}
	return base.ResponseModeDescriptor{Modes: base.ResponseModeBounded}, nil
}

// SelectResponseMode keeps early-stop and non-streaming AI responses on the
// bounded fallback while allowing an AI streaming terminal to remain
// transparent. Both paths run the same header rewrite exactly once.
func (*Plugin) SelectResponseMode(r *http.Request) base.RequestResponseMode {
	if state := ai_runtime.FromRequest(r); state != nil && state.StreamingIntent() {
		return base.RequestResponseModeStreaming
	}
	return base.RequestResponseModeBounded
}

func (p Config) pureHeaderOnly() bool {
	return !p.Headers.empty() && p.StatusCode == 0 && len(p.Vars) == 0 &&
		p.Body == nil && p.BodySecret == nil && len(p.Filters) == 0 &&
		!p.Headers.hasBodyLengthVariable()
}

// RunBufferedBodyFilter applies status, header and body rewrites atomically to
// the canonical response state owned by the bounded executor.
func (p *Plugin) RunBufferedBodyFilter(r *http.Request, state *base.ResponseState) error {
	if state == nil || !p.AppliesToResponseSource(responseSource(r)) {
		return nil
	}
	recorder := responseRecorder(r, state)
	if p.varsMatched(r, recorder) {
		if err := p.rewrite(r, recorder); err != nil {
			return err
		}
	}
	state.Status = recorder.StatusCode()
	state.Header = recorder.Header().Clone()
	state.Body = append([]byte(nil), recorder.Body()...)
	return nil
}

// RunStreamingHeaderFilter applies pure header rewrites at the upstream
// response's final header commit. It deliberately never changes status or
// body; configurations that need either remain on the bounded callback.
func (p *Plugin) RunStreamingHeaderFilter(r *http.Request, state *base.StreamingResponseState) error {
	if state == nil || !p.config.pureHeaderOnly() || responseSource(r) == apisixctx.ResponseSourceCacheHit {
		return nil
	}
	if state.Header == nil {
		state.Header = make(http.Header)
	}
	p.config.Headers.applyTo(state.Header, func(value string) string {
		return resolveValueFromResponse(r, state.Status, state.Header, 0, value)
	})
	return nil
}

func (p *Plugin) AppliesToResponseSource(source apisixctx.ResponseSource) bool {
	switch source {
	case apisixctx.ResponseSourceUpstream, apisixctx.ResponseSourceAPISIX, apisixctx.ResponseSourceEarlyStop:
		return true
	default:
		return false
	}
}

func responseRecorder(r *http.Request, state *base.ResponseState) *base.BufferedResponseWriter {
	recorder := base.GetOrCreateTransformResponseWriter(r)
	for field, values := range state.Header {
		recorder.Header()[field] = append([]string(nil), values...)
	}
	recorder.SetStatusCode(state.Status)
	recorder.SetBody(state.Body)
	return recorder
}

func responseSource(r *http.Request) apisixctx.ResponseSource {
	if lifecycle := apisixctx.GetRequestLifecycle(r); lifecycle != nil {
		return lifecycle.ResponseSource()
	}
	if source, _ := apisixctx.GetRequestVar(r, "$response_source").(string); source != "" {
		return apisixctx.ResponseSource(source)
	}
	return apisixctx.ResponseSourceUnknown
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if apisixctx.GetRequestLifecycle(r) != nil {
			next.ServeHTTP(w, r)
			return
		}
		recorder := base.GetOrCreateTransformResponseWriter(r)
		next.ServeHTTP(recorder, r)

		if p.varsMatched(r, recorder) {
			if err := p.rewrite(r, recorder); err != nil {
				http.Error(w, "response-rewrite body unavailable", http.StatusInternalServerError)
				return
			}
		}
		writeRewrittenResponse(w, recorder)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) rewrite(r *http.Request, resp *base.BufferedResponseWriter) error {
	if p.config.StatusCode != 0 {
		resp.SetStatusCode(p.config.StatusCode)
	}
	responseEncoding := ""
	if p.config.Body != nil || p.config.BodySecret != nil || len(p.config.Filters) > 0 {
		responseEncoding = resp.Header().Get("Content-Encoding")
		clearHeadersAsBodyModified(resp.Header())
	}

	_, err := p.useBody(func(effectiveBody string) error {
		var body []byte
		if p.config.BodyBase64 != nil && *p.config.BodyBase64 {
			decoded, err := base64.StdEncoding.DecodeString(effectiveBody)
			if err == nil {
				body = decoded
			}
		} else {
			body = []byte(effectiveBody)
		}
		if body != nil && !bytes.Equal(body, resp.Body()) {
			resp.ReplaceBody(body)
		}
		return nil
	})
	if err != nil {
		return err
	}

	if len(p.config.Filters) > 0 {
		body := resp.Body()
		bodyChanged := false
		canFilter := true
		if responseEncoding != "" {
			decoded, ok := decodeFilterBody(resp, responseEncoding)
			if !ok {
				canFilter = false
				logger.Errorf(
					"filters may not work as expected due to unsupported compression encoding type: %s",
					responseEncoding,
				)
			} else {
				body = decoded
				bodyChanged = true
			}
		}
		if canFilter {
			for _, filter := range p.config.Filters {
				if filter.Scope == "global" {
					body = []byte(filter.pattern.ReplaceAllString(string(body), filter.Replace))
					continue
				}
				body = []byte(replaceFirstString(filter.pattern, string(body), filter.Replace))
			}
			if bodyChanged || !bytes.Equal(body, resp.Body()) {
				resp.ReplaceBody(body)
			}
		}
	}

	p.config.Headers.apply(r, resp)
	return nil
}

func clearHeadersAsBodyModified(header http.Header) {
	for actual := range header {
		switch {
		case strings.EqualFold(actual, "Content-Length"),
			strings.EqualFold(actual, "Content-Encoding"),
			strings.EqualFold(actual, "Last-Modified"),
			strings.EqualFold(actual, "ETag"):
			delete(header, actual)
		}
	}
}

func (p *Plugin) varsMatched(r *http.Request, resp *base.BufferedResponseWriter) bool {
	if p.expr == nil {
		return true
	}
	return p.expr.Eval(func(name string) any {
		return responseValue(r, resp, name)
	})
}

func (h Headers) apply(r *http.Request, resp *base.BufferedResponseWriter) {
	h.applyTo(resp.Header(), func(value string) string {
		return resolveValue(r, resp, value)
	})
}

func (h Headers) applyTo(header http.Header, resolve func(string) string) {
	for _, entry := range h.Add {
		field, value, ok := strings.Cut(entry, ":")
		if !ok {
			continue
		}
		header.Add(strings.TrimSpace(field), resolve(strings.TrimSpace(value)))
	}
	for field, value := range h.LegacySet {
		resolved := resolve(value)
		if resolved == "" {
			header[http.CanonicalHeaderKey(field)] = nil
			continue
		}
		header.Set(field, resolved)
	}
	for field, value := range h.Set {
		header.Set(field, resolve(value))
	}
	for _, field := range h.Remove {
		header.Del(field)
	}
}

func (h Headers) empty() bool {
	return len(h.Add) == 0 && len(h.Set) == 0 && len(h.Remove) == 0 && len(h.LegacySet) == 0
}

func (h Headers) hasBodyLengthVariable() bool {
	for _, entry := range h.Add {
		_, value, ok := strings.Cut(entry, ":")
		if ok && hasBodyLengthVariable(value) {
			return true
		}
	}
	for _, value := range h.Set {
		if hasBodyLengthVariable(value) {
			return true
		}
	}
	for _, value := range h.LegacySet {
		if hasBodyLengthVariable(value) {
			return true
		}
	}
	return false
}

func hasBodyLengthVariable(value string) bool {
	for _, variable := range base.RequestVariablePattern.FindAllString(value, -1) {
		if variable == "$bytes_sent" || variable == "$body_bytes_sent" {
			return true
		}
	}
	return false
}

func compileFilterPattern(pattern string, options string) (*regexp.Regexp, error) {
	var prefix strings.Builder
	for _, flag := range options {
		switch flag {
		case 'i':
			prefix.WriteString("(?i)")
		case 'm':
			prefix.WriteString("(?m)")
		case 's':
			prefix.WriteString("(?s)")
		case 'o':
			// no-op: "o" is accepted by APISIX's gsub flags but has no Go equivalent
		case 'j':
			// no-op: "j" is accepted by APISIX's gsub flags but has no Go equivalent
		default:
			return nil, fmt.Errorf("unknown flag %q (flags %q)", string(flag), options)
		}
	}
	return regexp.Compile(prefix.String() + pattern)
}

func replaceFirstString(pattern *regexp.Regexp, body string, replacement string) string {
	replaced := false
	return pattern.ReplaceAllStringFunc(body, func(match string) string {
		if replaced {
			return match
		}
		replaced = true
		return pattern.ReplaceAllString(match, replacement)
	})
}

func decodeFilterBody(resp *base.BufferedResponseWriter, encoding string) ([]byte, bool) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(resp.Body()))
		if err != nil {
			return nil, false
		}
		defer func() { _ = reader.Close() }()
		return readLimitedDecodedBody(reader)
	case "br":
		return readLimitedDecodedBody(brotlidec.NewReader(bytes.NewReader(resp.Body())))
	default:
		return nil, false
	}
}

func readLimitedDecodedBody(reader io.Reader) ([]byte, bool) {
	decoded, err := io.ReadAll(io.LimitReader(reader, base.DefaultBufferedResponseMaxBytes+1))
	if err != nil || int64(len(decoded)) > base.DefaultBufferedResponseMaxBytes {
		return nil, false
	}
	return decoded, true
}

func expressionString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case []string:
		return strings.Join(typed, ",")
	case []any:
		parts := make([]string, len(typed))
		for i, item := range typed {
			parts[i] = fmt.Sprint(item)
		}
		return strings.Join(parts, ",")
	default:
		return fmt.Sprint(value)
	}
}

func resolveValue(r *http.Request, resp *base.BufferedResponseWriter, value string) string {
	return resolveValueFromResponse(r, resp.StatusCode(), resp.Header(), len(resp.Body()), value)
}

func responseValue(r *http.Request, resp *base.BufferedResponseWriter, name string) any {
	return responseValueFromResponse(r, resp.StatusCode(), resp.Header(), len(resp.Body()), name)
}

func resolveValueFromResponse(r *http.Request, status int, header http.Header, bodyLength int, value string) string {
	return base.ResolveRequestVariables(value, func(name string) string {
		return expressionString(responseValueFromResponse(r, status, header, bodyLength, name))
	})
}

func responseValueFromResponse(r *http.Request, status int, header http.Header, bodyLength int, name string) any {
	name = strings.TrimPrefix(name, "$")
	switch {
	case name == "status", name == "status_code", name == "upstream_status":
		return status
	case strings.HasPrefix(name, "sent_http_"), strings.HasPrefix(name, "upstream_http_"):
		prefix := "sent_http_"
		if strings.HasPrefix(name, "upstream_http_") {
			prefix = "upstream_http_"
		}
		headerName := strings.ReplaceAll(strings.TrimPrefix(name, prefix), "_", "-")
		return pluginexpr.HeaderValue(header, headerName)
	case name == "body_bytes_sent" || name == "bytes_sent":
		return bodyLength
	}
	return pluginexpr.RequestValue(r, name)
}

func writeRewrittenResponse(w http.ResponseWriter, resp *base.BufferedResponseWriter) {
	for field, values := range resp.Header() {
		if len(values) == 0 {
			w.Header()[field] = nil
			continue
		}
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	w.WriteHeader(resp.StatusCode())
	resp.WriteBodyTo(w)
}

func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
