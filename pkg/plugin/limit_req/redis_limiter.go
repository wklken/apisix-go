package limit_req

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/limitbase"
	"github.com/wklken/apisix-go/pkg/shared"
)

type redisReqLimiter struct {
	client redis.UniversalClient
	now    func() time.Time
}

type redisPoolStatsProvider interface {
	PoolStats() *redis.PoolStats
}

func logRedisConnectionReuse(client redisPoolStatsProvider) {
	if logger.DebugEnabled() {
		logger.Debugf("redis connection reused times: %d", client.PoolStats().Hits)
	}
}

func (p *Plugin) newRedisLimiter() reqLimiter {
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
	return &redisReqLimiter{client: value.(redis.UniversalClient), now: p.now}
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

func (p *Plugin) newRedisClusterLimiter() reqLimiter {
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
	return &redisReqLimiter{client: value.(redis.UniversalClient), now: p.now}
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

func (l *redisReqLimiter) incoming(key string, rate float64, burst float64) (time.Duration, bool, error) {
	ttl := max(time.Duration(math.Ceil((burst+1)/rate))*time.Second, time.Second)
	now := l.now
	if now == nil {
		now = time.Now
	}

	result, err := l.client.Eval(
		context.Background(),
		redisLimitReqScript,
		[]string{"plugin-limit-req:" + key},
		now().UnixMilli(),
		rate,
		burst,
		ttl.Milliseconds(),
	).Result()
	logRedisConnectionReuse(l.client)
	if err != nil {
		return 0, false, err
	}

	values, ok := result.([]any)
	if !ok || len(values) != 2 {
		return 0, false, fmt.Errorf("unexpected redis limit-req result: %v", result)
	}
	allowed, ok := limitbase.RedisInt(values[0])
	if !ok {
		return 0, false, fmt.Errorf("unexpected redis limit-req allowed value: %v", values[0])
	}
	delayMs, ok := limitbase.RedisInt(values[1])
	if !ok {
		return 0, false, fmt.Errorf("unexpected redis limit-req delay value: %v", values[1])
	}

	return time.Duration(delayMs) * time.Millisecond, allowed == 1, nil
}
