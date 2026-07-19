package api_breaker

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config

	// APISIX shares breaker counters by host and URI; this Go implementation keeps route-local state.
	mu                sync.Mutex
	unhealthyCount    int
	healthyCount      int
	lastUnhealthyTime time.Time
	now               func() time.Time
}

const (
	// version  = "0.1"
	priority = 1005
	name     = "api-breaker"
)

const schema = `
{
	"type": "object",
	"properties": {
	  "break_response_code": {
		"type": "integer",
		"minimum": 200,
		"maximum": 599
	  },
	  "break_response_body": {
		"type": "string"
	  },
	  "break_response_headers": {
		"type": "array",
		"items": {
		  "type": "object",
		  "properties": {
			"key": {
			  "type": "string",
			  "minLength": 1
			},
			"value": {
			  "type": "string",
			  "minLength": 1
			}
		  },
		  "required": ["key", "value"]
		}
	  },
	  "max_breaker_sec": {
		"type": "integer",
		"minimum": 3,
		"default": 300
	  },
	  "unhealthy": {
		"type": "object",
		"properties": {
		  "http_statuses": {
			"type": "array",
			"minItems": 1,
			"items": {
			  "type": "integer",
			  "minimum": 500,
			  "maximum": 599
			},
			"uniqueItems": true,
			"default": [500]
		  },
		  "failures": {
			"type": "integer",
			"minimum": 1,
			"default": 3
		  }
		},
		"default": {
		  "http_statuses": [500],
		  "failures": 3
		}
	  },
	  "healthy": {
		"type": "object",
		"properties": {
		  "http_statuses": {
			"type": "array",
			"minItems": 1,
			"items": {
			  "type": "integer",
			  "minimum": 200,
			  "maximum": 499
			},
			"uniqueItems": true,
			"default": [200]
		  },
		  "successes": {
			"type": "integer",
			"minimum": 1,
			"default": 3
		  }
		},
		"default": {
		  "http_statuses": [200],
		  "successes": 3
		}
	  }
	},
	"required": ["break_response_code"]
}`

type Header struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type UnHealthCheck struct {
	HTTPStatuses []int `json:"http_statuses"`
	Failures     *int  `json:"failures,omitempty"`
}

type HealthCheck struct {
	HTTPStatuses []int `json:"http_statuses"`
	Successes    *int  `json:"successes,omitempty"`
}

type Config struct {
	BreakResponseCode    int           `json:"break_response_code"`
	BreakResponseBody    *string       `json:"break_response_body,omitempty"`
	BreakResponseHeaders []Header      `json:"break_response_headers,omitempty"`
	MaxBreakerSec        int           `json:"max_breaker_sec"`
	Unhealthy            UnHealthCheck `json:"unhealthy"`
	Healthy              HealthCheck   `json:"healthy"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	if p.now == nil {
		p.now = time.Now
	}
	if p.config.MaxBreakerSec == 0 {
		p.config.MaxBreakerSec = 300
	}
	if p.config.Unhealthy.HTTPStatuses == nil {
		p.config.Unhealthy.HTTPStatuses = []int{500}
	}
	if p.config.Unhealthy.Failures == nil {
		defaultFailures := 3
		p.config.Unhealthy.Failures = &defaultFailures
	}

	if p.config.Healthy.HTTPStatuses == nil {
		p.config.Healthy.HTTPStatuses = []int{200}
	}
	if p.config.Healthy.Successes == nil {
		defaultSuccesses := 3
		p.config.Healthy.Successes = &defaultSuccesses
	}

	return nil
}

var variablePattern = regexp.MustCompile(`\$[A-Za-z0-9_]+`)

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if p.shouldBreak() {
			if p.config.BreakResponseBody != nil && p.config.BreakResponseHeaders != nil {
				for _, header := range p.config.BreakResponseHeaders {
					w.Header().Set(header.Key, resolveHeaderValue(r, header.Value))
				}
			}
			w.WriteHeader(p.config.BreakResponseCode)
			if p.config.BreakResponseBody != nil {
				_, _ = w.Write([]byte(*p.config.BreakResponseBody))
			}
			return
		}

		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)
		p.observeStatus(ww.Status())
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) shouldBreak() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unhealthyCount == 0 || p.lastUnhealthyTime.IsZero() {
		return false
	}
	seconds := breakerSeconds(p.unhealthyCount, *p.config.Unhealthy.Failures, p.config.MaxBreakerSec)
	logger.Info(fmt.Sprintf("breaker_time: %d", seconds))
	return !p.now().After(p.lastUnhealthyTime.Add(time.Duration(seconds) * time.Second))
}

func (p *Plugin) observeStatus(status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case containsStatus(p.config.Unhealthy.HTTPStatuses, status):
		p.unhealthyCount++
		p.healthyCount = 0
		if p.unhealthyCount%*p.config.Unhealthy.Failures == 0 {
			p.lastUnhealthyTime = p.now()
		}
	case containsStatus(p.config.Healthy.HTTPStatuses, status):
		if p.unhealthyCount == 0 {
			return
		}
		p.healthyCount++
		if p.healthyCount >= *p.config.Healthy.Successes {
			p.unhealthyCount = 0
			p.healthyCount = 0
			p.lastUnhealthyTime = time.Time{}
		}
	}
}

func breakerSeconds(unhealthyCount, failures, maximum int) int {
	failureTimes := max(unhealthyCount/failures, 1)
	seconds := 2
	for range failureTimes - 1 {
		if seconds >= maximum || seconds > maximum/2 {
			return maximum
		}
		seconds *= 2
	}
	if seconds > maximum {
		return maximum
	}
	return seconds
}

func containsStatus(statuses []int, status int) bool {
	return slices.Contains(statuses, status)
}

func resolveHeaderValue(r *http.Request, value string) string {
	return variablePattern.ReplaceAllStringFunc(value, func(variable string) string {
		return base.RequestVar(r, strings.TrimPrefix(variable, "$"), 0)
	})
}
