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
	pluginexpr "github.com/wklken/apisix-go/pkg/plugin/expr"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/store"
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
            "type": "array",
            "items": {
              "type": "object",
              "properties": {
                "vars": {"type": "array"}
              }
            }
          },
          "weighted_upstreams": {
            "type": "array",
            "minItems": 1,
            "maxItems": 20,
            "items": {
              "type": "object",
              "properties": {
                "upstream_id": {
                  "anyOf": [
                    {
                      "type": "string",
                      "minLength": 1,
                      "maxLength": 64,
                      "pattern": "^[a-zA-Z0-9-_.]+$"
                    },
                    {"type": "integer", "minimum": 1}
                  ]
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
	weightSet  bool
}

type Upstream struct {
	Type   string `json:"type,omitempty"`
	Scheme string `json:"scheme,omitempty"`
	Nodes  []Node `json:"nodes,omitempty"`
}

type Node struct {
	Host      string `json:"host,omitempty"`
	Port      int    `json:"port,omitempty"`
	Weight    int    `json:"weight,omitempty"`
	weightSet bool
}

type Override struct {
	Scheme string
	Host   string
}

type compiledRule struct {
	exprs     []*pluginexpr.Expression
	balancer  pxy.LoadBalancer
	overrides map[string]*Override
	err       error
}

type overrideKey struct{}

const routeUpstreamKey = "plugin#upstream#is#empty"

type upstreamResolver func(id string) (*Upstream, error)

var getUpstreamByID upstreamResolver = loadUpstreamByID

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
			Host:      host,
			Port:      port,
			Weight:    weight,
			weightSet: true,
		})
	}
	return nil
}

func (w *WeightedUpstream) UnmarshalJSON(data []byte) error {
	var raw struct {
		UpstreamID json.RawMessage `json:"upstream_id"`
		Upstream   *Upstream       `json:"upstream"`
		Weight     *int            `json:"weight"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	w.Upstream = raw.Upstream
	if raw.Weight != nil {
		w.Weight = *raw.Weight
		w.weightSet = true
	}
	if len(raw.UpstreamID) == 0 || string(raw.UpstreamID) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw.UpstreamID, &w.UpstreamID); err == nil {
		return nil
	}
	var numericID int64
	if err := json.Unmarshal(raw.UpstreamID, &numericID); err != nil || numericID < 1 {
		return fmt.Errorf("traffic-split upstream_id must be a string or positive integer")
	}
	w.UpstreamID = strconv.FormatInt(numericID, 10)
	return nil
}

func (n *Node) UnmarshalJSON(data []byte) error {
	var raw struct {
		Host   string `json:"host"`
		Port   int    `json:"port"`
		Weight *int   `json:"weight"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	n.Host = raw.Host
	n.Port = raw.Port
	if raw.Weight != nil {
		n.Weight = *raw.Weight
		n.weightSet = true
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
	for ruleIndex, rule := range p.config.Rules {
		servers := map[string]int{}
		overrides := map[string]*Override{}
		var compileErr error
		exprs := make([]*pluginexpr.Expression, 0, len(rule.Match))
		for matchIndex, match := range rule.Match {
			expr, err := pluginexpr.Compile(match.Vars)
			if err != nil {
				return fmt.Errorf(
					"traffic-split rule %d match %d vars validation failed: %w",
					ruleIndex,
					matchIndex,
					err,
				)
			}
			exprs = append(exprs, expr)
		}
		for _, weightedUpstream := range rule.WeightedUpstreams {
			weight := configuredWeight(weightedUpstream.Weight, weightedUpstream.weightSet)
			upstream := weightedUpstream.Upstream
			if upstream == nil && weightedUpstream.UpstreamID != "" {
				var err error
				upstream, err = getUpstreamByID(weightedUpstream.UpstreamID)
				if err != nil {
					compileErr = fmt.Errorf(
						"failed to fetch upstream info by upstream id: %s",
						weightedUpstream.UpstreamID,
					)
					continue
				}
			}
			if upstream == nil {
				if weightedUpstream.UpstreamID == "" && weight > 0 {
					servers[routeUpstreamKey] += weight
					overrides[routeUpstreamKey] = nil
				}
				continue
			}
			if weight == 0 {
				continue
			}
			for _, node := range upstream.Nodes {
				override := overrideFromNode(upstream.Scheme, node)
				key := override.key()
				nodeWeight := configuredWeight(node.Weight, node.weightSet)
				if nodeWeight == 0 {
					continue
				}
				servers[key] += weight * nodeWeight
				overrides[key] = override
			}
		}

		compiled := compiledRule{
			exprs:     exprs,
			overrides: overrides,
			err:       compileErr,
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
		override, err := p.nextOverride(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if override == nil {
			next.ServeHTTP(w, r)
			return
		}

		next.ServeHTTP(w, WithOverride(r, override))
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) nextOverride(r *http.Request) (*Override, error) {
	for _, rule := range p.rules {
		if !matchRule(r, rule.exprs) {
			continue
		}
		if rule.err != nil {
			return nil, rule.err
		}
		if rule.balancer == nil {
			return nil, nil
		}
		key := rule.balancer.Next()
		return rule.overrides[key], nil
	}
	return nil, nil
}

func configuredWeight(weight int, configured bool) int {
	if weight == 0 && !configured {
		return 1
	}
	return weight
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

func loadUpstreamByID(id string) (upstream *Upstream, err error) {
	defer func() {
		if recover() != nil {
			upstream = nil
			err = store.ErrNotFound
		}
	}()

	stored, err := store.GetUpstream(id)
	if err != nil {
		return nil, err
	}
	return upstreamFromResource(stored), nil
}

func upstreamFromResource(stored resource.Upstream) *Upstream {
	upstream := &Upstream{
		Type:   stored.Type,
		Scheme: stored.Scheme,
		Nodes:  make([]Node, 0, len(stored.Nodes)),
	}
	for _, node := range stored.Nodes {
		upstream.Nodes = append(upstream.Nodes, Node{
			Host:   node.Host,
			Port:   node.Port,
			Weight: node.Weight,
		})
	}
	return upstream
}

func matchRule(r *http.Request, exprs []*pluginexpr.Expression) bool {
	if len(exprs) == 0 {
		return true
	}
	for _, expr := range exprs {
		if expr.Eval(func(name string) any {
			return pluginexpr.RequestValue(r, name)
		}) {
			return true
		}
	}
	return false
}
