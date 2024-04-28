package resource

import (
	"encoding/json"
	"fmt"
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

type PluginConfig interface{}

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
	Type    string  `json:"type,omitempty"`
	Nodes   []Node  `json:"nodes,omitempty"`
	Scheme  string  `json:"scheme,omitempty"`
	Timeout Timeout `json:"timeout,omitempty"`

	Retries int                    `json:"retries,omitempty"`
	Checks  map[string]interface{} `json:"checks,omitempty"`
	HashOn  string                 `json:"hash_on,omitempty"`
	Key     string                 `json:"key,omitempty"`
	Name    string                 `json:"name,omitempty"`
	Desc    string                 `json:"desc,omitempty"`
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
				port := 80
				if strings.Contains(host, ":") {
					port, _ = strconv.Atoi(strings.Split(host, ":")[1])
				}

				s.Nodes = append(s.Nodes, Node{
					Host:   host,
					Port:   port,
					Weight: weight,
				})
			}
		} else {
			return fmt.Errorf("unmarshal field `nodes` fail, %w", err)
		}
	}

	if err := json.Unmarshal(upstreamData["type"], &s.Type); err != nil {
		return fmt.Errorf("unmarshal field `type` fail, %w", err)
	}

	if err := json.Unmarshal(upstreamData["scheme"], &s.Scheme); err != nil {
		return fmt.Errorf("unmarshal field `scheme` fail, %w", err)
	}

	if err := json.Unmarshal(upstreamData["timeout"], &s.Timeout); err != nil {
		return fmt.Errorf("unmarshal field `timeout` fail, %w", err)
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

type Timeout struct {
	Connect int `json:"connect,omitempty"`
	Send    int `json:"send,omitempty"`
	Read    int `json:"read,omitempty"`
}

type Node struct {
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	Weight   int    `json:"weight,omitempty"`
	Priority int    `json:"priority,omitempty"`
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
	Upstream       Upstream `json:"upstream,omitempty"`
	Timeout        struct {
		Connect int `json:"connect,omitempty"`
		Send    int `json:"send,omitempty"`
		Read    int `json:"read,omitempty"`
	} `json:"timeout,omitempty"`
	FilterFunc string `json:"filter_func,omitempty"`

	CreateTime int64 `json:"create_time,omitempty"`
	UpdateTime int64 `json:"update_time,omitempty"`
	Status     int   `json:"status,omitempty"`
}

type Service struct {
	ID         string                  `json:"id,omitempty"`
	Plugins    map[string]PluginConfig `json:"plugins,omitempty"`
	UpstreamID string                  `json:"upstream_id,omitempty"`
	Upstream   Upstream                `json:"upstream,omitempty"`

	Name            string   `json:"name,omitempty"`
	Desc            string   `json:"desc,omitempty"`
	EnableWebsocket bool     `json:"enable_websocket,omitempty"`
	Hosts           []string `json:"hosts,omitempty"`
}

// {"username":"foo","plugins":{"basic-auth":{"_meta":{"disable":false},"password":"bar","username":"foo"}},"create_time":1712331168,"update_time":1712331168}
type Consumer struct {
	Username string `json:"username,required"`
	Plugins  map[string]PluginConfig
}

type GlobalRule struct {
	Plugins map[string]PluginConfig `json:"plugins,omitempty"`
}

type PluginConfigRule struct {
	Desc    string                  `json:"desc,omitempty"`
	Plugins map[string]PluginConfig `json:"plugins,omitempty"`
}
