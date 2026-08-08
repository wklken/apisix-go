package base

import (
	"crypto/tls"
	"net"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisConnConfig is the narrow standalone Redis connection configuration
// shared by the rate-limit plugins.
type RedisConnConfig struct {
	Host, Username, Password                                 string
	Port, Database, Timeout, KeepaliveTimeout, KeepalivePool int
	SSL, SSLVerify                                           *bool
}

// Options builds a standalone redis.Options. Timeout and KeepaliveTimeout are
// in milliseconds, matching the rate-limit plugin configuration.
func (c RedisConnConfig) Options() *redis.Options {
	options := &redis.Options{
		Addr:         net.JoinHostPort(c.Host, strconv.Itoa(c.Port)),
		Username:     c.Username,
		Password:     c.Password,
		DB:           c.Database,
		DialTimeout:  time.Duration(c.Timeout) * time.Millisecond,
		ReadTimeout:  time.Duration(c.Timeout) * time.Millisecond,
		WriteTimeout: time.Duration(c.Timeout) * time.Millisecond,
		PoolSize:     c.KeepalivePool,
	}
	if c.KeepaliveTimeout > 0 {
		options.ConnMaxIdleTime = time.Duration(c.KeepaliveTimeout) * time.Millisecond
	}
	if c.SSL != nil && *c.SSL {
		verify := c.SSLVerify == nil || *c.SSLVerify
		tlsConfig := &tls.Config{InsecureSkipVerify: !verify} //nolint:gosec // explicit operator configuration only
		options.TLSConfig = tlsConfig
	}
	return options
}

// RedisClusterConnConfig is the narrow redis-cluster connection configuration
// shared by the rate-limit plugins.
type RedisClusterConnConfig struct {
	Nodes                                    []string
	Password                                 string
	Timeout, KeepaliveTimeout, KeepalivePool int
	SSL, SSLVerify                           *bool
}

// ClusterOptions builds a cluster redis.ClusterOptions. Timeout and
// KeepaliveTimeout are in milliseconds.
func (c RedisClusterConnConfig) ClusterOptions() *redis.ClusterOptions {
	options := &redis.ClusterOptions{
		Addrs:        append([]string(nil), c.Nodes...),
		Password:     c.Password,
		DialTimeout:  time.Duration(c.Timeout) * time.Millisecond,
		ReadTimeout:  time.Duration(c.Timeout) * time.Millisecond,
		WriteTimeout: time.Duration(c.Timeout) * time.Millisecond,
		PoolSize:     c.KeepalivePool,
	}
	if c.KeepaliveTimeout > 0 {
		options.ConnMaxIdleTime = time.Duration(c.KeepaliveTimeout) * time.Millisecond
	}
	if c.SSL != nil && *c.SSL {
		verify := c.SSLVerify == nil || *c.SSLVerify
		tlsConfig := &tls.Config{InsecureSkipVerify: !verify} //nolint:gosec // explicit operator configuration only
		options.TLSConfig = tlsConfig
	}
	return options
}
