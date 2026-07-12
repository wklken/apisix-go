package traffic_split

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

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
                    "type": {"type": "string"},
                    "scheme": {"type": "string"},
                    "pass_host": {
                      "type": "string",
                      "enum": ["pass", "node", "rewrite"],
                      "default": "pass"
                    },
					"upstream_host": {"type": "string", "minLength": 1},
					"hash_on": {
						"type": "string",
						"enum": ["vars", "header", "cookie", "consumer", "vars_combinations"]
					},
					"key": {"type": "string"},
					"timeout": {
						"type": "object",
						"properties": {
							"connect": {"type": "integer", "minimum": 0},
							"send": {"type": "integer", "minimum": 0},
							"read": {"type": "integer", "minimum": 0}
						}
					},
					"checks": {"type": "object"},
					"retries": {"type": "integer", "minimum": 0},
					"nodes": {"type": ["array", "object"]}
                  }
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
	Type         string                 `json:"type,omitempty"`
	Scheme       string                 `json:"scheme,omitempty"`
	PassHost     string                 `json:"pass_host,omitempty"`
	UpstreamHost string                 `json:"upstream_host,omitempty"`
	HashOn       string                 `json:"hash_on,omitempty"`
	Key          string                 `json:"key,omitempty"`
	Timeout      resource.Timeout       `json:"timeout,omitempty"`
	Retries      int                    `json:"retries,omitempty"`
	Checks       map[string]interface{} `json:"checks,omitempty"`
	Nodes        []Node                 `json:"nodes,omitempty"`
}

type Node struct {
	Host      string `json:"host,omitempty"`
	Port      int    `json:"port,omitempty"`
	Weight    int    `json:"weight,omitempty"`
	weightSet bool
}

type Override struct {
	Scheme         string
	Host           string
	PassHost       string
	UpstreamHost   string
	Timeout        resource.Timeout
	Retries        int
	HealthReporter pxy.HealthReporter
	HealthTarget   string
}

type compiledRule struct {
	exprs    []*pluginexpr.Expression
	balancer pxy.LoadBalancer
	targets  map[string]compiledTarget
	err      error
}

type compiledTarget struct {
	fallback  bool
	balancer  pxy.LoadBalancer
	overrides map[string]*Override
	hashOn    string
	key       string
	hashNodes []hashNode
}

type hashNode struct {
	id     string
	weight int
}

type overrideKey struct{}

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
			nodeOverrides := map[string]*Override{}
			hashNodes := make([]hashNode, 0, len(upstream.Nodes))
			for _, node := range upstream.Nodes {
				override := overrideFromNode(upstream, node)
				nodeWeight := configuredWeight(node.Weight, node.weightSet)
				if nodeWeight == 0 {
					continue
				}
				nodeID := fmt.Sprintf("%s-node-%d", targetID, len(nodeWeights))
				nodeWeights[nodeID] = nodeWeight
				nodeOverrides[nodeID] = override
				hashNodes = append(hashNodes, hashNode{id: nodeID, weight: nodeWeight})
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
			targetBalancer, err := pxy.NewUpstreamLoadBalance(nodeWeights, upstream.Checks)
			if err != nil {
				return fmt.Errorf("traffic-split rule %d upstream health checks invalid: %w", ruleIndex, err)
			}
			reporter, _ := targetBalancer.(pxy.HealthReporter)
			for nodeID, override := range nodeOverrides {
				override.HealthReporter = reporter
				override.HealthTarget = nodeID
			}
			targetWeights[targetID] += weight
			targets[targetID] = compiledTarget{
				balancer:  targetBalancer,
				overrides: nodeOverrides,
				hashOn:    hashOn,
				key:       upstream.Key,
				hashNodes: hashNodes,
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

		if timeout := upstreamTimeout(override.Timeout); timeout > 0 {
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			r = r.WithContext(ctx)
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
		if !matchRule(r, rule.exprs) {
			continue
		}
		if rule.balancer == nil {
			return nil, nil
		}
		target, ok := rule.targets[rule.balancer.Next()]
		if !ok || target.fallback {
			return nil, nil
		}
		nodeID := target.balancer.Next()
		if target.hashOn != "" {
			nodeID = target.selectHashedNode(r)
		}
		return target.overrides[nodeID], nil
	}
	return nil, nil
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
	if len(target.hashNodes) == 0 {
		return ""
	}
	value := resolveHashValue(r, target.hashOn, target.key)
	if value == "" {
		value = pluginexpr.String(pluginexpr.RequestValue(r, "remote_addr"))
	}
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(value))
	var total uint64
	for _, node := range target.hashNodes {
		if node.weight > 0 {
			total += uint64(node.weight)
		}
	}
	if total == 0 {
		return target.balancer.Next()
	}
	offset := uint64(hasher.Sum32()) % total
	selected := target.hashNodes[len(target.hashNodes)-1].id
	for _, node := range target.hashNodes {
		if offset < uint64(node.weight) {
			selected = node.id
			break
		}
		offset -= uint64(node.weight)
	}
	if health, ok := target.balancer.(interface{ IsHealthy(string) bool }); ok && !health.IsHealthy(selected) {
		return target.balancer.Next()
	}
	return selected
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
	scheme := upstream.Scheme
	if scheme == "" {
		scheme = "http"
	}
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
		Retries:      upstream.Retries,
	}
}

func upstreamTimeout(timeout resource.Timeout) time.Duration {
	seconds := []int{timeout.Connect, timeout.Send, timeout.Read}
	minimum := 0
	for _, value := range seconds {
		if value <= 0 {
			continue
		}
		if minimum == 0 || value < minimum {
			minimum = value
		}
	}
	if minimum == 0 {
		return 0
	}
	return time.Duration(minimum) * time.Second
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
		Type:         stored.Type,
		Scheme:       stored.Scheme,
		PassHost:     stored.PassHost,
		UpstreamHost: stored.UpstreamHost,
		HashOn:       stored.HashOn,
		Key:          stored.Key,
		Timeout:      stored.Timeout,
		Retries:      stored.Retries,
		Checks:       stored.Checks,
		Nodes:        make([]Node, 0, len(stored.Nodes)),
	}
	for _, node := range stored.Nodes {
		upstream.Nodes = append(upstream.Nodes, Node{
			Host:      node.Host,
			Port:      node.Port,
			Weight:    node.Weight,
			weightSet: node.WeightConfigured(),
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
