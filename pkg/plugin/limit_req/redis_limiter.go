package limit_req

import (
	"context"
	"fmt"
	"math"
	"strconv"
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
	return &redisReqLimiter{client: value.(redis.UniversalClient), now: time.Now}
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
	return &redisReqLimiter{client: value.(redis.UniversalClient), now: time.Now}
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
	ttl := redisLimitReqTTL(rate, burst)
	now := l.now
	if now == nil {
		now = time.Now
	}

	result, err := l.client.Eval(
		context.Background(),
		redisLimitReqScript,
		[]string{redisStateKey(key)},
		rate*1000,
		burst*1000,
		now().UnixMilli(),
		int64(ttl/time.Second),
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
	excess, ok := redisFloat(values[1])
	if !ok {
		return 0, false, fmt.Errorf("unexpected redis limit-req excess value: %v", values[1])
	}
	return time.Duration(excess / (rate * 1000) * float64(time.Second)), allowed == 1, nil
}

func redisLimitReqTTL(rate float64, burst float64) time.Duration {
	return time.Duration(math.Ceil(burst/rate)+1) * time.Second
}

func redisStateKey(key string) string {
	return "limit_req:" + key
}

func redisFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case string:
		result, err := strconv.ParseFloat(typed, 64)
		return result, err == nil
	case []byte:
		result, err := strconv.ParseFloat(string(typed), 64)
		return result, err == nil
	default:
		integer, ok := limitbase.RedisInt(value)
		return float64(integer), ok
	}
}
