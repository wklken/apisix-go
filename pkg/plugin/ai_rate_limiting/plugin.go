package ai_rate_limiting

import (
	"bytes"
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
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	v "github.com/wklken/apisix-go/pkg/apisix/variable"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/ai_runtime"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config

	mu       sync.Mutex
	counters map[string]*counter
	now      func() time.Time
	costExpr *govaluate.EvaluableExpression
}

const (
	priority = 1030
	name     = "ai-rate-limiting"
)

var variablePattern = regexp.MustCompile(`\$\{?[A-Za-z0-9_]+\}?`)

var errNoUsableRules = errors.New("no usable rate limit rules")

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
  "dependencies": {
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

type counter struct {
	used  int64
	reset time.Time
}

type responseRecorder struct {
	header      http.Header
	body        bytes.Buffer
	statusCode  int
	wroteHeader bool
}

func WithPickedAIInstanceName(r *http.Request, name string) *http.Request {
	return ai_runtime.WithSelectedInstanceName(r, name)
}

func PickedAIInstanceName(r *http.Request) (string, bool) {
	return ai_runtime.SelectedInstanceName(r)
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	return nil
}

func (p *Plugin) PostInit() error {
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

	if p.counters == nil {
		p.counters = map[string]*counter{}
	}
	if p.now == nil {
		p.now = time.Now
	}
	return nil
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		var quotas []quota
		for {
			var ok bool
			var err error
			quotas, ok, err = p.quotasForRequest(r)
			if err != nil {
				http.Error(w, "failed to get rate limit rules", http.StatusInternalServerError)
				return
			}
			if !ok {
				next.ServeHTTP(w, r)
				return
			}

			rejectedIndex := p.firstRejectedQuota(quotas)
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
			return
		}
		if state := ai_runtime.FromRequest(r); state != nil && state.Streaming() {
			for _, q := range quotas {
				p.writeQuotaHeaders(w.Header(), q)
			}
			next.ServeHTTP(w, r)
			if len(p.config.Rules) == 0 {
				if finalQuotas, ok, err := p.quotasForRequest(r); err == nil && ok {
					quotas = finalQuotas
				} else if err == nil {
					quotas = nil
				}
			}
			if usedTokens := p.responseTokenCostForRequest(r, nil); usedTokens > 0 {
				for _, q := range quotas {
					p.charge(q, usedTokens)
				}
			}
			return
		}

		recorder := newResponseRecorder()
		next.ServeHTTP(recorder, r)
		if len(p.config.Rules) == 0 {
			if finalQuotas, ok, err := p.quotasForRequest(r); err == nil && ok {
				quotas = finalQuotas
			} else if err == nil {
				quotas = nil
			}
		}
		usedTokens := p.responseTokenCostForRequest(r, recorder.body.Bytes())
		if usedTokens > 0 {
			for _, q := range quotas {
				p.charge(q, usedTokens)
			}
		}
		for _, q := range quotas {
			p.writeQuotaHeaders(recorder.header, q)
		}
		recorder.writeTo(w)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) firstRejectedQuota(quotas []quota) int {
	for i, q := range quotas {
		if !p.allowed(q) {
			return i
		}
	}
	return -1
}

func (p *Plugin) quotasForRequest(r *http.Request) ([]quota, bool, error) {
	if len(p.config.Rules) > 0 {
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
				key:          "rule:" + strconv.Itoa(i) + ":" + key,
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
					key:        "instance:" + instance.Name,
					headerName: instance.Name,
					limit:      limit,
					window:     windowDuration,
				}, true, nil
			}
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
			key:        "global",
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
		key:        "global",
		headerName: "global",
		limit:      limit,
		window:     windowDuration,
	}, true, nil
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
		value = variablePattern.ReplaceAllStringFunc(text, func(match string) string {
			variableName := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(match, "${"), "$"), "}")
			return requestVariable(r, variableName)
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
	if strings.HasPrefix(key, "http_") {
		return r.Header.Get(strings.ReplaceAll(strings.TrimPrefix(key, "http_"), "_", "-"))
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

func (p *Plugin) allowed(q quota) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	state := p.state(q)
	return state.used < q.limit
}

func (p *Plugin) charge(q quota, tokens int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	state := p.state(q)
	state.used += tokens
}

func (p *Plugin) state(q quota) *counter {
	now := p.now()
	state, ok := p.counters[q.key]
	if !ok || !now.Before(state.reset) {
		state = &counter{reset: now.Add(q.window)}
		p.counters[q.key] = state
	}
	return state
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

	value := numericUsage(decoded.Usage[p.config.LimitStrategy])
	if value < 0 {
		return 0
	}
	return value
}

func (p *Plugin) responseTokenCostForRequest(r *http.Request, body []byte) int64 {
	if p.config.LimitStrategy == "expression" {
		if rawUsage, ok := apisixctx.GetRequestVar(r, "$llm_raw_usage").(map[string]any); ok {
			return p.expressionCost(rawUsage)
		}
	}
	if usage, ok := apisixctx.GetRequestVar(r, "$ai_token_usage").(map[string]any); ok {
		if value := numericUsage(usage[p.config.LimitStrategy]); value > 0 {
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

func numericUsage(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(math.Round(typed))
	case int64:
		return typed
	case int:
		return int64(typed)
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
	if p.config.ShowLimitQuotaHeader != nil && !*p.config.ShowLimitQuotaHeader {
		return
	}

	used, reset := p.snapshot(q)
	remaining := q.limit - used
	if remaining < 0 {
		remaining = 0
	}
	if q.headerPrefix != "" {
		header.Set("X-AI-"+q.headerPrefix+"-RateLimit-Limit", strconv.FormatInt(q.limit, 10))
		header.Set("X-AI-"+q.headerPrefix+"-RateLimit-Remaining", strconv.FormatInt(remaining, 10))
		header.Set("X-AI-"+q.headerPrefix+"-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		return
	}
	header.Set("X-AI-RateLimit-Limit-"+q.headerName, strconv.FormatInt(q.limit, 10))
	header.Set("X-AI-RateLimit-Remaining-"+q.headerName, strconv.FormatInt(remaining, 10))
	header.Set("X-AI-RateLimit-Reset-"+q.headerName, strconv.FormatInt(reset.Unix(), 10))
}

func (p *Plugin) snapshot(q quota) (int64, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()

	state := p.state(q)
	return state.used, state.reset
}

func (p *Plugin) reject(w http.ResponseWriter) {
	if p.config.RejectedMsg == "" {
		http.Error(w, http.StatusText(p.config.RejectedCode), p.config.RejectedCode)
		return
	}
	http.Error(w, p.config.RejectedMsg, p.config.RejectedCode)
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{
		header:     make(http.Header),
		statusCode: http.StatusOK,
	}
}

func (r *responseRecorder) Header() http.Header {
	return r.header
}

func (r *responseRecorder) WriteHeader(statusCode int) {
	if r.wroteHeader {
		return
	}
	r.statusCode = statusCode
	r.wroteHeader = true
}

func (r *responseRecorder) Write(body []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.body.Write(body)
}

func (r *responseRecorder) writeTo(w http.ResponseWriter) {
	for field, values := range r.header {
		for _, value := range values {
			w.Header().Add(field, value)
		}
	}
	w.WriteHeader(r.statusCode)
	_, _ = w.Write(r.body.Bytes())
}
