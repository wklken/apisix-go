package resource

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/wklken/apisix-go/pkg/json"
)

//	{
//	    "id": "1",                            # id, unnecessary.
//	    "uris": ["/a","/b"],                  # A set of uri.
//	    "methods": ["GET","POST"],            # Can fill multiple methods
//	    "hosts": ["a.com","b.com"],           # A set of host.
//	    "plugins": {},                        # Bound plugin
//	    "priority": 0,                        # If different routes contain the same `uri`, determine which route is matched first based on the attribute` priority`, the default value is 0.
//	    "name": "route-xxx",
//	    "desc": "hello world",
//	    "remote_addrs": ["127.0.0.1"],        # A set of Client IP.
//	    "vars": [["http_user", "==", "ios"]], # A list of one or more `[var, operator, val]` elements
//	    "upstream_id": "1",                   # upstream id, recommended
//	    "upstream": {},                       # upstream, not recommended
//	    "timeout": {                          # Set the upstream timeout for connecting, sending and receiving messages of the route.
//	        "connect": 3,
//	        "send": 3,
//	        "read": 3
//	    },
//	    "filter_func": ""                     # User-defined filtering function
//	}

type PluginConfig any

//	{
//	    "id": "1",                  # id
//	    "retries": 1,               # 请求重试次数
//	    "timeout": {                # 设置连接、发送消息、接收消息的超时时间，每项都为 15 秒
//	        "connect":15,
//	        "send":15,
//	        "read":15
//	    },
//	    "nodes": {"host:80": 100},  # 上游机器地址列表，格式为`地址 + 端口`
//	                                # 等价于 "nodes": [ {"host":"host", "port":80, "weight": 100} ],
//	    "type":"roundrobin",
//	    "checks": {},               # 配置健康检查的参数
//	    "hash_on": "",
//	    "key": "",
//	    "name": "upstream-xxx",     # upstream 名称
//	    "desc": "hello world",      # upstream 描述
//	    "scheme": "http"            # 跟上游通信时使用的 scheme，默认是 `http`
//	}
type Upstream struct {
	Type    string       `json:"type,omitempty"`
	Nodes   []Node       `json:"nodes,omitempty"`
	Scheme  string       `json:"scheme,omitempty"`
	Timeout Timeout      `json:"timeout"`
	TLS     *UpstreamTLS `json:"tls,omitempty"`

	DiscoveryType string `json:"discovery_type,omitempty"`
	ServiceName   string `json:"service_name,omitempty"`

	Retries      int `json:"retries,omitempty"`
	retriesSet   bool
	Checks       map[string]any `json:"checks,omitempty"`
	HashOn       string         `json:"hash_on,omitempty"`
	Key          string         `json:"key,omitempty"`
	PassHost     string         `json:"pass_host,omitempty"`
	UpstreamHost string         `json:"upstream_host,omitempty"`
	Name         string         `json:"name,omitempty"`
	Desc         string         `json:"desc,omitempty"`
}

func (s *Upstream) UnmarshalJSON(data []byte) error {
	s.Scheme = "http"
	var upstreamData map[string]json.RawMessage
	if err := json.Unmarshal(data, &upstreamData); err != nil {
		return fmt.Errorf("unmarshal to json.RawMessage fail, %w", err)
	}

	var nodes []Node
	nodesRaw, nodesPresent := upstreamData["nodes"]
	if err := json.Unmarshal(nodesRaw, &nodes); err == nil {
		s.Nodes = nodes
	} else if !nodesPresent && (len(upstreamData["discovery_type"]) > 0 || len(upstreamData["service_name"]) > 0) {
		// Discovery-only upstreams have no static nodes. Preserve the
		// compatibility fields so route compilation can reject them explicitly.
	} else {
		/*
			"nodes": {
				"httpbin.org": 1
			}
		*/
		var nodeMap map[string]int
		if err := json.Unmarshal(upstreamData["nodes"], &nodeMap); err == nil {
			addresses := make([]string, 0, len(nodeMap))
			for host := range nodeMap {
				addresses = append(addresses, host)
			}
			sort.Strings(addresses)
			for _, host := range addresses {
				weight := nodeMap[host]
				host, port := parseNodeAddress(host)

				s.Nodes = append(s.Nodes, Node{
					Host:      host,
					Port:      port,
					Weight:    weight,
					weightSet: true,
				})
			}
		} else {
			return fmt.Errorf("unmarshal field `nodes` fail, %w", err)
		}
	}

	for _, field := range []struct {
		name string
		raw  json.RawMessage
		dest any
	}{
		{name: "type", raw: upstreamData["type"], dest: &s.Type},
		{name: "scheme", raw: upstreamData["scheme"], dest: &s.Scheme},
		{name: "timeout", raw: upstreamData["timeout"], dest: &s.Timeout},
		{name: "tls", raw: upstreamData["tls"], dest: &s.TLS},
		{name: "discovery_type", raw: upstreamData["discovery_type"], dest: &s.DiscoveryType},
		{name: "service_name", raw: upstreamData["service_name"], dest: &s.ServiceName},
		{name: "retries", raw: upstreamData["retries"], dest: &s.Retries},
		{name: "checks", raw: upstreamData["checks"], dest: &s.Checks},
		{name: "hash_on", raw: upstreamData["hash_on"], dest: &s.HashOn},
		{name: "key", raw: upstreamData["key"], dest: &s.Key},
		{name: "pass_host", raw: upstreamData["pass_host"], dest: &s.PassHost},
		{name: "upstream_host", raw: upstreamData["upstream_host"], dest: &s.UpstreamHost},
		{name: "name", raw: upstreamData["name"], dest: &s.Name},
		{name: "desc", raw: upstreamData["desc"], dest: &s.Desc},
	} {
		if len(field.raw) == 0 {
			continue
		}
		if err := json.Unmarshal(field.raw, field.dest); err != nil {
			return fmt.Errorf("unmarshal field `%s` fail, %w", field.name, err)
		}
	}

	if raw := upstreamData["retries"]; raw != nil {
		s.retriesSet = true
	}

	return nil
}

// RetriesConfigured reports whether retries was explicitly present, including zero.
func (s Upstream) RetriesConfigured() bool {
	return s.retriesSet || s.Retries != 0
}

func parseNodeAddress(address string) (string, int) {
	const defaultPort = 80
	if _, portText, err := net.SplitHostPort(address); err == nil {
		if port, parseErr := strconv.Atoi(portText); parseErr == nil {
			host, _, _ := net.SplitHostPort(address)
			return host, port
		}
	}
	if strings.HasPrefix(address, "[") && strings.HasSuffix(address, "]") {
		return address, defaultPort
	}
	if strings.Count(address, ":") == 1 {
		host, portText, _ := strings.Cut(address, ":")
		if port, err := strconv.Atoi(portText); err == nil {
			return host, port
		}
	}
	return address, defaultPort
}

type Timeout struct {
	Connect int `json:"connect,omitempty"`
	Send    int `json:"send,omitempty"`
	Read    int `json:"read,omitempty"`
}

// UpstreamTLS contains APISIX upstream TLS fields used by HTTPS/grpcs and
// Kafka owners. client_cert_id is resolved from the local SSL resource store
// at the protocol owner boundary.
type UpstreamTLS struct {
	ClientCertID any    `json:"client_cert_id,omitempty" yaml:"client_cert_id,omitempty"`
	ClientCert   string `json:"client_cert,omitempty" yaml:"client_cert,omitempty"`
	ClientKey    string `json:"client_key,omitempty" yaml:"client_key,omitempty"`
	Verify       bool   `json:"verify,omitempty" yaml:"verify,omitempty"`
}

type Node struct {
	Host      string `json:"host,omitempty"`
	Port      int    `json:"port,omitempty"`
	Weight    int    `json:"weight,omitempty"`
	Priority  int    `json:"priority,omitempty"`
	weightSet bool
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
	n.Weight = 0
	n.weightSet = false
	if raw.Weight != nil {
		n.Weight = *raw.Weight
		n.weightSet = true
	}
	return nil
}

func (n Node) WeightConfigured() bool {
	return n.weightSet || n.Weight != 0
}

type Route struct {
	ID          string                  `json:"id,omitempty"`
	Uri         string                  `json:"uri,omitempty"`
	Uris        []string                `json:"uris,omitempty"`
	Methods     []string                `json:"methods,omitempty"`
	Hosts       []string                `json:"hosts,omitempty"`
	Plugins     map[string]PluginConfig `json:"plugins,omitempty"`
	Priority    int                     `json:"priority,omitempty"`
	Name        string                  `json:"name,omitempty"`
	Desc        string                  `json:"desc,omitempty"`
	Labels      map[string]any          `json:"labels,omitempty"`
	RemoteAddr  string                  `json:"remote_addr,omitempty"`
	RemoteAddrs []string                `json:"remote_addrs,omitempty"`
	Vars        json.RawMessage         `json:"vars,omitempty"`
	// FIXME: the ID maybe number => will unmarshal fail
	PluginConfigID  string          `json:"plugin_config_id,omitempty"`
	ServiceID       string          `json:"service_id,omitempty"`
	EnableWebsocket bool            `json:"enable_websocket,omitempty"`
	UpstreamID      string          `json:"upstream_id,omitempty"`
	Upstream        Upstream        `json:"upstream"`
	Timeout         Timeout         `json:"timeout"`
	Script          json.RawMessage `json:"script,omitempty"`
	FilterFunc      string          `json:"filter_func,omitempty"`

	CreateTime int64 `json:"create_time,omitempty"`
	UpdateTime int64 `json:"update_time,omitempty"`
	Status     int   `json:"status,omitempty"`
	statusSet  bool
}

func (r *Route) UnmarshalJSON(data []byte) error {
	type routeJSON Route
	aux := struct {
		routeJSON
		Status json.RawMessage `json:"status"`
	}{}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*r = Route(aux.routeJSON)
	if len(aux.Status) == 0 {
		r.statusSet = false
		r.Status = 0
		return nil
	}
	if strings.TrimSpace(string(aux.Status)) == "null" {
		return fmt.Errorf("unmarshal field `status` fail: null is not allowed")
	}
	if err := json.Unmarshal(aux.Status, &r.Status); err != nil {
		return fmt.Errorf("unmarshal field `status` fail, %w", err)
	}
	r.statusSet = true
	return nil
}

func (r Route) StatusConfigured() bool {
	return r.statusSet
}

func (r Route) Disabled() bool {
	return r.statusSet && r.Status == 0
}

// StreamRoute describes the APISIX L4 route fields used by the Go stream
// owner. The stream listener and remote address are matched before the
// selected upstream is dialed.
type StreamRoute struct {
	ID         string                  `json:"id,omitempty"`
	ServerAddr string                  `json:"server_addr,omitempty"`
	ServerPort int                     `json:"server_port,omitempty"`
	RemoteAddr string                  `json:"remote_addr,omitempty"`
	Plugins    map[string]PluginConfig `json:"plugins,omitempty"`
	UpstreamID string                  `json:"upstream_id,omitempty"`
	Upstream   Upstream                `json:"upstream"`
}

type Service struct {
	ID         string                  `json:"id,omitempty"`
	Plugins    map[string]PluginConfig `json:"plugins,omitempty"`
	UpstreamID string                  `json:"upstream_id,omitempty"`
	Upstream   Upstream                `json:"upstream"`

	Name            string   `json:"name,omitempty"`
	Desc            string   `json:"desc,omitempty"`
	EnableWebsocket bool     `json:"enable_websocket,omitempty"`
	Hosts           []string `json:"hosts,omitempty"`
}

// {"username":"foo","plugins":{"basic-auth":{"_meta":{"disable":false},"password":"bar","username":"foo"}},"create_time":1712331168,"update_time":1712331168}
type Consumer struct {
	Username     string                  `json:"username"`
	GroupID      string                  `json:"group_id,omitempty"`
	Plugins      map[string]PluginConfig `json:"plugins" yaml:"plugins"`
	Labels       map[string]any          `json:"labels,omitempty"`
	ConfigDigest [32]byte                `json:"-" yaml:"-"`
}

type ConsumerGroup struct {
	Plugins      map[string]PluginConfig
	ConfigDigest [32]byte `json:"-" yaml:"-"`
}

type GlobalRule struct {
	ID      string                  `json:"id,omitempty"`
	Plugins map[string]PluginConfig `json:"plugins,omitempty"`
}

type PluginConfigRule struct {
	Desc    string                  `json:"desc,omitempty"`
	Plugins map[string]PluginConfig `json:"plugins,omitempty"`
}

type Proto struct {
	ID      string `json:"id,omitempty"`
	Content string `json:"content,omitempty"`
}
