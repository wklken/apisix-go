package limit_count

import (
	"context"
	"github.com/redis/go-redis/v9"
	limiter "github.com/ulule/limiter/v3"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/util"
	"net"
	"strconv"
	"time"
)

type redisPoolStatsProvider interface {
	PoolStats() *redis.PoolStats
}

type redisDiagnosticStore struct {
	limiter.Store
	client       redisPoolStatsProvider
	baselineHits uint32
}

func newRedisDiagnosticStore(store limiter.Store, client redisPoolStatsProvider) limiter.Store {
	return &redisDiagnosticStore{
		Store:        store,
		client:       client,
		baselineHits: client.PoolStats().Hits,
	}
}

func (s *redisDiagnosticStore) logConnectionReuse() {
	if logger.DebugEnabled() {
		logger.Debugf("redis connection reused times: %d", s.client.PoolStats().Hits-s.baselineHits)
	}
}

func (s *redisDiagnosticStore) Get(
	ctx context.Context,
	key string,
	rate limiter.Rate,
) (limiter.Context, error) {
	s.logConnectionReuse()
	return s.Store.Get(ctx, key, rate)
}

func (s *redisDiagnosticStore) Peek(
	ctx context.Context,
	key string,
	rate limiter.Rate,
) (limiter.Context, error) {
	s.logConnectionReuse()
	return s.Store.Peek(ctx, key, rate)
}

func (s *redisDiagnosticStore) Increment(
	ctx context.Context,
	key string,
	count int64,
	rate limiter.Rate,
) (limiter.Context, error) {
	s.logConnectionReuse()
	return s.Store.Increment(ctx, key, count, rate)
}

func (s *redisDiagnosticStore) Reset(
	ctx context.Context,
	key string,
	rate limiter.Rate,
) (limiter.Context, error) {
	s.logConnectionReuse()
	return s.Store.Reset(ctx, key, rate)
}

type RedisConfig struct {
	RedisHost             string `json:"redis_host,omitempty"`
	RedisPort             int    `json:"redis_port,omitempty"`
	RedisUsername         string `json:"redis_username,omitempty"`
	RedisPassword         string `json:"redis_password,omitempty"`
	RedisDatabase         int    `json:"redis_database,omitempty"`
	RedisTimeout          int    `json:"redis_timeout,omitempty"`
	RedisSSL              *bool  `json:"redis_ssl,omitempty"`
	RedisSSLVerify        *bool  `json:"redis_ssl_verify,omitempty"`
	RedisKeepaliveTimeout int    `json:"redis_keepalive_timeout,omitempty"`
	RedisKeepalivePool    int    `json:"redis_keepalive_pool,omitempty"`
}

type RedisSentinel struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

func (rc *RedisConfig) String() string {
	c, _ := json.Marshal(rc)
	return util.BytesToString(c)
}

// RedisClusterConfig holds fields specific to the "redis-cluster" policy.
type RedisClusterConfig struct {
	RedisClusterNodes     []string `json:"redis_cluster_nodes,omitempty"`
	RedisPassword         string   `json:"redis_password,omitempty"`
	RedisTimeout          int      `json:"redis_timeout,omitempty"`
	RedisClusterName      string   `json:"redis_cluster_name,omitempty"`
	RedisClusterSSL       *bool    `json:"redis_cluster_ssl,omitempty"`
	RedisClusterSSLVerify *bool    `json:"redis_cluster_ssl_verify,omitempty"`
	RedisKeepaliveTimeout int      `json:"redis_keepalive_timeout,omitempty"`
	RedisKeepalivePool    int      `json:"redis_keepalive_pool,omitempty"`
}

func (p *Plugin) applyRootRedisConfig() {
	if p.config.Redis.RedisHost != "" {
		return
	}

	p.config.Redis.RedisHost = p.config.RedisHost
	p.config.Redis.RedisPort = p.config.RedisPort
	p.config.Redis.RedisUsername = p.config.RedisUsername
	p.config.Redis.RedisPassword = p.config.RedisPassword
	p.config.Redis.RedisDatabase = p.config.RedisDatabase
	p.config.Redis.RedisTimeout = p.config.RedisTimeout
	p.config.Redis.RedisSSL = p.config.RedisSSL
	p.config.Redis.RedisSSLVerify = p.config.RedisSSLVerify
	p.config.Redis.RedisKeepaliveTimeout = p.config.RedisKeepaliveTimeout
	p.config.Redis.RedisKeepalivePool = p.config.RedisKeepalivePool
}

func (p *Plugin) applyRootRedisClusterConfig() {
	if len(p.config.RedisCluster.RedisClusterNodes) > 0 {
		return
	}

	p.config.RedisCluster.RedisClusterNodes = append([]string(nil), p.config.RedisClusterNodes...)
	p.config.RedisCluster.RedisPassword = p.config.RedisPassword
	p.config.RedisCluster.RedisTimeout = p.config.RedisTimeout
	p.config.RedisCluster.RedisClusterName = p.config.RedisClusterName
	p.config.RedisCluster.RedisClusterSSL = p.config.RedisClusterSSL
	p.config.RedisCluster.RedisClusterSSLVerify = p.config.RedisClusterSSLVerify
	p.config.RedisCluster.RedisKeepaliveTimeout = p.config.RedisKeepaliveTimeout
	p.config.RedisCluster.RedisKeepalivePool = p.config.RedisKeepalivePool
}

func (p *Plugin) redisConnConfig() base.RedisConnConfig {
	conf := p.config.Redis
	return base.RedisConnConfig{
		Host:             conf.RedisHost,
		Port:             conf.RedisPort,
		Username:         conf.RedisUsername,
		Password:         conf.RedisPassword,
		Database:         conf.RedisDatabase,
		Timeout:          conf.RedisTimeout,
		KeepaliveTimeout: conf.RedisKeepaliveTimeout,
		KeepalivePool:    conf.RedisKeepalivePool,
		SSL:              conf.RedisSSL,
		SSLVerify:        conf.RedisSSLVerify,
	}
}

func (p *Plugin) redisSentinelOptions() *redis.FailoverOptions {
	addresses := make([]string, 0, len(p.config.RedisSentinels))
	for _, sentinel := range p.config.RedisSentinels {
		addresses = append(addresses, net.JoinHostPort(sentinel.Host, strconv.Itoa(sentinel.Port)))
	}
	return &redis.FailoverOptions{
		MasterName:       p.config.RedisMasterName,
		SentinelAddrs:    addresses,
		Username:         p.config.RedisUsername,
		Password:         p.config.RedisPassword,
		SentinelUsername: p.config.SentinelUsername,
		SentinelPassword: p.config.SentinelPassword,
		DB:               p.config.RedisDatabase,
		DialTimeout:      time.Duration(p.config.RedisTimeout) * time.Millisecond,
		ReadTimeout:      time.Duration(p.config.RedisTimeout) * time.Millisecond,
		WriteTimeout:     time.Duration(p.config.RedisTimeout) * time.Millisecond,
		ReplicaOnly:      p.config.RedisRole == "slave",
	}
}

func (p *Plugin) redisClusterConnConfig() base.RedisClusterConnConfig {
	conf := p.config.RedisCluster
	return base.RedisClusterConnConfig{
		Nodes:            conf.RedisClusterNodes,
		Password:         conf.RedisPassword,
		Timeout:          conf.RedisTimeout,
		KeepaliveTimeout: conf.RedisKeepaliveTimeout,
		KeepalivePool:    conf.RedisKeepalivePool,
		SSL:              conf.RedisClusterSSL,
		SSLVerify:        conf.RedisClusterSSLVerify,
	}
}
