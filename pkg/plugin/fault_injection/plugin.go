package fault_injection

import (
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
}

var sleep = time.Sleep

const (
	// version  = "0.1"
	priority = 11000
	name     = "fault-injection"
)

const schema = `
{
	"type": "object",
	"properties": {
	  "abort": {
		"type": "object",
		"properties": {
		  "http_status": {
			"type": "integer",
			"minimum": 200
		  },
		  "body": {
			"type": "string",
			"minLength": 0
		  },
		  "headers": {
			"type": "object",
			"minProperties": 1,
			"patternProperties": {
			  "^[^:]+$": {
				"oneOf": [
				  {
					"type": "string"
				  },
				  {
					"type": "number"
				  }
				]
			  }
			}
		  },
		  "percentage": {
			"type": "integer",
			"minimum": 0,
			"maximum": 100
		  },
		  "vars": {
			"type": "array",
			"maxItems": 20,
			"items": {
			  "type": "array"
			}
		  }
		},
		"required": ["http_status"]
	  },
	  "delay": {
		"type": "object",
		"properties": {
		  "duration": {
			"type": "number",
			"minimum": 0
		  },
		  "percentage": {
			"type": "integer",
			"minimum": 0,
			"maximum": 100
		  },
		  "vars": {
			"type": "array",
			"maxItems": 20,
			"items": {
			  "type": "array"
			}
		  }
		},
		"required": ["duration"]
	  }
	},
	"minProperties": 1
}`

type Abort struct {
	HTTPStatus int                    `json:"http_status"`
	Body       *string                `json:"body,omitempty"`
	Percentage *int                   `json:"percentage,omitempty"`
	Headers    map[string]interface{} `json:"headers,omitempty"` // Note: interface{} due to oneOf {string, number}

	// FIXME: not support yet
	Vars [][]interface{} `json:"vars,omitempty"`
}

type Delay struct {
	Duration   float64 `json:"duration"`
	Percentage *int    `json:"percentage,omitempty"`

	// FIXME: not support yet
	Vars [][]interface{} `json:"vars,omitempty"`
}

type Config struct {
	Abort *Abort `json:"abort"`
	Delay *Delay `json:"delay"`
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		if p.config.Delay != nil {
			if p.config.Delay.Duration > 0 && SampleHit(p.config.Delay.Percentage) &&
				varsMatch(r, p.config.Delay.Vars) {
				// sleep
				sleep(time.Duration(p.config.Delay.Duration * float64(time.Second)))
			}
		}

		if p.config.Abort != nil {
			if SampleHit(p.config.Abort.Percentage) && varsMatch(r, p.config.Abort.Vars) {
				if p.config.Abort.Headers != nil {
					for k, v := range p.config.Abort.Headers {
						value := fmt.Sprintf("%s", v)
						if stringValue, ok := v.(string); ok {
							value = resolveValue(r, stringValue)
						}
						w.Header().Set(k, value)
					}
				}

				w.WriteHeader(p.config.Abort.HTTPStatus)
				if p.config.Abort.Body != nil {
					_, _ = w.Write([]byte(resolveValue(r, *p.config.Abort.Body)))
				}
				return
			}
		}

		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func SampleHit(percentage *int) bool {
	if percentage == nil {
		return true
	}

	return rand.Intn(100) < *percentage
}

func varsMatch(r *http.Request, vars [][]interface{}) bool {
	if len(vars) == 0 {
		return true
	}
	for _, expr := range vars {
		if matchExpr(r, expr) {
			return true
		}
	}
	return false
}

func matchExpr(r *http.Request, expr []interface{}) bool {
	if len(expr) != 3 {
		return false
	}

	left := fmt.Sprint(expr[0])
	op := fmt.Sprint(expr[1])
	right := fmt.Sprint(expr[2])
	actual := requestVar(r, left)

	switch op {
	case "==":
		return actual == right
	case "!=":
		return actual != right
	case ">":
		return compareNumber(actual, right, func(a, b float64) bool { return a > b })
	case ">=":
		return compareNumber(actual, right, func(a, b float64) bool { return a >= b })
	case "<":
		return compareNumber(actual, right, func(a, b float64) bool { return a < b })
	case "<=":
		return compareNumber(actual, right, func(a, b float64) bool { return a <= b })
	case "~":
		matched, _ := regexp.MatchString(right, actual)
		return matched
	case "!~":
		matched, _ := regexp.MatchString(right, actual)
		return !matched
	default:
		return false
	}
}

func compareNumber(left string, right string, compare func(float64, float64) bool) bool {
	l, err := strconv.ParseFloat(left, 64)
	if err != nil {
		return false
	}
	r, err := strconv.ParseFloat(right, 64)
	if err != nil {
		return false
	}
	return compare(l, r)
}

var variablePattern = regexp.MustCompile(`\$[A-Za-z0-9_]+`)

func resolveValue(r *http.Request, value string) string {
	return variablePattern.ReplaceAllStringFunc(value, func(variable string) string {
		return requestVar(r, strings.TrimPrefix(variable, "$"))
	})
}

func requestVar(r *http.Request, name string) string {
	name = strings.TrimPrefix(name, "$")
	switch {
	case name == "uri":
		return r.URL.Path
	case name == "request_uri":
		return r.URL.RequestURI()
	case name == "method", name == "request_method":
		return r.Method
	case name == "host":
		return r.Host
	case name == "scheme":
		if scheme := r.Header.Get("X-Forwarded-Proto"); scheme != "" {
			return scheme
		}
		if r.TLS != nil {
			return "https"
		}
		return "http"
	case name == "remote_addr":
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			return host
		}
		return r.RemoteAddr
	case strings.HasPrefix(name, "arg_"):
		return r.URL.Query().Get(strings.TrimPrefix(name, "arg_"))
	case strings.HasPrefix(name, "http_"):
		header := strings.ReplaceAll(strings.TrimPrefix(name, "http_"), "_", "-")
		return r.Header.Get(header)
	default:
		return ""
	}
}
