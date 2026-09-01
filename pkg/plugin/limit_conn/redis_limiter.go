package limit_conn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	client              redis.UniversalClient
	unitDelay           float64
	keyTTL              time.Duration
	onlyUseDefaultDelay bool
	useEvalSHA          bool
	now                 func() time.Time
	newMemberID         func() (string, error)
}

var redisIncomingScriptState struct {
	sync.Mutex
	sha string
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
		useEvalSHA:          p.config.Policy == "redis",
		now:                 time.Now,
		newMemberID:         randomConnMemberID,
	}
}

func randomConnMemberID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate limit-conn reservation member: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
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

func (l *redisConnLimiter) incoming(key string, conn int, burst int) (time.Duration, string, bool, error) {
	now := l.now
	if now == nil {
		now = time.Now
	}
	newMemberID := l.newMemberID
	if newMemberID == nil {
		newMemberID = randomConnMemberID
	}
	member, err := newMemberID()
	if err != nil {
		return 0, "", false, err
	}
	logRedisConnectionReuse(l.client)
	result, err := l.runIncomingScript(
		context.Background(),
		[]string{"limit_conn:" + key},
		conn+burst,
		int64(l.keyTTL/time.Second),
		now().Unix(),
		member,
	)
	if err != nil {
		return 0, "", false, err
	}

	values, ok := result.([]any)
	if !ok || len(values) != 2 {
		return 0, "", false, fmt.Errorf("unexpected redis limit-conn result: %v", result)
	}
	allowed, ok := limitbase.RedisInt(values[0])
	if !ok {
		return 0, "", false, fmt.Errorf("unexpected redis limit-conn allowed value: %v", values[0])
	}
	current, ok := limitbase.RedisInt(values[1])
	if !ok {
		return 0, "", false, fmt.Errorf("unexpected redis limit-conn count value: %v", values[1])
	}

	if allowed != 1 {
		return 0, "", false, nil
	}
	delay := time.Duration(0)
	if current > int64(conn) {
		delay = connectionDelay(int(current), conn, l.unitDelay)
	}
	return delay, member, true, nil
}

func (l *redisConnLimiter) leaving(key string, member string, latency *time.Duration) error {
	logRedisConnectionReuse(l.client)
	err := l.client.Eval(
		context.Background(),
		redisLimitConnLeavingScript,
		[]string{"limit_conn:" + key},
		member,
	).Err()
	return err
}

func (l *redisConnLimiter) runIncomingScript(
	ctx context.Context,
	keys []string,
	args ...any,
) (any, error) {
	if !l.useEvalSHA {
		return l.client.Eval(ctx, redisLimitConnIncomingScript, keys, args...).Result()
	}

	redisIncomingScriptState.Lock()
	sha := redisIncomingScriptState.sha
	redisIncomingScriptState.Unlock()
	if sha == "" {
		loaded, err := l.client.ScriptLoad(ctx, redisLimitConnIncomingScript).Result()
		if err != nil {
			return nil, err
		}
		sha = loaded
		redisIncomingScriptState.Lock()
		redisIncomingScriptState.sha = sha
		redisIncomingScriptState.Unlock()
	}

	result, err := l.client.EvalSha(ctx, sha, keys, args...).Result()
	if err == nil || !strings.HasPrefix(strings.ToUpper(err.Error()), "NOSCRIPT") {
		return result, err
	}
	redisIncomingScriptState.Lock()
	if redisIncomingScriptState.sha == sha {
		redisIncomingScriptState.sha = ""
	}
	redisIncomingScriptState.Unlock()
	return l.client.Eval(ctx, redisLimitConnIncomingScript, keys, args...).Result()
}
