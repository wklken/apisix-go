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

	// NGINX-only directives are retained for config compatibility; the Go server
	// applies the HTTP timeout settings that have direct net/http equivalents.
	Proxy Proxy `mapstructure:"proxy"`

	Discovery     Discovery `mapstructure:"discovery"`
	GraphQL       GraphQL   `mapstructure:"graphql"`
	ExtPlugin     ExtPlugin `mapstructure:"ext-plugin"`
	Wasm          Wasm      `mapstructure:"wasm"`
	XRPC          XRPC      `mapstructure:"xrpc"`
	Events        Events    `mapstructure:"events"`
	Plugins       []string  `mapstructure:"plugins"`
	StreamPlugins []string  `mapstructure:"stream_plugins"`
	// PluginAttr    PluginAttr `mapstructure:"plugin_attr"`
	PluginAttr map[string]map[string]any `mapstructure:"plugin_attr"`
	Deployment Deployment                `mapstructure:"deployment"`
}

// section: apisix

type Apisix struct {
	ID                                 string        `mapstructure:"id"`
	NodeListen                         []NodeListen  `mapstructure:"node_listen"`
	EnableAdmin                        bool          `mapstructure:"enable_admin"`
	EnableDevMode                      bool          `mapstructure:"enable_dev_mode"`
	EnableReuseport                    bool          `mapstructure:"enable_reuseport"`
	ShowUpstreamStatusInResponseHeader bool          `mapstructure:"show_upstream_status_in_response_header"`
	EnableIpv6                         bool          `mapstructure:"enable_ipv6"`
	EnableHttp2                        bool          `mapstructure:"enable_http2"`
	EnableServerTokens                 bool          `mapstructure:"enable_server_tokens"`
	ExtraLuaPath                       string        `mapstructure:"extra_lua_path"`
	ExtraLuaCpath                      string        `mapstructure:"extra_lua_cpath"`
	LuaModuleHook                      string        `mapstructure:"lua_module_hook"`
	ProxyProtocol                      ProxyProtocol `mapstructure:"proxy_protocol"`
	ProxyCache                         ProxyCache    `mapstructure:"proxy_cache"`
	DeleteURITailSlash                 bool          `mapstructure:"delete_uri_tail_slash"`
	NormalizeURILikeServlet            bool          `mapstructure:"normalize_uri_like_servlet"`
	MatchURIEncodedSlash               bool          `mapstructure:"match_uri_encoded_slash"`
	MaxPostArgsReadableSize            int           `mapstructure:"max_post_args_readable_size"`

	Router                              Router         `mapstructure:"router"`
	ProxyMode                           string         `mapstructure:"proxy_mode"`
	StreamProxy                         StreamProxy    `mapstructure:"stream_proxy"`
	DnsResolver                         []string       `mapstructure:"dns_resolver"`
	DnsResolverValid                    int            `mapstructure:"dns_resolver_valid"`
	ResolverTimeout                     int            `mapstructure:"resolver_timeout"`
	EnableResolvSearchOpt               bool           `mapstructure:"enable_resolv_search_opt"`
	Ssl                                 Ssl            `mapstructure:"ssl"`
	EnableControl                       bool           `mapstructure:"enable_control"`
	Control                             Control        `mapstructure:"control"`
	DisableSyncConfigurationDuringStart bool           `mapstructure:"disable_sync_configuration_during_start"`
	WorkerStartupTimeThreshold          int            `mapstructure:"worker_startup_time_threshold"`
	DataEncryption                      DataEncryption `mapstructure:"data_encryption"`
	LRU                                 LRU            `mapstructure:"lru"`
	Tracing                             bool           `mapstructure:"tracing"`
	Status                              Status         `mapstructure:"status"`
	DisableUpstreamHealthcheck          bool           `mapstructure:"disable_upstream_healthcheck"`
	TrustedAddresses                    []string       `mapstructure:"trusted_addresses"`
}

type ProxyProtocol struct {
	ListenHTTPPort        int  `mapstructure:"listen_http_port"`
	ListenHTTPSPort       int  `mapstructure:"listen_https_port"`
	EnableTCPPP           bool `mapstructure:"enable_tcp_pp"`
	EnableTCPPPToUpstream bool `mapstructure:"enable_tcp_pp_to_upstream"`
}

type ProxyCache struct {
	CacheTtl time.Duration `mapstructure:"cache_ttl"`
	Zones    []Zone        `mapstructure:"zones"`
}

type Zone struct {
	Name        string `mapstructure:"name"`
	MemorySize  string `mapstructure:"memory_size"`
	DiskSize    string `mapstructure:"disk_size"`
	DiskPath    string `mapstructure:"disk_path"`
	CacheLevels string `mapstructure:"cache_levels"`
}

type NodeListen struct {
	Ip                      string `mapstructure:"ip"`
	Port                    int    `mapstructure:"port"`
	EnableHttp2             bool   `mapstructure:"enable_http2"`
	ProxyProtocol           bool   `mapstructure:"proxy_protocol"`
	ProxyProtocolToUpstream bool   `mapstructure:"proxy_protocol_to_upstream"`
}

type Router struct {
	Http string `mapstructure:"http"`
	Ssl  string `mapstructure:"ssl"`
}

type StreamProxy struct {
	Tcp []TcpListen `mapstructure:"tcp"`
	Udp []string    `mapstructure:"udp"`
}

type TcpListen struct {
	Addr                    string `mapstructure:"addr"`
	Tls                     bool   `mapstructure:"tls"`
	ProxyProtocol           bool   `mapstructure:"proxy_protocol"`
	ProxyProtocolToUpstream bool   `mapstructure:"proxy_protocol_to_upstream"`
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
	EnableHttp2 bool   `mapstructure:"enable_http2"`
	EnableQuic  bool   `mapstructure:"enable_quic"`
	EnableHttp3 bool   `mapstructure:"enable_http3"`
}

type Control struct {
	Ip   string `mapstructure:"ip"`
	Port int    `mapstructure:"port"`
}

type DataEncryption struct {
	EnableEncryptFields bool     `mapstructure:"enable_encrypt_fields"`
	Keyring             []string `mapstructure:"keyring"`
}

type LRU struct {
	Secret LRUCache `mapstructure:"secret"`
}

type LRUCache struct {
	TTL      int `mapstructure:"ttl"`
	Count    int `mapstructure:"count"`
	NegTTL   int `mapstructure:"neg_ttl"`
	NegCount int `mapstructure:"neg_count"`
}

type Status struct {
	IP   string `mapstructure:"ip"`
	Port int    `mapstructure:"port"`
}

// section: proxy
type Proxy struct {
	// keepalive_timeout: 60s
	// client_header_timeout: 60s
	DialerTimeout   int `mapstructure:"dialer_timeout"`
	DialerKeepAlive int `mapstructure:"dialer_keep_alive"`

	IdleConnTimeout       int `mapstructure:"idle_conn_timeout"`
	TLSHandshakeTimeout   int `mapstructure:"tls_handshake_timeout"`
	ExpectContinueTimeout int `mapstructure:"expect_continue_timeout"`
	ResponseHeaderTimeout int `mapstructure:"response_header_timeout"`
	MaxIdleConnsPerHost   int `mapstructure:"max_idle_conns_per_host"`

	// TODO:
	// keepalive_timeout
	// client_header_timeout
	// client_body_timeout
	// send_timeout
	// client_max_body_size
	// underscores_in_headers
	// real_ip_header
	// real_ip_recursive
	// real_ip_from
	// proxy_ssl_server_name
	// charset
	// upstream.keepalive / keepalive_requests / keepalive_timeout
	// variables_hash_max_size
}

type NginxConfig struct {
	ErrorLog                               string        `mapstructure:"error_log"`
	ErrorLogLevel                          string        `mapstructure:"error_log_level"`
	WorkerProcesses                        string        `mapstructure:"worker_processes"`
	EnableCPUAffinity                      bool          `mapstructure:"enable_cpu_affinity"`
	WorkerRlimitNofile                     int           `mapstructure:"worker_rlimit_nofile"`
	WorkerShutdownTimeout                  time.Duration `mapstructure:"worker_shutdown_timeout"`
	MaxPendingTimers                       int           `mapstructure:"max_pending_timers"`
	MaxRunningTimers                       int           `mapstructure:"max_running_timers"`
	Event                                  NginxEvent    `mapstructure:"event"`
	Meta                                   NginxMeta     `mapstructure:"meta"`
	Stream                                 NginxStream   `mapstructure:"stream"`
	MainConfigurationSnippet               string        `mapstructure:"main_configuration_snippet"`
	HTTPConfigurationSnippet               string        `mapstructure:"http_configuration_snippet"`
	HTTPServerConfigurationSnippet         string        `mapstructure:"http_server_configuration_snippet"`
	HTTPServerLocationConfigurationSnippet string        `mapstructure:"http_server_location_configuration_snippet"`
	HTTPAdminConfigurationSnippet          string        `mapstructure:"http_admin_configuration_snippet"`
	HTTPEndConfigurationSnippet            string        `mapstructure:"http_end_configuration_snippet"`
	StreamConfigurationSnippet             string        `mapstructure:"stream_configuration_snippet"`
	HTTP                                   NginxHTTP     `mapstructure:"http"`
}

type NginxEvent struct {
	WorkerConnections int `mapstructure:"worker_connections"`
}

type NginxMeta struct {
	LuaSharedDict map[string]string `mapstructure:"lua_shared_dict"`
}

type NginxStream struct {
	EnableAccessLog       bool              `mapstructure:"enable_access_log"`
	AccessLog             string            `mapstructure:"access_log"`
	AccessLogFormat       string            `mapstructure:"access_log_format"`
	AccessLogFormatEscape string            `mapstructure:"access_log_format_escape"`
	LuaSharedDict         map[string]string `mapstructure:"lua_shared_dict"`
}

type NginxHTTP struct {
	EnableAccessLog       bool              `mapstructure:"enable_access_log"`
	AccessLog             string            `mapstructure:"access_log"`
	AccessLogBuffer       int               `mapstructure:"access_log_buffer"`
	AccessLogFormat       string            `mapstructure:"access_log_format"`
	AccessLogFormatEscape string            `mapstructure:"access_log_format_escape"`
	KeepaliveTimeout      time.Duration     `mapstructure:"keepalive_timeout"`
	ClientHeaderTimeout   time.Duration     `mapstructure:"client_header_timeout"`
	ClientBodyTimeout     time.Duration     `mapstructure:"client_body_timeout"`
	ClientMaxBodySize     int64             `mapstructure:"client_max_body_size"`
	SendTimeout           time.Duration     `mapstructure:"send_timeout"`
	UnderscoresInHeaders  string            `mapstructure:"underscores_in_headers"`
	RealIPHeader          string            `mapstructure:"real_ip_header"`
	RealIPRecursive       string            `mapstructure:"real_ip_recursive"`
	RealIPFrom            []string          `mapstructure:"real_ip_from"`
	ProxySSLServerName    bool              `mapstructure:"proxy_ssl_server_name"`
	Upstream              NginxUpstream     `mapstructure:"upstream"`
	Charset               string            `mapstructure:"charset"`
	VariablesHashMaxSize  int               `mapstructure:"variables_hash_max_size"`
	LuaSharedDict         map[string]string `mapstructure:"lua_shared_dict"`
	CustomLuaSharedDict   map[string]string `mapstructure:"custom_lua_shared_dict"`
	IPCSharedDict         map[string]string `mapstructure:"ipc_shared_dict"`
}

type NginxUpstream struct {
	Keepalive         int           `mapstructure:"keepalive"`
	KeepaliveRequests int           `mapstructure:"keepalive_requests"`
	KeepaliveTimeout  time.Duration `mapstructure:"keepalive_timeout"`
}

type ExtPlugin struct {
	Cmd []string `mapstructure:"cmd"`
}

type Wasm struct {
	Plugins []WasmPlugin `mapstructure:"plugins"`
}

type WasmPlugin struct {
	Name     string `mapstructure:"name"`
	Priority int    `mapstructure:"priority"`
	File     string `mapstructure:"file"`
}

type XRPC struct {
	Protocols []XRPCProtocol `mapstructure:"protocols"`
}

type XRPCProtocol struct {
	Name string `mapstructure:"name"`
}

type Events struct {
	Module string `mapstructure:"module"`
}

// section: plugin_attr

type PluginAttr map[string]any

// section: deployment

type Deployment struct {
	// TODO: add validation here
	Role             string                `mapstructure:"role"`
	RoleTraditional  RoleTraditionalConfig `mapstructure:"role_traditional"`
	RoleDataPlane    RoleConfig            `mapstructure:"role_data_plane"`
	RoleControlPlane RoleConfig            `mapstructure:"role_control_plane"`
	Admin            Admin                 `mapstructure:"admin"`
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

// section: deployment.admin

type Admin struct {
	AdminKeyRequired bool         `mapstructure:"admin_key_required"`
	AdminKey         []AdminKey   `mapstructure:"admin_key"`
	EnableAdminCORS  bool         `mapstructure:"enable_admin_cors"`
	EnableAdminUI    bool         `mapstructure:"enable_admin_ui"`
	AllowAdmin       []string     `mapstructure:"allow_admin"`
	AdminListen      AdminListen  `mapstructure:"admin_listen"`
	HTTPSAdmin       bool         `mapstructure:"https_admin"`
	AdminAPIMTLS     AdminAPIMTLS `mapstructure:"admin_api_mtls"`
	AdminAPIVersion  string       `mapstructure:"admin_api_version"`
}

type AdminKey struct {
	Name string `mapstructure:"name"`
	Key  string `mapstructure:"key"`
	Role string `mapstructure:"role"`
}

type AdminListen struct {
	IP   string `mapstructure:"ip"`
	Port int    `mapstructure:"port"`
}

type AdminAPIMTLS struct {
	AdminSSLCert    string `mapstructure:"admin_ssl_cert"`
	AdminSSLCertKey string `mapstructure:"admin_ssl_cert_key"`
	AdminSSLCA      string `mapstructure:"admin_ssl_ca_cert"`
}

// section: deployment.etcd

type Etcd struct {
	Host   []string `mapstructure:"host"`
	Prefix string   `mapstructure:"prefix"`

	// TODO: not support yet
	Timeout            int `mapstructure:"timeout"`
	WatchTimeout       int `mapstructure:"watch_timeout"`
	ResyncDelay        int `mapstructure:"resync_delay"`
	HealthCheckTimeout int `mapstructure:"health_check_timeout"`
	StartupRetry       int `mapstructure:"startup_retry"`

	User     string `mapstructure:"user"`
	Password string `mapstructure:"password"`

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
