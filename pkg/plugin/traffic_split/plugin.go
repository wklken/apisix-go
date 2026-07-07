package traffic_split

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	pxy "github.com/wklken/apisix-go/pkg/proxy"

	"github.com/wklken/apisix-go/pkg/plugin/base"
)

type Plugin struct {
	base.BasePlugin
	config Config
	rules  []compiledRule
}

const (
	priority = 966
	name     = "traffic-split"
)

const schema = `
{
  "type": "object",
  "properties": {
    "rules": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "match": {
            "type": "array"
          },
          "weighted_upstreams": {
            "type": "array",
            "minItems": 1,
            "maxItems": 20,
            "items": {
              "type": "object",
              "properties": {
                "upstream_id": {
                  "type": "string"
                },
                "upstream": {
                  "type": "object"
                },
                "weight": {
                  "type": "integer",
                  "default": 1,
                  "minimum": 0
                }
              }
            }
          }
        }
      }
    }
  }
}
`

type Config struct {
	Rules []Rule `json:"rules,omitempty"`
}

type Rule struct {
	Match             []Match            `json:"match,omitempty"`
	WeightedUpstreams []WeightedUpstream `json:"weighted_upstreams,omitempty"`
}

type Match struct {
	Vars []any `json:"vars,omitempty"`
}

type WeightedUpstream struct {
	UpstreamID string    `json:"upstream_id,omitempty"`
	Upstream   *Upstream `json:"upstream,omitempty"`
	Weight     int       `json:"weight,omitempty"`
}

type Upstream struct {
	Type   string `json:"type,omitempty"`
	Scheme string `json:"scheme,omitempty"`
	Nodes  []Node `json:"nodes,omitempty"`
}

type Node struct {
	Host   string `json:"host,omitempty"`
	Port   int    `json:"port,omitempty"`
	Weight int    `json:"weight,omitempty"`
}

type Override struct {
	Scheme string
	Host   string
}

type compiledRule struct {
	match     []Match
	balancer  pxy.LoadBalancer
	overrides map[string]*Override
}

type overrideKey struct{}

func WithOverride(r *http.Request, override *Override) *http.Request {
	if override == nil {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), overrideKey{}, override))
}

func GetOverride(r *http.Request) *Override {
	override, _ := r.Context().Value(overrideKey{}).(*Override)
	return override
}

func (u *Upstream) UnmarshalJSON(data []byte) error {
	type upstreamAlias Upstream
	var raw struct {
		upstreamAlias
		Nodes json.RawMessage `json:"nodes"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*u = Upstream(raw.upstreamAlias)

	if len(raw.Nodes) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw.Nodes, &u.Nodes); err == nil {
		return nil
	}

	var nodeMap map[string]int
	if err := json.Unmarshal(raw.Nodes, &nodeMap); err != nil {
		return err
	}
	for addr, weight := range nodeMap {
		host, port := splitAddr(addr)
		u.Nodes = append(u.Nodes, Node{
			Host:   host,
			Port:   port,
			Weight: weight,
		})
	}
	return nil
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	p.rules = p.rules[:0]
	for _, rule := range p.config.Rules {
		servers := map[string]int{}
		overrides := map[string]*Override{}
		for _, weightedUpstream := range rule.WeightedUpstreams {
			weight := weightedUpstream.Weight
			if weight == 0 {
				weight = 1
			}
			if weightedUpstream.Upstream == nil {
				continue
			}
			for _, node := range weightedUpstream.Upstream.Nodes {
				override := overrideFromNode(weightedUpstream.Upstream.Scheme, node)
				key := override.key()
				nodeWeight := node.Weight
				if nodeWeight == 0 {
					nodeWeight = 1
				}
				servers[key] += weight * nodeWeight
				overrides[key] = override
			}
		}

		compiled := compiledRule{
			match:     rule.Match,
			overrides: overrides,
		}
		if len(servers) > 0 {
			compiled.balancer = pxy.NewWeightedRRLoadBalance(servers)
		}
		p.rules = append(p.rules, compiled)
	}

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		override := p.nextOverride(r)
		if override == nil {
			next.ServeHTTP(w, r)
			return
		}

		next.ServeHTTP(w, WithOverride(r, override))
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) nextOverride(r *http.Request) *Override {
	for _, rule := range p.rules {
		if !matchRule(r, rule.match) {
			continue
		}
		if rule.balancer == nil {
			return nil
		}
		key := rule.balancer.Next()
		return rule.overrides[key]
	}
	return nil
}

func overrideFromNode(scheme string, node Node) *Override {
	if scheme == "" {
		scheme = "http"
	}
	return &Override{
		Scheme: scheme,
		Host:   joinHostPort(scheme, node),
	}
}

func (o *Override) key() string {
	return o.Scheme + "://" + o.Host
}

func joinHostPort(scheme string, node Node) string {
	if node.Port == 0 {
		if _, _, err := net.SplitHostPort(node.Host); err == nil {
			return node.Host
		}
		if scheme == "https" {
			node.Port = 443
		} else {
			node.Port = 80
		}
	}
	return fmt.Sprintf("%s:%d", node.Host, node.Port)
}

func splitAddr(addr string) (string, int) {
	host, portValue, err := net.SplitHostPort(addr)
	if err == nil {
		port, _ := strconv.Atoi(portValue)
		return host, port
	}

	if strings.Count(addr, ":") == 1 {
		parts := strings.Split(addr, ":")
		port, _ := strconv.Atoi(parts[1])
		return parts[0], port
	}

	return addr, 0
}

func matchRule(r *http.Request, matches []Match) bool {
	if len(matches) == 0 {
		return true
	}
	for _, match := range matches {
		if matchVars(r, match.Vars) {
			return true
		}
	}
	return false
}

func matchVars(r *http.Request, conditions []any) bool {
	if len(conditions) == 0 {
		return false
	}

	pendingOp := "AND"
	hasResult := false
	result := true
	for _, condition := range conditions {
		if op, ok := condition.(string); ok {
			switch strings.ToUpper(op) {
			case "AND", "OR":
				pendingOp = strings.ToUpper(op)
			default:
				return false
			}
			continue
		}

		matched := matchCondition(r, condition)
		if !hasResult {
			result = matched
			hasResult = true
			continue
		}
		if pendingOp == "OR" {
			result = result || matched
		} else {
			result = result && matched
		}
		pendingOp = "AND"
	}
	return hasResult && result
}

func matchCondition(r *http.Request, condition any) bool {
	parts, ok := condition.([]any)
	if !ok || len(parts) != 3 {
		return false
	}

	left := fmt.Sprint(parts[0])
	op := fmt.Sprint(parts[1])
	right := fmt.Sprint(parts[2])
	actual := requestVar(r, left)

	switch op {
	case "==":
		return actual == right
	case "!=":
		return actual != right
	default:
		return false
	}
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
