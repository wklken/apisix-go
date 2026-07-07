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
	config    Config
	balancer  pxy.LoadBalancer
	overrides map[string]*Override
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
	Match             []any              `json:"match,omitempty"`
	WeightedUpstreams []WeightedUpstream `json:"weighted_upstreams,omitempty"`
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
	servers := map[string]int{}
	p.overrides = map[string]*Override{}

	for _, rule := range p.config.Rules {
		if len(rule.Match) > 0 {
			continue
		}
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
				p.overrides[key] = override
			}
		}
		break
	}

	if len(servers) > 0 {
		p.balancer = pxy.NewWeightedRRLoadBalance(servers)
	}

	return nil
}

func (p *Plugin) Config() interface{} {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		override := p.nextOverride()
		if override == nil {
			next.ServeHTTP(w, r)
			return
		}

		next.ServeHTTP(w, WithOverride(r, override))
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) nextOverride() *Override {
	if p.balancer == nil {
		return nil
	}
	key := p.balancer.Next()
	return p.overrides[key]
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
