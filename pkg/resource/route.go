package resource

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
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

	Retries      int            `json:"retries,omitempty"`
	Checks       map[string]any `json:"checks,omitempty"`
	HashOn       string         `json:"hash_on,omitempty"`
	Key          string         `json:"key,omitempty"`
	PassHost     string         `json:"pass_host,omitempty"`
	UpstreamHost string         `json:"upstream_host,omitempty"`
	Name         string         `json:"name,omitempty"`
	Desc         string         `json:"desc,omitempty"`
}

func (s *Upstream) UnmarshalJSON(data []byte) error {
	// FIXME: refactor it
	var upstreamData map[string]json.RawMessage
	if err := json.Unmarshal(data, &upstreamData); err != nil {
		return fmt.Errorf("unmarshal to json.RawMessage fail, %w", err)
	}

	var nodes []Node
	if err := json.Unmarshal(upstreamData["nodes"], &nodes); err == nil {
		s.Nodes = nodes
	} else {
		/*
			"nodes": {
				"httpbin.org": 1
			}
		*/
		var nodes map[string]int
		if err := json.Unmarshal(upstreamData["nodes"], &nodes); err == nil {
			for host, weight := range nodes {
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

	if raw := upstreamData["type"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &s.Type); err != nil {
			return fmt.Errorf("unmarshal field `type` fail, %w", err)
		}
	}

	if raw := upstreamData["scheme"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &s.Scheme); err != nil {
			return fmt.Errorf("unmarshal field `scheme` fail, %w", err)
		}
	}

	if raw := upstreamData["timeout"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &s.Timeout); err != nil {
			return fmt.Errorf("unmarshal field `timeout` fail, %w", err)
		}
	}

	if raw := upstreamData["tls"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &s.TLS); err != nil {
			return fmt.Errorf("unmarshal field `tls` fail, %w", err)
		}
	}

	if upstreamData["retries"] != nil {
		if err := json.Unmarshal(upstreamData["retries"], &s.Retries); err != nil {
			return fmt.Errorf("unmarshal field `retries` fail, %w", err)
		}
	}

	if upstreamData["checks"] != nil {
		if err := json.Unmarshal(upstreamData["checks"], &s.Checks); err != nil {
			return fmt.Errorf("unmarshal field `checks` fail, %w", err)
		}
	}

	if upstreamData["hash_on"] != nil {
		if err := json.Unmarshal(upstreamData["hash_on"], &s.HashOn); err != nil {
			return fmt.Errorf("unmarshal field `hash_on` fail, %w", err)
		}
	}

	if upstreamData["key"] != nil {
		if err := json.Unmarshal(upstreamData["key"], &s.Key); err != nil {
			return fmt.Errorf("unmarshal field `key` fail, %w", err)
		}
	}

	if upstreamData["pass_host"] != nil {
		if err := json.Unmarshal(upstreamData["pass_host"], &s.PassHost); err != nil {
			return fmt.Errorf("unmarshal field `pass_host` fail, %w", err)
		}
	}

	if upstreamData["upstream_host"] != nil {
		if err := json.Unmarshal(upstreamData["upstream_host"], &s.UpstreamHost); err != nil {
			return fmt.Errorf("unmarshal field `upstream_host` fail, %w", err)
		}
	}

	if upstreamData["name"] != nil {
		if err := json.Unmarshal(upstreamData["name"], &s.Name); err != nil {
			return fmt.Errorf("unmarshal field `name` fail, %w", err)
		}
	}

	if upstreamData["desc"] != nil {
		if err := json.Unmarshal(upstreamData["desc"], &s.Desc); err != nil {
			return fmt.Errorf("unmarshal field `desc` fail, %w", err)
		}
	}

	return nil
}

func parseNodeAddress(address string) (string, int) {
	const defaultPort = 80
	if _, portText, err := net.SplitHostPort(address); err == nil {
		if port, parseErr := strconv.Atoi(portText); parseErr == nil {
			return address, port
		}
	}
	if strings.HasPrefix(address, "[") && strings.HasSuffix(address, "]") {
		return address, defaultPort
	}
	if strings.Count(address, ":") == 1 {
		_, portText, _ := strings.Cut(address, ":")
		if port, err := strconv.Atoi(portText); err == nil {
			return address, port
		}
	}
	return address, defaultPort
}

type Timeout struct {
	Connect int `json:"connect,omitempty"`
	Send    int `json:"send,omitempty"`
	Read    int `json:"read,omitempty"`
}

// UpstreamTLS contains the APISIX upstream TLS fields that are applicable to
// Kafka. client_cert_id is resolved from the local SSL resource store at the
// owner boundary.
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
	RemoteAddrs []string                `json:"remote_addrs,omitempty"`
	Vars        [][]string              `json:"vars,omitempty"`
	// FIXME: the ID maybe number => will unmarshal fail
	PluginConfigID string   `json:"plugin_config_id,omitempty"`
	ServiceID      string   `json:"service_id,omitempty"`
	UpstreamID     string   `json:"upstream_id,omitempty"`
	Upstream       Upstream `json:"upstream"`
	Timeout        struct {
		Connect int `json:"connect,omitempty"`
		Send    int `json:"send,omitempty"`
		Read    int `json:"read,omitempty"`
	} `json:"timeout"`
	FilterFunc string `json:"filter_func,omitempty"`

	CreateTime int64 `json:"create_time,omitempty"`
	UpdateTime int64 `json:"update_time,omitempty"`
	Status     int   `json:"status,omitempty"`
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
	Username string `json:"username,required"`
	GroupID  string `json:"group_id,omitempty"`
	Plugins  map[string]PluginConfig
	Labels   map[string]any `json:"labels,omitempty"`
}

type ConsumerGroup struct {
	Plugins map[string]PluginConfig
}

type GlobalRule struct {
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
