package traffic_split

import (
	"context"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	pxy "github.com/wklken/apisix-go/pkg/proxy"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/chash"
	pluginexpr "github.com/wklken/apisix-go/pkg/plugin/expr"
	"github.com/wklken/apisix-go/pkg/resource"
)

type Plugin struct {
	base.BasePlugin
	config             Config
	rules              []compiledRule
	runtimeAcquirer    RuntimeAcquirer
	runtimeAcquirerSet bool
	upstreamResolver   ResourceUpstreamResolver
}

const (
	priority = 966
	name     = "traffic-split"
)

var hashVariablePattern = regexp.MustCompile(`\$\{([^}]*)\}|\$([A-Za-z0-9_.]+)`)

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
                  "type": "object",
                  "properties": {
                    "id": {
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
                    "name": {"type": "string", "minLength": 1, "maxLength": 256},
                    "desc": {"type": "string", "maxLength": 256},
                    "labels": {
                      "type": "object",
                      "additionalProperties": {
                        "type": "string",
                        "pattern": "^\\S+$",
                        "minLength": 1,
                        "maxLength": 256
                      }
                    },
                    "create_time": {"type": "integer"},
                    "update_time": {"type": "integer"},
                    "type": {"type": "string", "default": "roundrobin"},
                    "scheme": {
                      "type": "string",
                      "enum": ["grpc", "grpcs", "http", "https", "tcp", "tls", "udp", "kafka"],
                      "default": "http"
                    },
                    "tls": {
                      "type": "object",
                      "properties": {
                        "client_cert_id": {
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
                        "client_cert": {"type": "string", "minLength": 128, "maxLength": 65536},
                        "client_key": {"type": "string", "minLength": 64, "maxLength": 65536},
                        "verify": {"type": "boolean", "default": false}
                      },
                      "dependencies": {
                        "client_cert": {"required": ["client_key"]},
                        "client_key": {"required": ["client_cert"]},
                        "client_cert_id": {"not": {"required": ["client_cert", "client_key"]}}
                      }
                    },
                    "keepalive_pool": {
                      "type": "object",
                      "properties": {
                        "size": {"type": "integer", "minimum": 1, "default": 320},
                        "idle_timeout": {"type": "number", "minimum": 0, "default": 60},
                        "requests": {"type": "integer", "minimum": 1, "default": 1000}
                      }
                    },
                    "pass_host": {
                      "type": "string",
                      "enum": ["pass", "node", "rewrite"],
                      "default": "pass"
                    },
                    "upstream_host": {
                      "type": "string",
                      "pattern": "^\\*$|^\\*?[0-9a-zA-Z-._\\[\\]:]+$"
                    },
                    "hash_on": {
                      "type": "string",
                      "enum": ["vars", "header", "cookie", "consumer", "vars_combinations"],
                      "default": "vars"
                    },
                    "key": {"type": "string"},
                    "timeout": {
                      "type": "object",
                      "properties": {
                        "connect": {"type": "number", "exclusiveMinimum": 0},
                        "send": {"type": "number", "exclusiveMinimum": 0},
                        "read": {"type": "number", "exclusiveMinimum": 0}
                      },
                      "required": ["connect", "send", "read"]
                    },
                    "checks": {
                      "type": "object",
                      "properties": {
                        "active": {
                          "type": "object",
                          "properties": {
                            "type": {"type": "string", "enum": ["http", "https", "tcp"], "default": "http"},
                            "timeout": {"type": "number", "default": 1},
                            "concurrency": {"type": "integer", "default": 10},
                            "host": {
                              "type": "string",
                              "pattern": "^\\*$|^\\*?[0-9a-zA-Z-._\\[\\]:]+$"
                            },
                            "port": {"type": "integer", "minimum": 1, "maximum": 65535},
                            "http_path": {"type": "string", "default": "/"},
                            "https_verify_certificate": {"type": "boolean", "default": true},
                            "healthy": {
                              "type": "object",
                              "properties": {
                                "interval": {"type": "integer", "minimum": 1, "default": 1},
                                "http_statuses": {
                                  "type": "array",
                                  "minItems": 1,
                                  "items": {"type": "integer", "minimum": 200, "maximum": 599},
                                  "uniqueItems": true,
                                  "default": [200, 302]
                                },
                                "successes": {"type": "integer", "minimum": 1, "maximum": 254, "default": 2}
                              }
                            },
                            "unhealthy": {
                              "type": "object",
                              "properties": {
                                "interval": {"type": "integer", "minimum": 1, "default": 1},
                                "http_statuses": {
                                  "type": "array",
                                  "minItems": 1,
                                  "items": {"type": "integer", "minimum": 200, "maximum": 599},
                                  "uniqueItems": true,
                                  "default": [429, 404, 500, 501, 502, 503, 504, 505]
                                },
                                "http_failures": {"type": "integer", "minimum": 1, "maximum": 254, "default": 5},
                                "tcp_failures": {"type": "integer", "minimum": 1, "maximum": 254, "default": 2},
                                "timeouts": {"type": "integer", "minimum": 1, "maximum": 254, "default": 3}
                              }
                            },
                            "req_headers": {
                              "type": "array",
                              "minItems": 1,
                              "items": {"type": "string", "uniqueItems": true}
                            }
                          }
                        },
                        "passive": {
                          "type": "object",
                          "properties": {
                            "type": {"type": "string", "enum": ["http", "https", "tcp"], "default": "http"},
                            "healthy": {
                              "type": "object",
                              "properties": {
                                "http_statuses": {
                                  "type": "array",
                                  "minItems": 1,
                                  "items": {"type": "integer", "minimum": 200, "maximum": 599},
                                  "uniqueItems": true,
                                  "default": [200, 201, 202, 203, 204, 205, 206, 207, 208, 226, 300, 301, 302, 303, 304, 305, 306, 307, 308]
                                },
                                "successes": {"type": "integer", "minimum": 0, "maximum": 254, "default": 5}
                              }
                            },
                            "unhealthy": {
                              "type": "object",
                              "properties": {
                                "http_statuses": {
                                  "type": "array",
                                  "minItems": 1,
                                  "items": {"type": "integer", "minimum": 200, "maximum": 599},
                                  "uniqueItems": true,
                                  "default": [429, 500, 503]
                                },
                                "tcp_failures": {"type": "integer", "minimum": 0, "maximum": 254, "default": 2},
                                "timeouts": {"type": "integer", "minimum": 0, "maximum": 254, "default": 7},
                                "http_failures": {"type": "integer", "minimum": 0, "maximum": 254, "default": 5}
                              }
                            }
                          }
                        }
                      },
                      "anyOf": [
                        {"required": ["active"]},
                        {"required": ["active", "passive"]}
                      ]
                    },
                    "retries": {"type": "integer", "minimum": 0},
                    "retry_timeout": {"type": "number", "minimum": 0},
                    "discovery_type": {"type": "string"},
                    "discovery_args": {
                      "type": "object",
                      "properties": {
                        "namespace_id": {"type": "string"},
                        "group_name": {"type": "string"}
                      }
                    },
                    "service_name": {"type": "string", "minLength": 1, "maxLength": 256},
                    "nodes": {
                      "anyOf": [
                        {
                          "type": "object",
                          "additionalProperties": {"type": "integer", "minimum": 0}
                        },
                        {
                          "type": "array",
                          "items": {
                            "type": "object",
                            "properties": {
                              "host": {
                                "type": "string",
                                "pattern": "^\\*$|^\\*?[0-9a-zA-Z-._\\[\\]:]+$"
                              },
                              "port": {"type": "integer", "minimum": 1, "maximum": 65535},
                              "weight": {"type": "integer", "minimum": 0},
                              "priority": {"type": "integer", "default": 0},
                              "metadata": {"type": "object"}
                            },
                            "required": ["host", "weight"]
                          }
                        }
                      ]
                    }
                  },
                  "oneOf": [
                    {"required": ["nodes"]},
                    {"required": ["service_name", "discovery_type"]}
                  ],
                  "additionalProperties": false
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
	Name         string                `json:"name,omitempty"`
	Type         string                `json:"type,omitempty"`
	Scheme       string                `json:"scheme,omitempty"`
	TLS          *resource.UpstreamTLS `json:"tls,omitempty"`
	PassHost     string                `json:"pass_host,omitempty"`
	UpstreamHost string                `json:"upstream_host,omitempty"`
	HashOn       string                `json:"hash_on,omitempty"`
	Key          string                `json:"key,omitempty"`
	Timeout      resource.Timeout      `json:"timeout"`
	Retries      int                   `json:"retries,omitempty"`
	retriesSet   bool
	Checks       map[string]any `json:"checks,omitempty"`
	Nodes        []Node         `json:"nodes,omitempty"`
}

type Node struct {
	Host      string `json:"host,omitempty"`
	Port      int    `json:"port,omitempty"`
	Weight    int    `json:"weight,omitempty"`
	Priority  int    `json:"priority,omitempty"`
	weightSet bool
}

type Override struct {
	Scheme         string
	Host           string
	PassHost       string
	UpstreamHost   string
	Timeout        resource.Timeout
	Retries        int
	NextRetry      func(*http.Request) *Override
	HealthReporter pxy.HealthReporter
	HealthTarget   string
	RoundTripper   http.RoundTripper
}

// Runtime contains the reusable cluster-owned state for one weighted
// upstream. The route builder materializes it once per route generation.
type Runtime struct {
	LoadBalancer pxy.LoadBalancer
	RoundTripper http.RoundTripper
}

// RuntimeAcquirer materializes a weighted upstream through the route-owned
// cluster registry. It accepts concrete target identities so active probes and
// passive health use the same keys as request selection.
type RuntimeAcquirer interface {
	Acquire(upstream *Upstream, targets map[string]int, priorities map[string]int) (*Runtime, error)
}

// ResourceUpstreamResolver resolves an upstream_id from the route builder's
// current Store generation before PostInit compiles the weighted targets.
type ResourceUpstreamResolver func(id string) (resource.Upstream, error)

type compiledRule struct {
	exprs    []*pluginexpr.Expression
	balancer pxy.LoadBalancer
	targets  map[string]compiledTarget
	err      error
}

type compiledTarget struct {
	fallback   bool
	balancer   pxy.LoadBalancer
	overrides  map[string]*Override
	priorities map[string]int
	hashOn     string
	key        string
	ring       *chash.Ring
	retryScan  int
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
		Retries *int            `json:"retries"`
		Nodes   json.RawMessage `json:"nodes"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*u = Upstream(raw.upstreamAlias)
	if raw.Retries != nil {
		u.Retries = *raw.Retries
		u.retriesSet = true
	}

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
	addresses := make([]string, 0, len(nodeMap))
	for addr := range nodeMap {
		addresses = append(addresses, addr)
	}
	sort.Strings(addresses)
	for _, addr := range addresses {
		weight := nodeMap[addr]
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
		Host     string `json:"host"`
		Port     int    `json:"port"`
		Weight   *int   `json:"weight"`
		Priority int    `json:"priority"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	n.Host = raw.Host
	n.Port = raw.Port
	n.Priority = raw.Priority
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

// SetRuntimeAcquirer supplies the route-owned materializer before PostInit.
func (p *Plugin) SetRuntimeAcquirer(acquirer RuntimeAcquirer) {
	p.runtimeAcquirer = acquirer
	p.runtimeAcquirerSet = true
}

// SetUpstreamResolver supplies the immutable generation's upstream resolver
// before PostInit.
func (p *Plugin) SetUpstreamResolver(resolver ResourceUpstreamResolver) {
	p.upstreamResolver = resolver
}

func (p *Plugin) PostInit() error {
	defer func() {
		p.runtimeAcquirer = nil
		p.runtimeAcquirerSet = false
		p.upstreamResolver = nil
	}()
	p.rules = p.rules[:0]
	for ruleIndex, rule := range p.config.Rules {
		targetWeights := map[string]int{}
		targets := map[string]compiledTarget{}
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
		for upstreamIndex, weightedUpstream := range rule.WeightedUpstreams {
			targetID := fmt.Sprintf("traffic-split-%d-%d", ruleIndex, upstreamIndex)
			weight := configuredWeight(weightedUpstream.Weight, weightedUpstream.weightSet)
			upstream := weightedUpstream.Upstream
			if upstream != nil {
				upstream = projectInlineUpstream(upstream)
			}
			if upstream == nil && weightedUpstream.UpstreamID != "" {
				var err error
				upstream, err = p.resolveUpstreamByID(weightedUpstream.UpstreamID)
				if err != nil {
					compileErr = fmt.Errorf(
						"failed to find upstream by id: %s",
						weightedUpstream.UpstreamID,
					)
					continue
				}
			}
			if upstream == nil {
				if weightedUpstream.UpstreamID == "" && weight > 0 {
					targetWeights[targetID] += weight
					targets[targetID] = compiledTarget{fallback: true}
				}
				continue
			}
			if err := validateUpstreamHostMode(upstream); err != nil {
				return fmt.Errorf("traffic-split rule %d upstream validation failed: %w", ruleIndex, err)
			}
			if err := validateUpstreamHash(upstream); err != nil {
				return fmt.Errorf("traffic-split rule %d upstream validation failed: %w", ruleIndex, err)
			}
			if weight == 0 {
				continue
			}
			nodeWeights := map[string]int{}
			nodePriorities := map[string]int{}
			nodeOverrides := map[string]*Override{}
			hashNodes := make([]chash.Node, 0, len(upstream.Nodes))
			retryScan := 0
			for _, node := range upstream.Nodes {
				override := overrideFromNode(upstream, node)
				nodeWeight := configuredWeight(node.Weight, node.weightSet)
				if nodeWeight == 0 {
					continue
				}
				nodeTarget := overrideTargetURL(override)
				nodeWeights[nodeTarget] = nodeWeight
				nodePriorities[nodeTarget] = node.Priority
				nodeOverrides[nodeTarget] = override
				hashNodes = append(hashNodes, chash.Node{
					ID: override.Host, Target: nodeTarget, Weight: nodeWeight,
				})
				retryScan += nodeWeight
			}
			if len(nodeWeights) == 0 {
				continue
			}
			hashOn := ""
			if strings.EqualFold(upstream.Type, "chash") {
				hashOn = upstream.HashOn
				if hashOn == "" {
					hashOn = "vars"
				}
			}
			var ring *chash.Ring
			if hashOn != "" {
				var ringErr error
				ring, ringErr = chash.New(hashNodes)
				if ringErr != nil {
					return fmt.Errorf("traffic-split rule %d chash ring invalid: %w", ruleIndex, ringErr)
				}
			}
			var targetBalancer pxy.LoadBalancer
			var targetTransport http.RoundTripper
			var err error
			if p.runtimeAcquirerSet {
				if p.runtimeAcquirer == nil {
					return fmt.Errorf("traffic-split rule %d upstream runtime acquirer is not configured", ruleIndex)
				}
				runtime, err := p.runtimeAcquirer.Acquire(upstream, nodeWeights, nodePriorities)
				if err != nil {
					return fmt.Errorf(
						"traffic-split rule %d upstream runtime materialization failed: %w",
						ruleIndex,
						err,
					)
				}
				if runtime == nil || runtime.LoadBalancer == nil || runtime.RoundTripper == nil {
					return fmt.Errorf(
						"traffic-split rule %d upstream runtime materialization returned incomplete runtime",
						ruleIndex,
					)
				}
				targetBalancer = runtime.LoadBalancer
				targetTransport = runtime.RoundTripper
			} else {
				targetBalancer, err = pxy.NewUpstreamLoadBalanceWithPriorities(
					nodeWeights,
					nodePriorities,
					upstream.Checks,
				)
				if err != nil {
					return fmt.Errorf("traffic-split rule %d upstream health checks invalid: %w", ruleIndex, err)
				}
			}
			reporter, _ := targetBalancer.(pxy.HealthReporter)
			for nodeID, override := range nodeOverrides {
				override.HealthReporter = reporter
				override.HealthTarget = nodeID
				override.RoundTripper = targetTransport
			}
			targetWeights[targetID] += weight
			targets[targetID] = compiledTarget{
				balancer:   targetBalancer,
				overrides:  nodeOverrides,
				priorities: nodePriorities,
				hashOn:     hashOn,
				key:        upstream.Key,
				ring:       ring,
				retryScan:  retryScan,
			}
		}

		compiled := compiledRule{
			exprs:   exprs,
			targets: targets,
			err:     compileErr,
		}
		if len(targetWeights) > 0 {
			compiled.balancer = pxy.NewWeightedRRLoadBalance(targetWeights)
		}
		p.rules = append(p.rules, compiled)
	}

	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

// RetriesConfigured reports whether retries was explicitly present, including
// an explicit zero.
func (u Upstream) RetriesConfigured() bool {
	return u.retriesSet || u.Retries != 0
}

func projectInlineUpstream(source *Upstream) *Upstream {
	if source == nil {
		return nil
	}
	return &Upstream{
		Name:         source.Name,
		Type:         source.Type,
		Scheme:       source.Scheme,
		PassHost:     source.PassHost,
		UpstreamHost: source.UpstreamHost,
		HashOn:       source.HashOn,
		Key:          source.Key,
		Timeout:      source.Timeout,
		Nodes:        append([]Node(nil), source.Nodes...),
	}
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
		if rule.err != nil {
			return nil, rule.err
		}
		matched, err := p.matchRule(r, rule.exprs)
		if err != nil {
			return nil, err
		}
		if !matched {
			continue
		}
		if rule.balancer == nil {
			return nil, nil
		}
		target, ok := rule.targets[rule.balancer.Next()]
		if !ok || target.fallback {
			return nil, nil
		}
		var nodeID string
		if target.hashOn != "" {
			nodeID = target.selectHashedNode(r)
			pxy.RecordSelectedTarget(target.balancer, r, nodeID)
		} else {
			nodeID = pxy.NextTarget(target.balancer, r)
		}
		return target.requestOverride(r, nodeID), nil
	}
	return nil, nil
}

func (target compiledTarget) requestOverride(request *http.Request, nodeID string) *Override {
	base := target.overrides[nodeID]
	if base == nil {
		return nil
	}
	override := *base
	override.NextRetry = func(retry *http.Request) *Override {
		nextID := target.nextRetryNode(retry, nodeID)
		if nextID == "" {
			return nil
		}
		return target.requestOverride(retry, nextID)
	}
	return &override
}

func (target compiledTarget) nextRetryNode(request *http.Request, previous string) string {
	if target.balancer == nil || len(target.overrides) < 2 {
		return ""
	}
	scanLimit := target.retryScan
	scanLimit = max(scanLimit, len(target.overrides))
	for range scanLimit + 1 {
		nodeID := pxy.NextTarget(target.balancer, request)
		if nodeID != "" && nodeID != previous {
			return nodeID
		}
	}
	return ""
}

func configuredWeight(weight int, configured bool) int {
	if weight == 0 && !configured {
		return 1
	}
	return weight
}

func validateUpstreamHostMode(upstream *Upstream) error {
	switch upstream.PassHost {
	case "", "pass", "node":
		return nil
	case "rewrite":
		if upstream.UpstreamHost == "" {
			return fmt.Errorf("pass_host=\"rewrite\" requires upstream_host")
		}
		return nil
	default:
		return fmt.Errorf("pass_host must be one of pass, node, or rewrite")
	}
}

func validateUpstreamHash(upstream *Upstream) error {
	if !strings.EqualFold(upstream.Type, "chash") {
		return nil
	}
	hashOn := upstream.HashOn
	if hashOn == "" {
		hashOn = "vars"
	}
	switch hashOn {
	case "vars", "header", "cookie", "consumer", "vars_combinations":
	default:
		return fmt.Errorf("hash_on must be one of vars, header, cookie, consumer, or vars_combinations")
	}
	if hashOn != "consumer" && upstream.Key == "" {
		return fmt.Errorf("chash upstream requires key when hash_on is not consumer")
	}
	return nil
}

func (target compiledTarget) selectHashedNode(r *http.Request) string {
	if target.ring == nil {
		return ""
	}
	value := resolveHashValue(r, target.hashOn, target.key)
	if value == "" {
		value = pluginexpr.String(pluginexpr.RequestValue(r, "remote_addr"))
	}
	candidates := target.ring.Candidates(value)
	maxPriority := 0
	found := false
	for _, selected := range candidates {
		if health, ok := target.balancer.(interface{ IsHealthy(string) bool }); ok && !health.IsHealthy(selected) {
			continue
		}
		priority := target.priorities[selected]
		if !found || priority > maxPriority {
			maxPriority = priority
			found = true
		}
	}
	for _, selected := range candidates {
		if target.priorities[selected] != maxPriority {
			continue
		}
		if health, ok := target.balancer.(interface{ IsHealthy(string) bool }); ok && !health.IsHealthy(selected) {
			continue
		}
		return selected
	}
	return pxy.NextTarget(target.balancer, r)
}

func resolveHashValue(r *http.Request, hashOn string, key string) string {
	switch hashOn {
	case "header":
		return r.Header.Get(key)
	case "cookie":
		cookie, err := r.Cookie(key)
		if err == nil {
			return cookie.Value
		}
		return ""
	case "consumer":
		return pluginexpr.String(pluginexpr.RequestValue(r, "consumer_name"))
	case "vars_combinations":
		return resolveHashVariableCombination(r, key)
	default:
		return pluginexpr.String(pluginexpr.RequestValue(r, key))
	}
}

func resolveHashVariableCombination(r *http.Request, template string) string {
	matches := hashVariablePattern.FindAllStringSubmatchIndex(template, -1)
	if len(matches) == 0 {
		return ""
	}

	var value strings.Builder
	resolved := false
	position := 0
	for _, match := range matches {
		start, end := match[0], match[1]
		if start > 0 && template[start-1] == '\\' {
			value.WriteString(template[position:end])
			position = end
			continue
		}

		value.WriteString(template[position:start])
		variableStart, variableEnd := match[2], match[3]
		if variableStart < 0 {
			variableStart, variableEnd = match[4], match[5]
		}
		variable := strings.TrimSpace(template[variableStart:variableEnd])
		name, fallback, hasFallback := strings.Cut(variable, "??")
		name = strings.TrimSpace(name)
		resolvedValue := pluginexpr.String(pluginexpr.RequestValue(r, name))
		if resolvedValue == "" && hasFallback {
			resolvedValue = strings.TrimSpace(fallback)
		}
		if resolvedValue != "" {
			resolved = true
		}
		value.WriteString(resolvedValue)
		position = end
	}
	value.WriteString(template[position:])
	if !resolved {
		return ""
	}
	return value.String()
}

func overrideFromNode(upstream *Upstream, node Node) *Override {
	scheme := remapTrafficSplitScheme(upstream.Scheme)
	passHost := upstream.PassHost
	if passHost == "" {
		passHost = "pass"
	}
	return &Override{
		Scheme:       scheme,
		Host:         joinHostPort(scheme, node),
		PassHost:     passHost,
		UpstreamHost: upstream.UpstreamHost,
		Timeout:      upstream.Timeout,
		Retries:      configuredRetries(upstream),
	}
}

func (p *Plugin) resolveUpstreamByID(id string) (*Upstream, error) {
	if p.upstreamResolver == nil {
		return nil, fmt.Errorf("traffic-split upstream resolver is required")
	}
	stored, err := p.upstreamResolver(id)
	if err != nil {
		return nil, err
	}
	return upstreamFromResource(stored), nil
}

func overrideTargetURL(override *Override) string {
	return (&url.URL{Scheme: override.Scheme, Host: override.Host}).String()
}

func configuredRetries(upstream *Upstream) int {
	if upstream.retriesSet || upstream.Retries != 0 {
		return upstream.Retries
	}
	if len(upstream.Nodes) < 2 {
		return 0
	}
	return len(upstream.Nodes) - 1
}

func remapTrafficSplitScheme(scheme string) string {
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "", "grpc":
		return "http"
	case "grpcs":
		return "https"
	default:
		return scheme
	}
}

func joinHostPort(scheme string, node Node) string {
	if host, portText, err := net.SplitHostPort(node.Host); err == nil {
		if port, parseErr := strconv.Atoi(portText); parseErr == nil && (node.Port == 0 || node.Port == port) {
			return node.Host
		}
		node.Host = host
	}
	if node.Port == 0 {
		if scheme == "https" {
			node.Port = 443
		} else {
			node.Port = 80
		}
	}
	host := node.Host
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	return net.JoinHostPort(host, strconv.Itoa(node.Port))
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

func upstreamFromResource(stored resource.Upstream) *Upstream {
	upstream := &Upstream{
		Name:         stored.Name,
		Type:         stored.Type,
		Scheme:       stored.Scheme,
		TLS:          stored.TLS,
		PassHost:     stored.PassHost,
		UpstreamHost: stored.UpstreamHost,
		HashOn:       stored.HashOn,
		Key:          stored.Key,
		Timeout:      stored.Timeout,
		Retries:      stored.Retries,
		retriesSet:   stored.RetriesConfigured(),
		Checks:       stored.Checks,
		Nodes:        make([]Node, 0, len(stored.Nodes)),
	}
	for _, node := range stored.Nodes {
		upstream.Nodes = append(upstream.Nodes, Node{
			Host:      node.Host,
			Port:      node.Port,
			Weight:    node.Weight,
			Priority:  node.Priority,
			weightSet: node.WeightConfigured(),
		})
	}
	return upstream
}

func (p *Plugin) matchRule(r *http.Request, exprs []*pluginexpr.Expression) (bool, error) {
	if len(exprs) == 0 {
		return true, nil
	}
	var postArgs url.Values
	var postArgsErr error
	postArgsLoaded := false
	for _, expr := range exprs {
		if expr.Eval(func(name string) any {
			name = strings.TrimPrefix(name, "$")
			if strings.HasPrefix(name, "post_arg_") {
				if !postArgsLoaded {
					postArgs, postArgsErr = p.requestPostArgs(r)
					postArgsLoaded = true
				}
				if postArgsErr != nil {
					return ""
				}
				return postArgs.Get(strings.TrimPrefix(name, "post_arg_"))
			}
			return pluginexpr.RequestValue(r, name)
		}) {
			return true, postArgsErr
		}
		if postArgsErr != nil {
			return false, postArgsErr
		}
	}
	return false, nil
}

func (p *Plugin) requestPostArgs(r *http.Request) (url.Values, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/x-www-form-urlencoded" {
		return nil, nil
	}
	body, err := base.ReadRequestBody(r)
	if err != nil {
		return nil, err
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, nil
	}
	return values, nil
}
