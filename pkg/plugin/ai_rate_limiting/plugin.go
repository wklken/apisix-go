package ai_rate_limiting

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/casbin/govaluate"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/ai_protocols"
	"github.com/wklken/apisix-go/pkg/plugin/ai_runtime"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/limit_count"
	"github.com/wklken/apisix-go/pkg/plugin/limitbase"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	v "github.com/wklken/apisix-go/pkg/apisix/variable"
)

type Plugin struct {
	base.BasePlugin
	config Config

	rateLimitState *limitbase.State
	apisixContext  base.APISIXPluginContext
	now            func() time.Time
	costExpr       *govaluate.EvaluableExpression
}

const (
	priority = 1030
	name     = "ai-rate-limiting"
)

var (
	variablePattern      = regexp.MustCompile(`\$\{?[A-Za-z0-9_]+\}?`)
	quotaVariablePattern = regexp.MustCompile(`\$\{([A-Za-z0-9_]+)(?:\s*\?\?\s*([^}]+))?\}|\$([A-Za-z0-9_]+)`)
)

var (
	errNoUsableRules     = errors.New("no usable rate limit rules")
	errQuotaStateMissing = errors.New("AI rate limit quota is exhausted before response accounting")
)

const schema = `
{
  "type": "object",
  "properties": {
    "limit": {
      "oneOf": [
        {"type": "integer", "exclusiveMinimum": 0},
        {"type": "string"}
      ]
    },
    "time_window": {
      "oneOf": [
        {"type": "integer", "exclusiveMinimum": 0},
        {"type": "string"}
      ]
    },
    "show_limit_quota_header": {
      "type": "boolean",
      "default": true
    },
    "limit_strategy": {
      "type": "string",
      "enum": ["total_tokens", "prompt_tokens", "completion_tokens", "expression"],
      "default": "total_tokens"
    },
    "cost_expr": {
      "type": "string",
      "minLength": 1
    },
    "instances": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "name": {
            "type": "string"
          },
          "limit": {
            "oneOf": [
              {"type": "integer", "minimum": 1},
              {"type": "string"}
            ]
          },
          "time_window": {
            "oneOf": [
              {"type": "integer", "minimum": 1},
              {"type": "string"}
            ]
          }
        },
        "required": ["name", "limit", "time_window"]
      }
    },
    "rejected_code": {
      "type": "integer",
      "minimum": 200,
      "maximum": 599,
      "default": 503
    },
    "rejected_msg": {
      "type": "string",
      "minLength": 1
    },
    "rules": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "count": {
            "oneOf": [
              {"type": "integer", "exclusiveMinimum": 0},
              {"type": "string"}
            ]
          },
          "time_window": {
            "oneOf": [
              {"type": "integer", "exclusiveMinimum": 0},
              {"type": "string"}
            ]
          },
          "key": {
            "type": "string"
          },
          "header_prefix": {
            "type": "string"
          }
        },
        "required": ["count", "time_window", "key"]
      }
    }
  },
  "dependentRequired": {
    "limit": ["time_window"],
    "time_window": ["limit"]
  },
  "oneOf": [
    {
      "anyOf": [
        {
          "required": ["limit", "time_window"]
        },
        {
          "required": ["instances"]
        }
      ]
    },
    {
      "required": ["rules"]
    }
  ]
}
`

type Config struct {
	Limit                any             `json:"limit,omitempty"`
	TimeWindow           any             `json:"time_window,omitempty"`
	ShowLimitQuotaHeader *bool           `json:"show_limit_quota_header,omitempty"`
	LimitStrategy        string          `json:"limit_strategy,omitempty"`
	CostExpr             string          `json:"cost_expr,omitempty"`
	Instances            []InstanceLimit `json:"instances,omitempty"`
	RejectedCode         int             `json:"rejected_code,omitempty"`
	RejectedMsg          string          `json:"rejected_msg,omitempty"`
	Rules                []Rule          `json:"rules,omitempty"`
}

type InstanceLimit struct {
	Name       string `json:"name"`
	Limit      any    `json:"limit"`
	TimeWindow any    `json:"time_window"`
}

type Rule struct {
	Count        any    `json:"count"`
	TimeWindow   any    `json:"time_window"`
	Key          string `json:"key"`
	HeaderPrefix string `json:"header_prefix,omitempty"`
}

type quota struct {
	key          string
	headerName   string
	headerPrefix string
	limit        int64
	window       time.Duration
}

type quotaResponseWriter struct {
	http.ResponseWriter
	plugin      *Plugin
	request     *http.Request
	state       *requestQuotaState
	wroteHeader bool
}

type requestQuotaState struct {
	mu      sync.Mutex
	quotas  []quota
	charged bool
}

type requestQuotaStateKey struct{}

func WithPickedAIInstanceName(r *http.Request, name string) *http.Request {
	return ai_runtime.WithSelectedInstanceName(r, name)
}

func PickedAIInstanceName(r *http.Request) (string, bool) {
	return ai_runtime.SelectedInstanceName(r)
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) PostInit() error {
	if len(p.config.Rules) > 0 && (len(p.config.Instances) > 0 || p.config.Limit != nil || p.config.TimeWindow != nil) {
		return errors.New("rules cannot be configured with limit, time_window, or instances")
	}
	if len(p.config.Instances) > 0 && ((p.config.Limit == nil) != (p.config.TimeWindow == nil)) {
		return errors.New("limit and time_window must be configured together")
	}
	if p.config.ShowLimitQuotaHeader == nil {
		show := true
		p.config.ShowLimitQuotaHeader = &show
	}
	if p.config.LimitStrategy == "" {
		p.config.LimitStrategy = "total_tokens"
	}
	if p.config.LimitStrategy == "expression" {
		if p.config.CostExpr == "" {
			return fmt.Errorf("cost_expr is required when limit_strategy is expression")
		}
		costExpr, err := govaluate.NewEvaluableExpressionWithFunctions(
			strings.ReplaceAll(p.config.CostExpr, "math.", ""),
			costExpressionFunctions(),
		)
		if err != nil {
			return fmt.Errorf("invalid cost_expr: %w", err)
		}
		p.costExpr = costExpr
	}
	for i, rule := range p.config.Rules {
		if rule.Key == "" {
			return fmt.Errorf("rule %d key is required", i+1)
		}
		if _, err := staticQuotaValue(rule.Count, fmt.Sprintf("rule %d count", i+1)); err != nil {
			return err
		}
		if _, err := staticQuotaValue(rule.TimeWindow, fmt.Sprintf("rule %d time_window", i+1)); err != nil {
			return err
		}
	}
	if p.config.RejectedCode == 0 {
		p.config.RejectedCode = http.StatusServiceUnavailable
	}
	if p.config.RejectedCode < 200 || p.config.RejectedCode > 599 {
		return fmt.Errorf("rejected_code must be between 200 and 599")
	}
	if len(p.config.Rules) == 0 && len(p.config.Instances) == 0 {
		if _, err := staticQuotaValue(p.config.Limit, "limit"); err != nil {
			return err
		}
		if _, err := staticQuotaValue(p.config.TimeWindow, "time_window"); err != nil {
			return err
		}
	}
	for _, instance := range p.config.Instances {
		if instance.Name == "" {
			return fmt.Errorf("instance name is required")
		}
		if _, err := staticQuotaValue(instance.Limit, "instance "+instance.Name+" limit"); err != nil {
			return err
		}
		if _, err := staticQuotaValue(instance.TimeWindow, "instance "+instance.Name+" time_window"); err != nil {
			return err
		}
	}

	if p.now == nil {
		p.now = time.Now
	}
	if p.rateLimitState == nil {
		p.rateLimitState = limitbase.NewStateWithClock(p.now)
	}
	return nil
}

func (p *Plugin) SetRateLimitState(state *limitbase.State) {
	p.rateLimitState = state
}

func (p *Plugin) SetAPISIXPluginContext(pluginContext base.APISIXPluginContext) error {
	p.apisixContext = pluginContext.Clone()
	return nil
}

// RunRequestPhase performs the admission check once after authentication and
// publishes immutable request-local quota bindings for response accounting.
// It does not wrap or invoke the downstream handler.
func (p *Plugin) RunRequestPhase(w http.ResponseWriter, r *http.Request) base.RequestPhaseResult {
	var quotas []quota
	for {
		var ok bool
		var err error
		quotas, ok, err = p.quotasForRequest(r)
		if err != nil {
			http.Error(w, "failed to get rate limit rules", http.StatusInternalServerError)
			return base.StopRequestWithSource(r, apisixctx.ResponseSourceAPISIX)
		}
		if !ok {
			return base.ContinueRequest(r)
		}
		rejectedIndex := p.reserveQuotas(quotas)
		if rejectedIndex < 0 {
			break
		}
		state := ai_runtime.FromRequest(r)
		if len(p.config.Rules) == 0 && state != nil && state.RateLimitFallbackEnabled() &&
			state.AdvanceRateLimitTarget() {
			continue
		}
		for _, headerQuota := range quotas[:rejectedIndex+1] {
			p.writeQuotaHeaders(w.Header(), headerQuota)
		}
		p.reject(w)
		return base.StopRequestWithSource(r, apisixctx.ResponseSourceEarlyStop)
	}
	requestState := &requestQuotaState{quotas: append([]quota(nil), quotas...)}
	request := r.WithContext(context.WithValue(r.Context(), requestQuotaStateKey{}, requestState))
	return base.ContinueRequest(request)
}

func requestQuotaStateFromRequest(r *http.Request) *requestQuotaState {
	if r == nil {
		return nil
	}
	state, _ := r.Context().Value(requestQuotaStateKey{}).(*requestQuotaState)
	return state
}

// SelectResponseMode chooses the one response accounting path after AI request
// preparation has published whether the selected operation is streaming.
func (*Plugin) SelectResponseMode(r *http.Request) base.RequestResponseMode {
	if state := ai_runtime.FromRequest(r); state != nil && state.StreamingIntent() {
		return base.RequestResponseModeStreaming
	}
	return base.RequestResponseModeBounded
}

// WrapStreamingResponse installs request-local quota headers and completion
// accounting. The returned writer owns exactly-once finalization; no quota
// quota state is owned by the process-scoped limit-count engine.
func (p *Plugin) WrapStreamingResponse(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, error) {
	state, err := p.responseQuotaState(r)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return w, nil
	}
	return &quotaResponseWriter{ResponseWriter: w, plugin: p, request: r, state: state}, nil
}

// RunBufferedBodyFilter accounts bounded responses after Plan 15 has produced
// the canonical body and before the final response is committed.
func (p *Plugin) RunBufferedBodyFilter(r *http.Request, response *base.ResponseState) error {
	if response == nil {
		return nil
	}
	state, err := p.responseQuotaState(r)
	if err != nil {
		return err
	}
	if state == nil {
		return nil
	}
	if response.Header == nil {
		response.Header = make(http.Header)
	}
	state.writeQuotaHeaders(p, r, response.Header)
	state.chargeOnce(p, r, response.Body)
	return nil
}

func (p *Plugin) responseQuotaState(r *http.Request) (*requestQuotaState, error) {
	if state := requestQuotaStateFromRequest(r); state != nil {
		return state, nil
	}
	quotas, ok, err := p.quotasForRequest(r)
	if err != nil || !ok {
		return nil, err
	}
	if p.reserveQuotas(quotas) >= 0 {
		return nil, errQuotaStateMissing
	}
	return &requestQuotaState{quotas: append([]quota(nil), quotas...)}, nil
}

func (s *requestQuotaState) quotasForResponse(p *Plugin, r *http.Request) []quota {
	if s == nil {
		return nil
	}
	if len(p.config.Rules) == 0 {
		if quotas, ok, err := p.quotasForRequest(r); err == nil && ok {
			return quotas
		}
	}
	return append([]quota(nil), s.quotas...)
}

func (s *requestQuotaState) writeQuotaHeaders(p *Plugin, r *http.Request, header http.Header) {
	for _, q := range s.quotasForResponse(p, r) {
		p.writeQuotaHeadersWithCredit(header, q, 0)
	}
}

func (s *requestQuotaState) chargeOnce(p *Plugin, r *http.Request, body []byte) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.charged {
		s.mu.Unlock()
		return
	}
	s.charged = true
	s.mu.Unlock()
	usedTokens := p.responseTokenCostForRequest(r, body)
	finalQuotas := s.quotasForResponse(p, r)
	if usedTokens <= 0 {
		return
	}
	for _, q := range finalQuotas {
		p.reconcile(q, usedTokens, true)
	}
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		result := p.RunRequestPhase(w, r)
		if result.Decision != base.RequestContinue {
			return
		}
		request := result.Request
		if request == nil {
			request = r
		}
		state := requestQuotaStateFromRequest(request)
		if state == nil {
			next.ServeHTTP(w, request)
			return
		}
		if runtimeState := ai_runtime.FromRequest(request); runtimeState != nil && runtimeState.StreamingIntent() {
			writer := &quotaResponseWriter{ResponseWriter: w, plugin: p, request: request, state: state}
			next.ServeHTTP(writer, request)
			writer.writeQuotaHeaders()
			state.chargeOnce(p, request, nil)
			return
		}

		recorder := base.GetOrCreateTransformResponseWriter(request)
		next.ServeHTTP(recorder, request)
		state.writeQuotaHeaders(p, request, recorder.Header())
		state.chargeOnce(p, request, recorder.Body())
		recorder.Commit(w)
	}
	return http.HandlerFunc(fn)
}

func (w *quotaResponseWriter) WriteHeader(statusCode int) {
	w.writeQuotaHeaders()
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *quotaResponseWriter) Write(body []byte) (int, error) {
	w.writeQuotaHeaders()
	return w.ResponseWriter.Write(body)
}

func (w *quotaResponseWriter) Flush() {
	w.writeQuotaHeaders()
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *quotaResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *quotaResponseWriter) FinishStreamingResponse(error) error {
	if w == nil || w.plugin == nil {
		return nil
	}
	w.writeQuotaHeaders()
	if w.state != nil {
		w.state.chargeOnce(w.plugin, w.request, nil)
	}
	return nil
}

func (w *quotaResponseWriter) writeQuotaHeaders() {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	var quotas []quota
	if w.state != nil {
		w.state.writeQuotaHeaders(w.plugin, w.request, w.Header())
		return
	} else {
		var ok bool
		var err error
		quotas, ok, err = w.plugin.quotasForRequest(w.request)
		if err != nil || !ok {
			return
		}
	}
	if len(quotas) == 0 {
		return
	}
	for _, q := range quotas {
		w.plugin.writeQuotaHeaders(w.Header(), q)
	}
}

// DescribeResponseMode advertises both bounded and streaming accounting; the
// actual response owner selects the mode per request.
func (*Config) DescribeResponseMode() (base.ResponseModeDescriptor, error) {
	return base.ResponseModeDescriptor{Modes: base.ResponseModeBounded | base.ResponseModeStreaming}, nil
}

func (p *Plugin) reserveQuotas(quotas []quota) int {
	for i, q := range quotas {
		if !p.reserve(q, 1) {
			return i
		}
	}
	return -1
}

func (p *Plugin) quotasForRequest(r *http.Request) ([]quota, bool, error) {
	if len(p.config.Rules) > 0 {
		document := p.limitCountRulesDocument()
		quotas := make([]quota, 0, len(p.config.Rules))
		for i, rule := range p.config.Rules {
			key, ok := resolveRuleKey(r, rule.Key)
			if !ok {
				continue
			}
			limit, err := resolveQuotaValue(r, rule.Count, fmt.Sprintf("rule %d count", i+1))
			if err != nil {
				continue
			}
			window, err := resolveQuotaValue(r, rule.TimeWindow, fmt.Sprintf("rule %d time_window", i+1))
			if err != nil {
				continue
			}
			windowDuration, err := quotaWindow(window, fmt.Sprintf("rule %d time_window", i+1))
			if err != nil {
				continue
			}
			headerPrefix := rule.HeaderPrefix
			if headerPrefix == "" {
				headerPrefix = strconv.Itoa(i + 1)
			}
			quotas = append(quotas, quota{
				key:          p.limitCountKey(document, key, nil),
				headerPrefix: headerPrefix,
				limit:        limit,
				window:       windowDuration,
			})
		}
		if len(quotas) == 0 {
			return nil, false, errNoUsableRules
		}
		return quotas, true, nil
	}

	q, ok, err := p.quotaForRequest(r)
	if err != nil || !ok {
		return nil, ok, err
	}
	return []quota{q}, true, nil
}

func resolveRuleKey(r *http.Request, key string) (string, bool) {
	resolved := 0
	key = variablePattern.ReplaceAllStringFunc(key, func(match string) string {
		variableName := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(match, "${"), "$"), "}")
		value := requestVariable(r, variableName)
		if value != "" {
			resolved++
		}
		return value
	})
	return key, resolved > 0 && key != ""
}

func (p *Plugin) quotaForRequest(r *http.Request) (quota, bool, error) {
	instanceName, hasInstance := PickedAIInstanceName(r)
	if hasInstance {
		for _, instance := range p.config.Instances {
			if instance.Name == instanceName {
				limit, err := resolveQuotaValue(r, instance.Limit, "instance "+instance.Name+" limit")
				if err != nil {
					return quota{}, false, err
				}
				window, err := resolveQuotaValue(r, instance.TimeWindow, "instance "+instance.Name+" time_window")
				if err != nil {
					return quota{}, false, err
				}
				windowDuration, err := quotaWindow(window, "instance "+instance.Name+" time_window")
				if err != nil {
					return quota{}, false, err
				}
				return quota{
					key:        p.limitCountInstanceKey(instance.Name, limit, windowDuration),
					headerName: instance.Name,
					limit:      limit,
					window:     windowDuration,
				}, true, nil
			}
		}
		if len(p.config.Instances) > 0 && (p.config.Limit == nil || p.config.TimeWindow == nil) {
			return quota{}, false, nil
		}
		limit, err := resolveQuotaValue(r, p.config.Limit, "limit")
		if err != nil {
			return quota{}, false, err
		}
		window, err := resolveQuotaValue(r, p.config.TimeWindow, "time_window")
		if err != nil {
			return quota{}, false, err
		}
		windowDuration, err := quotaWindow(window, "time_window")
		if err != nil {
			return quota{}, false, err
		}
		return quota{
			key:        p.limitCountInstanceKey("ai-rate-limiting#global", limit, windowDuration),
			headerName: instanceName,
			limit:      limit,
			window:     windowDuration,
		}, true, nil
	}

	if len(p.config.Instances) > 0 {
		return quota{}, false, nil
	}
	limit, err := resolveQuotaValue(r, p.config.Limit, "limit")
	if err != nil {
		return quota{}, false, err
	}
	window, err := resolveQuotaValue(r, p.config.TimeWindow, "time_window")
	if err != nil {
		return quota{}, false, err
	}
	windowDuration, err := quotaWindow(window, "time_window")
	if err != nil {
		return quota{}, false, err
	}
	return quota{
		key:        p.limitCountInstanceKey("ai-rate-limiting#global", limit, windowDuration),
		headerName: "global",
		limit:      limit,
		window:     windowDuration,
	}, true, nil
}

func (p *Plugin) hasAPISIXPluginContext() bool {
	return p.apisixContext.SourceResourceKey != "" || p.apisixContext.SourceID != ""
}

func (p *Plugin) limitCountKey(document map[string]any, key string, variant any) string {
	if !p.hasAPISIXPluginContext() {
		return key
	}
	scoped, err := limit_count.BuildLocalKeyWithVID(p.apisixContext, document, key, variant)
	if err != nil {
		return key
	}
	return scoped
}

func (p *Plugin) limitCountInstanceKey(name string, limit int64, window time.Duration) string {
	document := p.limitCountBaseDocument()
	document["count"] = limit
	document["time_window"] = int64(window / time.Second)
	document["key"] = name
	document["limit_header"] = "X-AI-RateLimit-Limit-" + strings.TrimPrefix(name, "ai-rate-limiting#global")
	document["remaining_header"] = "X-AI-RateLimit-Remaining-" + strings.TrimPrefix(name, "ai-rate-limiting#global")
	document["reset_header"] = "X-AI-RateLimit-Reset-" + strings.TrimPrefix(name, "ai-rate-limiting#global")
	legacyKey := "instance:" + name
	if name == "ai-rate-limiting#global" {
		legacyKey = "global"
	}
	if !p.hasAPISIXPluginContext() {
		return legacyKey
	}
	return p.limitCountKey(document, name, name)
}

func (p *Plugin) limitCountRulesDocument() map[string]any {
	document := p.limitCountBaseDocument()
	encoded, err := json.Marshal(p.config.Rules)
	if err == nil {
		var rules []any
		if json.Unmarshal(encoded, &rules) == nil {
			document["rules"] = rules
		}
	}
	return document
}

func (p *Plugin) limitCountBaseDocument() map[string]any {
	document := map[string]any{
		"policy": "local", "key_type": "constant", "allow_degradation": false,
		"sync_interval":    -1,
		"limit_header":     "X-AI-RateLimit-Limit",
		"remaining_header": "X-AI-RateLimit-Remaining",
		"reset_header":     "X-AI-RateLimit-Reset",
	}
	if p.config.RejectedCode != 0 {
		document["rejected_code"] = p.config.RejectedCode
	}
	if p.config.RejectedMsg != "" {
		document["rejected_msg"] = p.config.RejectedMsg
	}
	if p.config.ShowLimitQuotaHeader != nil {
		document["show_limit_quota_header"] = *p.config.ShowLimitQuotaHeader
	}
	if metadata, ok := p.apisixContext.SourceConfig["_meta"]; ok {
		document["_meta"] = metadata
	}
	return document
}

func quotaWindow(seconds int64, name string) (time.Duration, error) {
	const maxSeconds = int64((1<<63 - 1) / int64(time.Second))
	if seconds > maxSeconds {
		return 0, fmt.Errorf("%s exceeds the maximum supported duration", name)
	}
	return time.Duration(seconds) * time.Second, nil
}

func staticQuotaValue(value any, name string) (int64, error) {
	if text, ok := value.(string); ok && strings.Contains(text, "$") {
		return 1, nil
	}
	return numericQuotaValue(value, name)
}

func resolveQuotaValue(r *http.Request, value any, name string) (int64, error) {
	if text, ok := value.(string); ok {
		value = quotaVariablePattern.ReplaceAllStringFunc(text, func(match string) string {
			parts := quotaVariablePattern.FindStringSubmatch(match)
			variableName := parts[1]
			if variableName == "" {
				variableName = parts[3]
			}
			if resolved := requestVariable(r, variableName); resolved != "" {
				return resolved
			}
			return strings.TrimSpace(parts[2])
		})
	}
	return numericQuotaValue(value, name)
}

func numericQuotaValue(value any, name string) (int64, error) {
	switch typed := value.(type) {
	case int:
		return positiveQuotaValue(int64(typed), name)
	case int64:
		return positiveQuotaValue(typed, name)
	case float64:
		if math.Trunc(typed) != typed {
			return 0, fmt.Errorf("%s must be a positive integer", name)
		}
		return positiveQuotaValue(int64(typed), name)
	case string:
		parsed, err := strconv.ParseInt(typed, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s must resolve to a positive integer: %w", name, err)
		}
		return positiveQuotaValue(parsed, name)
	default:
		return 0, fmt.Errorf("%s must be a positive integer or string", name)
	}
}

func positiveQuotaValue(value int64, name string) (int64, error) {
	if value <= 0 {
		return 0, fmt.Errorf("%s must be greater than 0", name)
	}
	return value, nil
}

func requestVariable(r *http.Request, key string) string {
	key = strings.TrimPrefix(key, "$")
	if after, ok := strings.CutPrefix(key, "http_"); ok {
		return r.Header.Get(strings.ReplaceAll(after, "_", "-"))
	}

	variableName := "$" + key
	if _, ok := v.RequestVars[variableName]; ok {
		return fmt.Sprint(v.GetRequestVar(r, variableName))
	}
	if _, ok := v.ApisixVars[variableName]; ok {
		return fmt.Sprint(v.GetApisixVar(r, variableName))
	}
	return v.GetNginxVar(r, variableName)
}

func (p *Plugin) reserve(q quota, tokens int64) bool {
	if tokens <= 0 || tokens > q.limit {
		return false
	}
	return p.rateLimitState.FixedWindow(q.key, q.limit, tokens, q.window, false).Allowed
}

func (p *Plugin) reconcile(q quota, delta int64, create bool) {
	if delta > 0 {
		p.rateLimitState.FixedWindow(q.key, q.limit, delta, q.window, true)
		return
	}
	if delta < 0 {
		p.rateLimitState.AdjustFixedWindow(q.key, q.limit, delta, q.window, create)
	}
}

func (p *Plugin) responseTokenCost(body []byte) int64 {
	var decoded struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.Usage == nil {
		return 0
	}

	if p.config.LimitStrategy == "expression" {
		return p.expressionCost(decoded.Usage)
	}

	value := ai_protocols.NumericUsage(decoded.Usage[p.config.LimitStrategy], true)
	if value < 0 {
		return 0
	}
	return value
}

func (p *Plugin) responseTokenCostForRequest(r *http.Request, body []byte) int64 {
	if rawUsage, ok := apisixctx.GetRequestVar(r, "$llm_raw_usage").(map[string]any); ok {
		if p.config.LimitStrategy == "expression" {
			return p.expressionCost(rawUsage)
		}
		if value, exists := rawUsage[p.config.LimitStrategy]; exists {
			if numeric := ai_protocols.NumericUsage(value, true); numeric >= 0 {
				return numeric
			}
		}
	}
	if usage, ok := apisixctx.GetRequestVar(r, "$ai_token_usage").(map[string]any); ok {
		if value := ai_protocols.NumericUsage(usage[p.config.LimitStrategy], true); value >= 0 {
			return value
		}
	}
	return p.responseTokenCost(body)
}

func (p *Plugin) expressionCost(usage map[string]any) int64 {
	if p.costExpr == nil {
		return 0
	}
	value, err := p.costExpr.Eval(expressionParameters(usage))
	if err != nil {
		return 0
	}
	result, ok := value.(float64)
	if !ok || math.IsNaN(result) || math.IsInf(result, 0) || result >= float64(1<<63-1) {
		return 0
	}
	if result < 0 {
		return 0
	}
	return int64(math.Floor(result + 0.5))
}

type expressionParameters map[string]any

func (p expressionParameters) Get(name string) (any, error) {
	return numericExpressionValue(p[name]), nil
}

func numericExpressionValue(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	default:
		return 0
	}
}

func costExpressionFunctions() map[string]govaluate.ExpressionFunction {
	return map[string]govaluate.ExpressionFunction{
		"abs": func(arguments ...any) (any, error) {
			values, err := numericArguments(arguments, 1)
			if err != nil {
				return nil, err
			}
			return math.Abs(values[0]), nil
		},
		"ceil": func(arguments ...any) (any, error) {
			values, err := numericArguments(arguments, 1)
			if err != nil {
				return nil, err
			}
			return math.Ceil(values[0]), nil
		},
		"floor": func(arguments ...any) (any, error) {
			values, err := numericArguments(arguments, 1)
			if err != nil {
				return nil, err
			}
			return math.Floor(values[0]), nil
		},
		"sqrt": unaryMathFunction(math.Sqrt),
		"exp":  unaryMathFunction(math.Exp),
		"log":  unaryMathFunction(math.Log),
		"sin":  unaryMathFunction(math.Sin),
		"cos":  unaryMathFunction(math.Cos),
		"tan":  unaryMathFunction(math.Tan),
		"asin": unaryMathFunction(math.Asin),
		"acos": unaryMathFunction(math.Acos),
		"atan": unaryMathFunction(math.Atan),
		"pow": func(arguments ...any) (any, error) {
			values, err := numericArguments(arguments, 2)
			if err != nil || len(values) != 2 {
				return nil, fmt.Errorf("pow expects exactly 2 numeric arguments")
			}
			return math.Pow(values[0], values[1]), nil
		},
		"max": func(arguments ...any) (any, error) {
			values, err := numericArguments(arguments, 1)
			if err != nil {
				return nil, err
			}
			result := values[0]
			for _, value := range values[1:] {
				if value > result {
					result = value
				}
			}
			return result, nil
		},
		"min": func(arguments ...any) (any, error) {
			values, err := numericArguments(arguments, 1)
			if err != nil {
				return nil, err
			}
			result := values[0]
			for _, value := range values[1:] {
				if value < result {
					result = value
				}
			}
			return result, nil
		},
	}
}

func unaryMathFunction(fn func(float64) float64) govaluate.ExpressionFunction {
	return func(arguments ...any) (any, error) {
		values, err := numericArguments(arguments, 1)
		if err != nil || len(values) != 1 {
			return nil, fmt.Errorf("expected exactly 1 numeric argument")
		}
		return fn(values[0]), nil
	}
}

func numericArguments(arguments []any, minimum int) ([]float64, error) {
	if len(arguments) < minimum {
		return nil, fmt.Errorf("expected at least %d numeric arguments", minimum)
	}

	values := make([]float64, len(arguments))
	for i, argument := range arguments {
		value, ok := argument.(float64)
		if !ok {
			return nil, fmt.Errorf("argument %d must be numeric", i+1)
		}
		values[i] = value
	}
	return values, nil
}

func (p *Plugin) writeQuotaHeaders(header http.Header, q quota) {
	p.writeQuotaHeadersWithCredit(header, q, 0)
}

func (p *Plugin) writeQuotaHeadersWithCredit(
	header http.Header, q quota, reservationCredit int64,
) {
	if p.config.ShowLimitQuotaHeader != nil && !*p.config.ShowLimitQuotaHeader {
		return
	}

	used, reset := p.snapshot(q)
	remaining := max(min(q.limit-used+reservationCredit, q.limit), 0)
	if q.headerPrefix != "" {
		header.Set("X-AI-"+q.headerPrefix+"-RateLimit-Limit", strconv.FormatInt(q.limit, 10))
		header.Set("X-AI-"+q.headerPrefix+"-RateLimit-Remaining", strconv.FormatInt(remaining, 10))
		header.Set("X-AI-"+q.headerPrefix+"-RateLimit-Reset", strconv.FormatInt(reset, 10))
		return
	}
	header.Set("X-AI-RateLimit-Limit-"+q.headerName, strconv.FormatInt(q.limit, 10))
	header.Set("X-AI-RateLimit-Remaining-"+q.headerName, strconv.FormatInt(remaining, 10))
	header.Set("X-AI-RateLimit-Reset-"+q.headerName, strconv.FormatInt(reset, 10))
}

func (p *Plugin) snapshot(q quota) (int64, int64) {
	state := p.rateLimitState.FixedWindowSnapshot(q.key, q.limit, q.window)
	return q.limit - state.Remaining, max(int64(math.Ceil(state.Reset.Seconds())), 0)
}

func (p *Plugin) reject(w http.ResponseWriter) {
	if p.config.RejectedMsg == "" {
		http.Error(w, http.StatusText(p.config.RejectedCode), p.config.RejectedCode)
		return
	}
	payload, _ := json.Marshal(map[string]string{"error_msg": p.config.RejectedMsg})
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(p.config.RejectedCode)
	_, _ = w.Write(append(payload, '\n'))
}
