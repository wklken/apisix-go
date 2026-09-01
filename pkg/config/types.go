package config

import (
	"net"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Debug       bool        `mapstructure:"debug"`
	Apisix      Apisix      `mapstructure:"apisix"`
	NginxConfig NginxConfig `mapstructure:"nginx_config"`

	Discovery     Discovery `mapstructure:"discovery" secret:"container"`
	GraphQL       GraphQL   `mapstructure:"graphql"`
	ExtPlugin     ExtPlugin `mapstructure:"ext-plugin"`
	Wasm          Wasm      `mapstructure:"wasm"`
	XRPC          XRPC      `mapstructure:"xrpc"`
	Plugins       []string  `mapstructure:"plugins"`
	StreamPlugins []string  `mapstructure:"stream_plugins"`
	// PluginAttr    PluginAttr `mapstructure:"plugin_attr"`
	PluginAttr map[string]map[string]any `mapstructure:"plugin_attr" secret:"container"`
	Deployment Deployment                `mapstructure:"deployment"`
}

// section: apisix

type Apisix struct {
	ID                                 string        `mapstructure:"id"`
	NodeListen                         []NodeListen  `mapstructure:"node_listen"`
	EnableAdmin                        bool          `mapstructure:"enable_admin"`
	ShowUpstreamStatusInResponseHeader bool          `mapstructure:"show_upstream_status_in_response_header"`
	EnableHttp2                        bool          `mapstructure:"enable_http2"`
	EnableServerTokens                 bool          `mapstructure:"enable_server_tokens"`
	ProxyProtocol                      ProxyProtocol `mapstructure:"proxy_protocol"`
	ProxyCache                         ProxyCache    `mapstructure:"proxy_cache"`
	DeleteURITailSlash                 bool          `mapstructure:"delete_uri_tail_slash"`
	NormalizeURILikeServlet            bool          `mapstructure:"normalize_uri_like_servlet"`

	ProxyMode                  string         `mapstructure:"proxy_mode"`
	StreamProxy                StreamProxy    `mapstructure:"stream_proxy"`
	DnsResolver                []string       `mapstructure:"dns_resolver"`
	DnsResolverValid           int            `mapstructure:"dns_resolver_valid"`
	ResolverTimeout            int            `mapstructure:"resolver_timeout"`
	Ssl                        Ssl            `mapstructure:"ssl"`
	EnableControl              bool           `mapstructure:"enable_control"`
	Control                    Control        `mapstructure:"control"`
	DataEncryption             DataEncryption `mapstructure:"data_encryption"`
	Status                     Status         `mapstructure:"status"`
	DisableUpstreamHealthcheck bool           `mapstructure:"disable_upstream_healthcheck"`
	TrustedAddresses           []string       `mapstructure:"trusted_addresses"`
}

type ProxyProtocol struct {
	EnableTCPPP           bool `mapstructure:"enable_tcp_pp"`
	EnableTCPPPToUpstream bool `mapstructure:"enable_tcp_pp_to_upstream"`
}

type ProxyCache struct {
	Zones []Zone `mapstructure:"zones"`
}

type Zone struct {
	Name        string `mapstructure:"name"`
	MemorySize  string `mapstructure:"memory_size"`
	DiskSize    string `mapstructure:"disk_size"`
	DiskPath    string `mapstructure:"disk_path"`
	CacheLevels string `mapstructure:"cache_levels"`
}

type NodeListen struct {
	Ip   string `mapstructure:"ip"`
	Port int    `mapstructure:"port"`
}

type StreamProxy struct {
	Tcp []TcpListen `mapstructure:"tcp"`
	Udp []string    `mapstructure:"udp"`
}

type TcpListen struct {
	Addr string `mapstructure:"addr"`
	Tls  bool   `mapstructure:"tls"`
}

type Ssl struct {
	Enable                bool     `mapstructure:"enable"`
	Listen                []Listen `mapstructure:"listen"`
	SslTrustedCertificate string   `mapstructure:"ssl_trusted_certificate"`
	SslProtocols          string   `mapstructure:"ssl_protocols"`
	SslCiphers            string   `mapstructure:"ssl_ciphers"`
	SslSessionTickets     bool     `mapstructure:"ssl_session_tickets"`
	FallbackSNI           string   `mapstructure:"fallback_sni"`
}

type Listen struct {
	Ip          string `mapstructure:"ip"`
	Port        int    `mapstructure:"port"`
	EnableHttp3 bool   `mapstructure:"enable_http3"`
}

type Control struct {
	Ip   string `mapstructure:"ip"`
	Port int    `mapstructure:"port"`
}

type DataEncryption struct {
	EnableEncryptFields bool     `mapstructure:"enable_encrypt_fields"`
	Keyring             []string `mapstructure:"keyring" secret:"true"`
}

type Status struct {
	IP   string `mapstructure:"ip"`
	Port int    `mapstructure:"port"`
}

type NginxConfig struct {
	ErrorLog      string    `mapstructure:"error_log"`
	ErrorLogLevel string    `mapstructure:"error_log_level"`
	HTTP          NginxHTTP `mapstructure:"http"`
}

type NginxHTTP struct {
	EnableAccessLog     bool          `mapstructure:"enable_access_log"`
	AccessLog           string        `mapstructure:"access_log"`
	KeepaliveTimeout    time.Duration `mapstructure:"keepalive_timeout"`
	ClientHeaderTimeout time.Duration `mapstructure:"client_header_timeout"`
	ClientBodyTimeout   time.Duration `mapstructure:"client_body_timeout"`
	ClientMaxBodySize   int64         `mapstructure:"client_max_body_size"`
	SendTimeout         time.Duration `mapstructure:"send_timeout"`
}

type ExtPlugin struct {
	Cmd []string `mapstructure:"cmd"`
}

type Wasm struct {
	Plugins []WasmPlugin `mapstructure:"plugins"`
}

type WasmPlugin struct{}

type XRPC struct {
	Protocols []XRPCProtocol `mapstructure:"protocols"`
}

type XRPCProtocol struct{}

// section: plugin_attr

type PluginAttr map[string]any

// section: deployment

type Deployment struct {
	// TODO: add validation here
	Role             string                `mapstructure:"role"`
	RoleTraditional  RoleTraditionalConfig `mapstructure:"role_traditional"`
	RoleDataPlane    RoleConfig            `mapstructure:"role_data_plane"`
	RoleControlPlane RoleConfig            `mapstructure:"role_control_plane"`
	Etcd             Etcd                  `mapstructure:"etcd"`
}

type RoleConfig struct {
	ConfigProvider string `mapstructure:"config_provider"`
}

type RoleTraditionalConfig struct {
	ConfigProvider string `mapstructure:"config_provider"`
}

// section: discovery

type Discovery map[string]any

// section: graphql

type GraphQL struct {
	MaxSize int `mapstructure:"max_size"`
}

// section: deployment.etcd

type Etcd struct {
	Host   []string `mapstructure:"host" secret:"url-userinfo"`
	Prefix string   `mapstructure:"prefix"`

	// TODO: not support yet
	Timeout            int `mapstructure:"timeout"`
	WatchTimeout       int `mapstructure:"watch_timeout"`
	ResyncDelay        int `mapstructure:"resync_delay"`
	HealthCheckTimeout int `mapstructure:"health_check_timeout"`
	StartupRetry       int `mapstructure:"startup_retry"`

	User     string `mapstructure:"user"`
	Password string `mapstructure:"password" secret:"true"`

	// TODO: not support yet
	TLS EtcdTLS `mapstructure:"tls"`
}

type EtcdTLS struct {
	Cert   string `mapstructure:"cert"`
	Key    string `mapstructure:"key"`
	Verify *bool  `mapstructure:"verify"`
	SNI    string `mapstructure:"sni"`
}

func (a Apisix) ListenAddresses() []string {
	addresses := make([]string, 0, len(a.NodeListen))
	for _, listen := range a.NodeListen {
		if listen.Port < 1 || listen.Port > 65535 {
			continue
		}
		host := strings.TrimSpace(listen.Ip)
		if host == "" {
			host = "0.0.0.0"
		}
		addresses = append(addresses, net.JoinHostPort(host, strconv.Itoa(listen.Port)))
	}
	if len(addresses) == 0 {
		return []string{":8080"}
	}
	return addresses
}
