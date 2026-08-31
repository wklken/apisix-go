package limit_count

import (
	"context"
	"net"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	limiter "github.com/ulule/limiter/v3"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
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

type RedisSentinel struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

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
