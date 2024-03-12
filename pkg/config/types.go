package config

import "time"

type Config struct {
	Debug  bool   `mapstructure:"debug"`
	Apisix Apisix `mapstructure:"apisix"`

	// NOTE: the section nginx_config is not supported in this version
	// use proxy instead
	Proxy Proxy `mapstructure:"proxy"`

	Discovery     Discovery  `mapstructure:"discovery"`
	GraphQL       GraphQL    `mapstructure:"graphql"`
	Plugins       []string   `mapstructure:"plugins"`
	StreamPlugins []string   `mapstructure:"stream_plugins"`
	PluginAttr    PluginAttr `mapstructure:"plugin_attr"`
	Deployment    Deployment `mapstructure:"deployment"`
	Admin         Admin      `mapstructure:"admin"`
	Etcd          Etcd       `mapstructure:"etcd"`
}

// section: apisix

type Apisix struct {
	NodeListen                         []NodeListen `mapstructure:"node_listen"`
	EnableAdmin                        bool         `mapstructure:"enable_admin"`
	EnableDevMode                      bool         `mapstructure:"enable_dev_mode"`
	EnableReuseport                    bool         `mapstructure:"enable_reuseport"`
	ShowUpstreamStatusInResponseHeader bool         `mapstructure:"show_upstream_status_in_response_header"`
	EnableIpv6                         bool         `mapstructure:"enable_ipv6"`
	EnableServerTokens                 bool         `mapstructure:"enable_server_tokens"`
	ProxyCache                         ProxyCache   `mapstructure:"proxy_cache"`

	// NOTE: no need extra_lua_path/extra_lua_cpath

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
	DataEncryption                      DataEncryption `mapstructure:"data_encryption"`
	// NOTE: no need `events` here
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
	Ip          string `mapstructure:"ip"`
	Port        int    `mapstructure:"port"`
	EnableHttp2 bool   `mapstructure:"enable_http2"`
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
	Addr string `mapstructure:"addr"`
	Tls  bool   `mapstructure:"tls"`
}

type Ssl struct {
	Enable                bool     `mapstructure:"enable"`
	Listen                []Listen `mapstructure:"listen"`
	SslTrustedCertificate string   `mapstructure:"ssl_trusted_certificate"`
	SslProtocols          []string `mapstructure:"ssl_protocols"`
	SslCiphers            string   `mapstructure:"ssl_ciphers"`
	SslSessionTickets     bool     `mapstructure:"ssl_session_tickets"`
}

type Listen struct {
	Port        int  `mapstructure:"port"`
	EnableHttp2 bool `mapstructure:"enable_http2"`
	EnableQuic  bool `mapstructure:"enable_quic"`
}

type Control struct {
	Ip   string `mapstructure:"ip"`
	Port int    `mapstructure:"port"`
}

type DataEncryption struct {
	EnableEncryptFields bool     `mapstructure:"enable_encrypt_fields"`
	Keyring             []string `mapstructure:"keyring"`
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

// section: plugin_attr

type PluginAttr map[string]interface{}

// section: deployment

type Deployment struct {
	// TODO: add validation here
	Role            string                `mapstructure:"role"`
	RoleTraditional RoleTraditionalConfig `mapstructure:"role_traditional"`
	// TODO: role_data_plane, role_control_plane
}

type RoleTraditionalConfig struct {
	ConfigProvider string `mapstructure:"config_provider"`
}

// section: discovery

type Discovery map[string]interface{}

// section: graphql

type GraphQL struct {
	MaxSize int `mapstructure:"max_size"`
}

// section: deployment.admin

type Admin struct {
	AdminKeyRequired bool        `mapstructure:"admin_key_required"`
	AdminKey         []AdminKey  `mapstructure:"admin_key"`
	EnableAdminCORS  bool        `mapstructure:"enable_admin_cors"`
	AllowAdmin       []string    `mapstructure:"allow_admin"`
	AdminListen      AdminListen `mapstructure:"admin_listen"`
	AdminAPIVersion  string      `mapstructure:"admin_api_version"`
	// TODO: https_admin, admin_api_mtls
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

// section: deployment.etcd

type Etcd struct {
	Host               []string `mapstructure:"host"`
	Prefix             string   `mapstructure:"prefix"`
	Timeout            int      `mapstructure:"timeout"`
	WatchTimeout       int      `mapstructure:"watch_timeout"`
	ResyncDelay        int      `mapstructure:"resync_delay"`
	HealthCheckTimeout int      `mapstructure:"health_check_timeout"`
	StartupRetry       int      `mapstructure:"startup_retry"`
	User               string   `mapstructure:"user"`
	Password           string   `mapstructure:"password"`
	TLS                EtcdTLS  `mapstructure:"tls"`
}

type EtcdTLS struct {
	Cert   string `mapstructure:"cert"`
	Key    string `mapstructure:"key"`
	Verify bool   `mapstructure:"verify"`
	// TODO: sni
}
