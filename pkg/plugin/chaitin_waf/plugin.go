package chaitin_waf

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/store"
)

type Plugin struct {
	base.BasePlugin
	config Config

	client *http.Client
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

type Config struct {
	Mode                 string      `json:"mode,omitempty"`
	Match                []MatchRule `json:"match,omitempty"`
	AppendWAFRespHeader  *bool       `json:"append_waf_resp_header,omitempty"`
	AppendWAFDebugHeader *bool       `json:"append_waf_debug_header,omitempty"`
	Config               WAFConfig   `json:"config,omitempty"`

	Nodes []Node `json:"nodes,omitempty"`
}

type Metadata struct {
	Mode   string    `json:"mode,omitempty"`
	Nodes  []Node    `json:"nodes"`
	Config WAFConfig `json:"config,omitempty"`
}

type MatchRule struct {
	Vars [][]any `json:"vars,omitempty"`
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

func (p *Plugin) Init() error {
	p.Name = name
	p.Priority = priority
	p.Schema = schema

	return nil
}

func (p *Plugin) PostInit() error {
	p.applyDefaults()
	p.client = &http.Client{Timeout: time.Duration(p.config.Config.ReadTimeout) * time.Millisecond}

	return nil
}

func (p *Plugin) Config() interface{} {
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
				w.Write([]byte(body))
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
	effective := p.effectiveConfig()
	if len(effective.Nodes) == 0 {
		headers[HeaderChaitinWAF] = "err"
		headers[HeaderChaitinWAFError] = "missing metadata"
		return http.StatusInternalServerError, "", headers
	}

	node := effective.Nodes[0]
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

	headers[HeaderChaitinWAF] = "yes"
	headers[HeaderChaitinWAFAction] = "pass"
	if decision.Status == 0 {
		decision.Status = http.StatusOK
	}
	headers[HeaderChaitinWAFStatus] = strconv.Itoa(decision.Status)

	if decision.Status != http.StatusOK && decision.EventID != "" {
		headers[HeaderChaitinWAFAction] = "reject"
		if effective.Mode == "monitor" {
			return 0, "", headers
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

func (p *Plugin) effectiveConfig() effectiveConfig {
	metadata := p.loadMetadata()
	cfg := effectiveConfig{
		Mode:   "monitor",
		Nodes:  metadata.Nodes,
		Config: applyWAFConfigDefaults(metadata.Config),
	}
	if metadata.Mode != "" {
		cfg.Mode = metadata.Mode
	}
	if p.config.Mode != "" {
		cfg.Mode = p.config.Mode
	}
	if len(p.config.Nodes) > 0 {
		cfg.Nodes = p.config.Nodes
	}
	cfg.Config = mergeWAFConfig(cfg.Config, p.config.Config)
	return cfg
}

func (p *Plugin) loadMetadata() (metadata Metadata) {
	defer func() {
		if recover() != nil {
			metadata = Metadata{}
		}
	}()
	store.GetPluginMetadata(name, &metadata)
	return metadata
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
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(cfg.ReqBodySize)*1024))
	if err != nil {
		return wafDecision{}, 0, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	endpoint := "http://" + node.hostPort() + r.URL.RequestURI()
	req, err := http.NewRequestWithContext(r.Context(), r.Method, endpoint, bytes.NewReader(body))
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
	defer resp.Body.Close()

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
	if len(p.config.Match) == 0 {
		return true
	}
	for _, rule := range p.config.Match {
		if matchVars(rule.Vars, r) {
			return true
		}
	}
	return false
}

func matchVars(vars [][]any, r *http.Request) bool {
	for _, expr := range vars {
		if len(expr) != 3 {
			return false
		}
		left, _ := expr[0].(string)
		op, _ := expr[1].(string)
		want := fmt.Sprint(expr[2])
		got := requestVar(left, r)
		switch op {
		case "==":
			if got != want {
				return false
			}
		case "!=":
			if got == want {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func requestVar(name string, r *http.Request) string {
	switch name {
	case "method", "request_method":
		return r.Method
	case "uri":
		return r.URL.Path
	case "request_uri":
		return r.URL.RequestURI()
	case "host":
		return r.Host
	default:
		if strings.HasPrefix(name, "http_") {
			return r.Header.Get(strings.ReplaceAll(strings.TrimPrefix(name, "http_"), "_", "-"))
		}
		return ""
	}
}

func clientIP(r *http.Request, realClientIP bool) string {
	if realClientIP {
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			return strings.TrimSpace(strings.Split(forwarded, ",")[0])
		}
		if realIP := r.Header.Get("X-Real-IP"); realIP != "" {
			return realIP
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func (n Node) hostPort() string {
	port := n.Port
	if port == 0 {
		port = 80
	}
	return net.JoinHostPort(n.Host, strconv.Itoa(port))
}
