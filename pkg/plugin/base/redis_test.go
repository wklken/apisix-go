package base

import (
	"slices"
	"testing"
	"time"
)

func TestRedisConnConfigOptions(t *testing.T) {
	ssl := true
	verify := false
	cfg := RedisConnConfig{
		Host:             "127.0.0.1",
		Port:             6379,
		Username:         "user",
		Password:         "pass",
		Database:         3,
		Timeout:          1000,
		KeepaliveTimeout: 10000,
		KeepalivePool:    100,
		SSL:              &ssl,
		SSLVerify:        &verify,
	}

	options := cfg.Options()
	if got := options.Addr; got != "127.0.0.1:6379" {
		t.Fatalf("Addr = %q, want 127.0.0.1:6379", got)
	}
	if options.Username != "user" {
		t.Fatalf("Username = %q, want user", options.Username)
	}
	if options.Password != "pass" {
		t.Fatalf("Password = %q, want pass", options.Password)
	}
	if options.DB != 3 {
		t.Fatalf("DB = %d, want 3", options.DB)
	}
	if options.DialTimeout != time.Second {
		t.Fatalf("DialTimeout = %v, want 1s", options.DialTimeout)
	}
	if options.ReadTimeout != time.Second {
		t.Fatalf("ReadTimeout = %v, want 1s", options.ReadTimeout)
	}
	if options.WriteTimeout != time.Second {
		t.Fatalf("WriteTimeout = %v, want 1s", options.WriteTimeout)
	}
	if options.PoolSize != 100 {
		t.Fatalf("PoolSize = %d, want 100", options.PoolSize)
	}
	if options.ConnMaxIdleTime != 10*time.Second {
		t.Fatalf("ConnMaxIdleTime = %v, want 10s", options.ConnMaxIdleTime)
	}
	if options.TLSConfig == nil {
		t.Fatal("TLSConfig = nil, want non-nil")
	}
	if !options.TLSConfig.InsecureSkipVerify {
		t.Fatal("TLSConfig.InsecureSkipVerify = false, want true")
	}
}

func TestRedisConnConfigNilSSLVerifyDefaultsToVerify(t *testing.T) {
	ssl := true
	cfg := RedisConnConfig{Host: "127.0.0.1", Port: 6379, SSL: &ssl, SSLVerify: nil}

	options := cfg.Options()
	if options.TLSConfig == nil {
		t.Fatal("TLSConfig = nil, want non-nil")
	}
	if options.TLSConfig.InsecureSkipVerify {
		t.Fatal("TLSConfig.InsecureSkipVerify = true, want secure nil-default false")
	}
}

func TestRedisClusterConnConfigNilSSLVerifyDefaultsToVerify(t *testing.T) {
	ssl := true
	cfg := RedisClusterConnConfig{Nodes: []string{"a:6379"}, SSL: &ssl, SSLVerify: nil}

	options := cfg.ClusterOptions()
	if options.TLSConfig == nil {
		t.Fatal("TLSConfig = nil, want non-nil")
	}
	if options.TLSConfig.InsecureSkipVerify {
		t.Fatal("TLSConfig.InsecureSkipVerify = true, want secure nil-default false")
	}
}

func TestRedisConnConfigOptionsWithoutTLS(t *testing.T) {
	ssl := false
	cfg := RedisConnConfig{SSL: &ssl}

	options := cfg.Options()
	if options.TLSConfig != nil {
		t.Fatal("TLSConfig = non-nil, want nil")
	}
	if options.ConnMaxIdleTime != 0 {
		t.Fatalf("ConnMaxIdleTime = %v, want 0", options.ConnMaxIdleTime)
	}
}

func TestRedisClusterConnConfigOptions(t *testing.T) {
	ssl := true
	verify := true
	cfg := RedisClusterConnConfig{
		Nodes:            []string{"a:6379", "b:6379"},
		Password:         "pass",
		Timeout:          500,
		KeepaliveTimeout: 2000,
		KeepalivePool:    10,
		SSL:              &ssl,
		SSLVerify:        &verify,
	}

	options := cfg.ClusterOptions()
	if !slices.Equal(options.Addrs, []string{"a:6379", "b:6379"}) {
		t.Fatalf("Addrs = %v, want [a:6379 b:6379]", options.Addrs)
	}
	if options.Password != "pass" {
		t.Fatalf("Password = %q, want pass", options.Password)
	}
	if options.DialTimeout != 500*time.Millisecond {
		t.Fatalf("DialTimeout = %v, want 500ms", options.DialTimeout)
	}
	if options.ReadTimeout != 500*time.Millisecond {
		t.Fatalf("ReadTimeout = %v, want 500ms", options.ReadTimeout)
	}
	if options.WriteTimeout != 500*time.Millisecond {
		t.Fatalf("WriteTimeout = %v, want 500ms", options.WriteTimeout)
	}
	if options.PoolSize != 10 {
		t.Fatalf("PoolSize = %d, want 10", options.PoolSize)
	}
	if options.ConnMaxIdleTime != 2*time.Second {
		t.Fatalf("ConnMaxIdleTime = %v, want 2s", options.ConnMaxIdleTime)
	}
	if options.TLSConfig == nil {
		t.Fatal("TLSConfig = nil, want non-nil")
	}
	if options.TLSConfig.InsecureSkipVerify {
		t.Fatal("TLSConfig.InsecureSkipVerify = true, want false")
	}
}

func TestRedisClusterConnConfigOptionsWithoutTLS(t *testing.T) {
	ssl := false
	cfg := RedisClusterConnConfig{SSL: &ssl}

	options := cfg.ClusterOptions()
	if options.TLSConfig != nil {
		t.Fatal("TLSConfig = non-nil, want nil")
	}
}
