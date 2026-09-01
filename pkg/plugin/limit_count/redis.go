package limit_count

import (
	"github.com/wklken/apisix-go/pkg/plugin/base"
)

func (p *Plugin) redisConnConfig() base.RedisConnConfig {
	return base.RedisConnConfig{
		Host:             p.config.RedisHost,
		Port:             p.config.RedisPort,
		Username:         p.config.RedisUsername,
		Password:         p.config.RedisPassword,
		Database:         p.config.RedisDatabase,
		Timeout:          p.config.RedisTimeout,
		KeepaliveTimeout: p.config.RedisKeepaliveTimeout,
		KeepalivePool:    p.config.RedisKeepalivePool,
		SSL:              p.config.RedisSSL,
		SSLVerify:        p.config.RedisSSLVerify,
	}
}

func (p *Plugin) redisClusterConnConfig() base.RedisClusterConnConfig {
	return base.RedisClusterConnConfig{
		Nodes:            p.config.RedisClusterNodes,
		Password:         p.config.RedisPassword,
		Timeout:          p.config.RedisTimeout,
		KeepaliveTimeout: p.config.RedisKeepaliveTimeout,
		KeepalivePool:    p.config.RedisKeepalivePool,
		SSL:              p.config.RedisClusterSSL,
		SSLVerify:        p.config.RedisClusterSSLVerify,
	}
}
