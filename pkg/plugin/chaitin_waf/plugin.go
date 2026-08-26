package chaitin_waf

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/config"
	"github.com/wklken/apisix-go/pkg/httpclient"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	pluginexpr "github.com/wklken/apisix-go/pkg/plugin/expr"
	"github.com/wklken/apisix-go/pkg/util"
)

type Plugin struct {
	base.BasePlugin
	config    Config
	effective effectiveConfig

	client *http.Client
	picker nodePicker
	match  []*pluginexpr.Expression
}

const (
	priority = 2700
	name     = "chaitin-waf"

	HeaderChaitinWAF       = "X-APISIX-CHAITIN-WAF"
	HeaderChaitinWAFError  = "X-APISIX-CHAITIN-WAF-ERROR"
	HeaderChaitinWAFTime   = "X-APISIX-CHAITIN-WAF-TIME"
	HeaderChaitinWAFStatus = "X-APISIX-CHAITIN-WAF-STATUS"
	HeaderChaitinWAFAction = "X-APISIX-CHAITIN-WAF-ACTION"
	HeaderChaitinWAFServer = "X-APISIX-CHAITIN-WAF-SERVER"
)

const schema = `
{
  "type": "object",
  "properties": {
    "mode": {
      "type": "string",
      "enum": ["off", "monitor", "block"]
    },
    "match": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "vars": {
            "type": "array"
          }
        }
      }
    },
    "append_waf_resp_header": {
      "type": "boolean",
      "default": true
    },
    "append_waf_debug_header": {
      "type": "boolean",
      "default": false
    },
    "config": {
      "type": "object",
      "properties": {
        "connect_timeout": {"type": "integer"},
        "send_timeout": {"type": "integer"},
        "read_timeout": {"type": "integer"},
        "req_body_size": {"type": "integer"},
        "keepalive_size": {"type": "integer"},
        "keepalive_timeout": {"type": "integer"},
        "real_client_ip": {"type": "boolean"}
      }
    }
  }
}
`

const metadataSchema = `
{
  "type": "object",
  "properties": {
    "mode": {"type": "string", "enum": ["off", "monitor", "block"]},
    "nodes": {
      "type": "array",
      "minItems": 1,
      "items": {
        "type": "object",
        "properties": {
          "host": {"type": "string"},
          "port": {"type": "integer", "minimum": 1, "default": 80}
        },
        "required": ["host"]
      }
    },
    "config": {"type": "object"}
  },
  "required": ["nodes"]
}
`

type Config struct {
	Mode                 string      `json:"mode,omitempty"`
	Match                []MatchRule `json:"match,omitempty"`
	AppendWAFRespHeader  *bool       `json:"append_waf_resp_header,omitempty"`
	AppendWAFDebugHeader *bool       `json:"append_waf_debug_header,omitempty"`
	Config               WAFConfig   `json:"config"`

	Nodes []Node `json:"nodes,omitempty"`
}

type Metadata struct {
	Mode   string    `json:"mode,omitempty"`
	Nodes  []Node    `json:"nodes"`
	Config WAFConfig `json:"config"`
}

type MatchRule struct {
	Vars any `json:"vars,omitempty"`
}

type Node struct {
	Host string `json:"host"`
	Port int    `json:"port,omitempty"`
}

type WAFConfig struct {
	ConnectTimeout   int   `json:"connect_timeout,omitempty"`
	SendTimeout      int   `json:"send_timeout,omitempty"`
	ReadTimeout      int   `json:"read_timeout,omitempty"`
	ReqBodySize      int   `json:"req_body_size,omitempty"`
	KeepaliveSize    int   `json:"keepalive_size,omitempty"`
	KeepaliveTimeout int   `json:"keepalive_timeout,omitempty"`
	RealClientIP     *bool `json:"real_client_ip,omitempty"`
}

type wafDecision struct {
	Status  int    `json:"status"`
	EventID string `json:"event_id,omitempty"`
	Action  string `json:"action,omitempty"`
}

type effectiveConfig struct {
	Mode   string
	Nodes  []Node
	Config WAFConfig
}

const unhealthyNodeCooldown = 5 * time.Minute

type nodePicker struct {
	mu          sync.Mutex
	signature   string
	next        int
	unhealthyTo map[string]time.Time
}

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema
	p.MetadataSchema = metadataSchema

	return nil
}

func (p *Plugin) PostInit() error {
	routeConfig := p.config
	p.applyDefaults()
	if effective := p.StaticConfig(); effective != nil &&
		effective.Profiles.Security == config.SecurityStrict &&
		p.config.AppendWAFDebugHeader != nil && *p.config.AppendWAFDebugHeader {
		return fmt.Errorf("security_profile strict: append_waf_debug_header must be false")
	}
	var metadata Metadata
	if _, err := p.MetadataView().Decode(name, &metadata); err != nil {
		return fmt.Errorf("chaitin-waf metadata decode failed: %w", err)
	}
	p.effective = effectiveConfig{
		Mode:   "monitor",
		Nodes:  append([]Node(nil), metadata.Nodes...),
		Config: applyWAFConfigDefaults(metadata.Config),
	}
	if metadata.Mode != "" {
		p.effective.Mode = metadata.Mode
	}
	if routeConfig.Mode != "" {
		p.effective.Mode = routeConfig.Mode
	}
	if len(routeConfig.Nodes) > 0 {
		p.effective.Nodes = append([]Node(nil), routeConfig.Nodes...)
	}
	p.effective.Config = mergeWAFConfig(p.effective.Config, routeConfig.Config)
	p.match = p.match[:0]
	for index, rule := range p.config.Match {
		expression, err := pluginexpr.Compile(normalizeMatchVars(rule.Vars))
		if err != nil {
			return fmt.Errorf("chaitin-waf match %d vars validation failed: %w", index, err)
		}
		p.match = append(p.match, expression)
	}
	p.client = &http.Client{
		Transport: httpclient.NewTransport(),
		Timeout:   time.Duration(p.effective.Config.ReadTimeout) * time.Millisecond,
	}

	return nil
}

func (p *Plugin) Config() any {
	return &p.config
}

func (p *Plugin) Handler(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		code, body, headers := p.doAccess(r)
		if !*p.config.AppendWAFDebugHeader {
			delete(headers, HeaderChaitinWAFError)
			delete(headers, HeaderChaitinWAFServer)
		}
		if *p.config.AppendWAFRespHeader {
			for key, value := range headers {
				w.Header().Set(key, value)
			}
		}
		if code != 0 {
			w.WriteHeader(code)
			if body != "" {
				_, _ = w.Write([]byte(body))
			}
			return
		}
		next.ServeHTTP(w, r)
	}
	return http.HandlerFunc(fn)
}

func (p *Plugin) applyDefaults() {
	if p.config.AppendWAFRespHeader == nil {
		b := true
		p.config.AppendWAFRespHeader = &b
	}
	if p.config.AppendWAFDebugHeader == nil {
		b := false
		p.config.AppendWAFDebugHeader = &b
	}
	if p.config.Mode == "" {
		p.config.Mode = "monitor"
	}
	p.config.Config = applyWAFConfigDefaults(p.config.Config)
}

func applyWAFConfigDefaults(cfg WAFConfig) WAFConfig {
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 1000
	}
	if cfg.SendTimeout == 0 {
		cfg.SendTimeout = 1000
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 1000
	}
	if cfg.ReqBodySize == 0 {
		cfg.ReqBodySize = 1024
	}
	if cfg.KeepaliveSize == 0 {
		cfg.KeepaliveSize = 256
	}
	if cfg.KeepaliveTimeout == 0 {
		cfg.KeepaliveTimeout = 60000
	}
	if cfg.RealClientIP == nil {
		b := true
		cfg.RealClientIP = &b
	}
	return cfg
}

func (p *Plugin) doAccess(r *http.Request) (int, string, map[string]string) {
	headers := map[string]string{}
	effective := p.effective
	if len(effective.Nodes) == 0 {
		headers[HeaderChaitinWAF] = "err"
		headers[HeaderChaitinWAFError] = "missing metadata"
		return http.StatusInternalServerError, "", headers
	}

	node, ok := p.picker.pick(effective.Nodes)
	if !ok {
		headers[HeaderChaitinWAF] = "unhealthy"
		headers[HeaderChaitinWAFError] = "no healthy nodes"
		return http.StatusInternalServerError, "", headers
	}
	headers[HeaderChaitinWAFServer] = node.hostPort()

	if effective.Mode == "off" {
		headers[HeaderChaitinWAF] = "off"
		return 0, "", headers
	}
	if !p.matches(r) {
		headers[HeaderChaitinWAF] = "no"
		return 0, "", headers
	}

	decision, elapsed, err := p.askWAF(r, node, effective.Config)
	headers[HeaderChaitinWAFTime] = fmt.Sprintf("%.0f", elapsed.Seconds()*1000)
	if err != nil {
		p.picker.markFailure(node)
		headers[HeaderChaitinWAF] = "waf-err"
		if strings.Contains(strings.ToLower(err.Error()), "timeout") {
			headers[HeaderChaitinWAF] = "timeout"
		}
		headers[HeaderChaitinWAFError] = err.Error()
		if effective.Mode == "monitor" {
			return 0, "", headers
		}
		return http.StatusInternalServerError, "", headers
	}
	p.picker.markSuccess(node)

	headers[HeaderChaitinWAF] = "yes"
	headers[HeaderChaitinWAFAction] = "pass"
	if decision.Status == 0 {
		decision.Status = http.StatusOK
	}
	headers[HeaderChaitinWAFStatus] = strconv.Itoa(decision.Status)

	if decision.Status != http.StatusOK {
		headers[HeaderChaitinWAFAction] = "reject"
		if effective.Mode == "monitor" {
			return 0, "", headers
		}
		if _, ok := util.TerminalStatus(decision.Status); !ok {
			headers[HeaderChaitinWAF] = "waf-err"
			return http.StatusInternalServerError, "", headers
		}
		return decision.Status,
			fmt.Sprintf(
				`{"code": %d, "success":false, "message": "blocked by Chaitin SafeLine Web Application Firewall", "event_id": "%s"}`+"\n",
				decision.Status,
				decision.EventID,
			),
			headers
	}

	return 0, "", headers
}

func mergeWAFConfig(baseConfig, override WAFConfig) WAFConfig {
	if override.ConnectTimeout != 0 {
		baseConfig.ConnectTimeout = override.ConnectTimeout
	}
	if override.SendTimeout != 0 {
		baseConfig.SendTimeout = override.SendTimeout
	}
	if override.ReadTimeout != 0 {
		baseConfig.ReadTimeout = override.ReadTimeout
	}
	if override.ReqBodySize != 0 {
		baseConfig.ReqBodySize = override.ReqBodySize
	}
	if override.KeepaliveSize != 0 {
		baseConfig.KeepaliveSize = override.KeepaliveSize
	}
	if override.KeepaliveTimeout != 0 {
		baseConfig.KeepaliveTimeout = override.KeepaliveTimeout
	}
	if override.RealClientIP != nil {
		baseConfig.RealClientIP = override.RealClientIP
	}
	return baseConfig
}

func (p *Plugin) askWAF(r *http.Request, node Node, cfg WAFConfig) (wafDecision, time.Duration, error) {
	body, err := apisixctx.ReadRequestBody(r)
	if err != nil {
		return wafDecision{}, 0, err
	}
	inspectionBody := body
	limit := cfg.ReqBodySize * 1024
	if limit <= 0 {
		inspectionBody = inspectionBody[:0]
	} else if len(inspectionBody) > limit {
		inspectionBody = inspectionBody[:limit]
	}

	endpoint := "http://" + node.hostPort() + r.URL.RequestURI()
	req, err := http.NewRequestWithContext(r.Context(), r.Method, endpoint, bytes.NewReader(inspectionBody))
	if err != nil {
		return wafDecision{}, 0, err
	}
	req.Header = r.Header.Clone()
	req.Header.Set("X-Forwarded-For", clientIP(r, *cfg.RealClientIP))
	req.Header.Set("X-Forwarded-Method", r.Method)
	req.Header.Set("X-Forwarded-Host", r.Host)
	req.Header.Set("X-Forwarded-Uri", r.URL.RequestURI())

	start := time.Now()
	resp, err := p.client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		return wafDecision{}, elapsed, err
	}
	defer func() { _ = resp.Body.Close() }()

	var decision wafDecision
	if err := json.NewDecoder(resp.Body).Decode(&decision); err != nil {
		return wafDecision{}, elapsed, err
	}
	if decision.Status == 0 {
		decision.Status = resp.StatusCode
	}
	return decision, elapsed, nil
}

func (p *Plugin) matches(r *http.Request) bool {
	if len(p.match) == 0 {
		return true
	}
	for _, expression := range p.match {
		if expression.Eval(func(name string) any {
			return pluginexpr.RequestValue(r, name)
		}) {
			return true
		}
	}
	return false
}

func normalizeMatchVars(vars any) []any {
	switch typed := vars.(type) {
	case []any:
		return typed
	case [][]any:
		values := make([]any, len(typed))
		for i, expression := range typed {
			values[i] = expression
		}
		return values
	default:
		return nil
	}
}

func clientIP(r *http.Request, realClientIP bool) string {
	if realClientIP {
		return apisixctx.EffectiveRemoteIP(r)
	}
	return apisixctx.PeerRemoteIP(r)
}

func (n Node) hostPort() string {
	port := n.Port
	if port == 0 {
		port = 80
	}
	return net.JoinHostPort(n.Host, strconv.Itoa(port))
}

func (p *nodePicker) pick(nodes []Node) (Node, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	signature := nodesSignature(nodes)
	if signature != p.signature {
		p.signature = signature
		p.next = 0
		p.unhealthyTo = make(map[string]time.Time)
	}
	if len(nodes) == 0 {
		return Node{}, false
	}
	if p.unhealthyTo == nil {
		p.unhealthyTo = make(map[string]time.Time)
	}
	now := time.Now()
	for offset := range nodes {
		index := (p.next + offset) % len(nodes)
		node := nodes[index]
		key := node.hostPort()
		if until, ok := p.unhealthyTo[key]; ok {
			if until.After(now) {
				continue
			}
			delete(p.unhealthyTo, key)
		}
		p.next = (index + 1) % len(nodes)
		return node, true
	}
	return Node{}, false
}

func (p *nodePicker) markFailure(node Node) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unhealthyTo == nil {
		p.unhealthyTo = make(map[string]time.Time)
	}
	p.unhealthyTo[node.hostPort()] = time.Now().Add(unhealthyNodeCooldown)
}

func (p *nodePicker) markSuccess(node Node) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.unhealthyTo, node.hostPort())
}

func nodesSignature(nodes []Node) string {
	var builder strings.Builder
	for _, node := range nodes {
		builder.WriteString(node.hostPort())
		builder.WriteByte('|')
	}
	return builder.String()
}
