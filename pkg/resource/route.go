package resource

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

type PluginConfig map[string]interface{}

//	"upstream": {
//		"type": "roundrobin",
//		"nodes": {
//			"127.0.0.1:1980": 1
//		}
//	}
type Upstream struct {
	Type    string  `json:"type,omitempty"`
	Nodes   []Node  `json:"nodes,omitempty"`
	Scheme  string  `json:"scheme,omitempty"`
	Timeout Timeout `json:"timeout,omitempty"`
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
	ServiceID   string                  `json:"service_id,omitempty"`
	UpstreamID  string                  `json:"upstream_id,omitempty"`
	Upstream    Upstream                `json:"upstream,omitempty"`
	Timeout     struct {
		Connect int `json:"connect,omitempty"`
		Send    int `json:"send,omitempty"`
		Read    int `json:"read,omitempty"`
	} `json:"timeout,omitempty"`
	FilterFunc string `json:"filter_func,omitempty"`
}
