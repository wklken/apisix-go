package limit_conn

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/limitbase"
	"github.com/wklken/apisix-go/pkg/shared"
)

type redisConnLimiter struct {
	mu                  sync.Mutex
	client              redis.UniversalClient
	unitDelay           float64
	keyTTL              time.Duration
	onlyUseDefaultDelay bool
}

type redisPoolStatsProvider interface {
	PoolStats() *redis.PoolStats
}

func logRedisConnectionReuse(client redisPoolStatsProvider) {
	if logger.DebugEnabled() {
		logger.Debugf("redis connection reused times: %d", client.PoolStats().Hits)
	}
}

func (p *Plugin) newRedisLimiter() connLimiter {
	configUID := shared.NewConfigUID()
	configUID.Add(
		p.config.RedisHost,
		p.config.RedisPort,
		p.config.RedisUsername,
		p.config.RedisPassword,
		p.config.RedisDatabase,
		p.config.RedisTimeout,
		*p.config.RedisSSL,
		*p.config.RedisSSLVerify,
		p.config.RedisKeepaliveTimeout,
		p.config.RedisKeepalivePool,
	)
	value, release, err := shared.AcquireClient(
		shared.ClientKey(name, configUID),
		func() (any, error) {
			return redis.NewClient(p.redisConnConfig().Options()), nil
		},
		shared.CloseRedisClient,
	)
	if err != nil {
		return nil
	}
	p.clientRelease = release
	return p.newRedisConnLimiter(value.(redis.UniversalClient))
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

func (p *Plugin) newRedisClusterLimiter() connLimiter {
	configUID := shared.NewConfigUID()
	configUID.Add(
		p.config.RedisClusterName,
		strings.Join(p.config.RedisClusterNodes, ","),
		p.config.RedisPassword,
		p.config.RedisTimeout,
		*p.config.RedisClusterSSL,
		*p.config.RedisClusterSSLVerify,
		p.config.RedisKeepaliveTimeout,
		p.config.RedisKeepalivePool,
	)
	value, release, err := shared.AcquireClient(
		shared.ClientKey(name, configUID),
		func() (any, error) {
			return redis.NewClusterClient(p.redisClusterConnConfig().ClusterOptions()), nil
		},
		shared.CloseRedisClient,
	)
	if err != nil {
		return nil
	}
	p.clientRelease = release
	return p.newRedisConnLimiter(value.(redis.UniversalClient))
}

func (p *Plugin) newRedisConnLimiter(client redis.UniversalClient) connLimiter {
	return &redisConnLimiter{
		client:              client,
		unitDelay:           p.config.DefaultConnDelay,
		keyTTL:              time.Duration(p.config.RedisKeyTTL) * time.Second,
		onlyUseDefaultDelay: p.config.OnlyUseDefaultDelay,
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

func (l *redisConnLimiter) incoming(key string, conn int, burst int) (time.Duration, bool, error) {
	l.mu.Lock()
	unitDelay := l.unitDelay
	l.mu.Unlock()
	logRedisConnectionReuse(l.client)
	result, err := l.client.Eval(
		context.Background(),
		redisLimitConnIncomingScript,
		[]string{"plugin-limit-conn:" + key},
		conn,
		burst,
		unitDelay,
		l.keyTTL.Milliseconds(),
	).Result()
	if err != nil {
		return 0, false, err
	}

	values, ok := result.([]any)
	if !ok || len(values) != 2 {
		return 0, false, fmt.Errorf("unexpected redis limit-conn result: %v", result)
	}
	allowed, ok := limitbase.RedisInt(values[0])
	if !ok {
		return 0, false, fmt.Errorf("unexpected redis limit-conn allowed value: %v", values[0])
	}
	delayMs, ok := limitbase.RedisInt(values[1])
	if !ok {
		return 0, false, fmt.Errorf("unexpected redis limit-conn delay value: %v", values[1])
	}

	return time.Duration(delayMs) * time.Millisecond, allowed == 1, nil
}

func (l *redisConnLimiter) leaving(key string, latency *time.Duration) error {
	logRedisConnectionReuse(l.client)
	err := l.client.Eval(
		context.Background(),
		redisLimitConnLeavingScript,
		[]string{"plugin-limit-conn:" + key},
	).Err()
	if err != nil || latency == nil || l.onlyUseDefaultDelay {
		return err
	}
	l.mu.Lock()
	l.unitDelay = (l.unitDelay + latency.Seconds()) / 2
	l.mu.Unlock()
	return nil
}
