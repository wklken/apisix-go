package api_breaker

import (
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/sony/gobreaker"
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
	cb     *gobreaker.CircuitBreaker
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

	cb := gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        "api-breaker",
		MaxRequests: uint32(*p.config.Healthy.Successes),
		Interval:    time.Duration(p.config.MaxBreakerSec) * time.Second,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.TotalFailures >= uint32(*p.config.Unhealthy.Failures)
		},
		OnStateChange: func(name string, from gobreaker.State, to gobreaker.State) {
			log.Printf("circuit breaker %s state change: %s -> %s\n", name, from, to)
		},
		IsSuccessful: func(err error) bool {
			return true
		},
	})
	p.cb = cb

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)

		status := ww.Status()

		// stats the status code
		_, err := p.cb.Execute(func() (interface{}, error) {
			for _, s := range p.config.Unhealthy.HTTPStatuses {
				if status == s {
					return nil, gobreaker.ErrOpenState
				}
			}
			// for _, s := range p.config.Healthy.HTTPStatuses {
			// 	if status == s {
			// 		return nil, nil
			// 	}
			// }
			return nil, nil
		})
		if err != nil {
			// FIXME: reset the response?

			if p.config.BreakResponseBody != nil {
				w.Write([]byte(*p.config.BreakResponseBody))
			}
			if p.config.BreakResponseHeaders != nil {
				for _, h := range p.config.BreakResponseHeaders {
					w.Header().Set(h.Key, h.Value)
				}
			}
			w.WriteHeader(p.config.BreakResponseCode)
			return
		}
	}
	return http.HandlerFunc(fn)
}
