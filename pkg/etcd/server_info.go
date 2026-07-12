package etcd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/wklken/apisix-go/pkg/logger"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const serverInfoKeyPrefix = "data_plane/server_info"

type serverInfoLeaseClient interface {
	Put(context.Context, string, string, ...clientv3.OpOption) (*clientv3.PutResponse, error)
	Grant(context.Context, int64) (*clientv3.LeaseGrantResponse, error)
	KeepAliveOnce(context.Context, clientv3.LeaseID) (*clientv3.LeaseKeepAliveResponse, error)
}

// ServerInfoReporter owns the lease used for the control-plane server-info
// record. It deliberately reports through the same etcd client as the config
// watcher so the record follows the configured prefix and credentials.
type ServerInfoReporter struct {
	client  serverInfoLeaseClient
	key     string
	ttl     int64
	leaseID clientv3.LeaseID
	mu      sync.Mutex
}

func serverInfoKey(prefix string, nodeID string) string {
	base := "/" + strings.Trim(prefix, "/")
	if base == "/" {
		base = ""
	}
	return base + "/" + serverInfoKeyPrefix + "/" + strings.Trim(nodeID, "/")
}

func newServerInfoReporter(client serverInfoLeaseClient, key string, ttl time.Duration) *ServerInfoReporter {
	return &ServerInfoReporter{
		client: client,
		key:    key,
		ttl:    int64(ttl / time.Second),
	}
}

// Report writes the current JSON payload and renews its lease. A failed
// renewal clears the cached lease so the next report creates a fresh one.
func (r *ServerInfoReporter) Report(ctx context.Context, payload []byte) error {
	if r == nil || r.client == nil {
		return errors.New("server-info reporter is not initialized")
	}
	if r.key == "" {
		return errors.New("server-info reporter key is empty")
	}
	if r.ttl <= 0 {
		return errors.New("server-info reporter TTL must be positive")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.leaseID == 0 {
		lease, err := r.client.Grant(ctx, r.ttl)
		if err != nil {
			return fmt.Errorf("grant server-info lease: %w", err)
		}
		if lease == nil || lease.ID == 0 {
			return errors.New("grant server-info lease returned an empty lease")
		}
		r.leaseID = lease.ID
	}

	if _, err := r.client.Put(ctx, r.key, string(payload), clientv3.WithLease(r.leaseID)); err != nil {
		r.leaseID = 0
		return fmt.Errorf("put server-info: %w", err)
	}
	if _, err := r.client.KeepAliveOnce(ctx, r.leaseID); err != nil {
		r.leaseID = 0
		return fmt.Errorf("keepalive server-info lease: %w", err)
	}
	return nil
}

// Start reports immediately and refreshes the record at half the configured
// TTL until ctx is canceled. A transient failure is logged and retried on the
// next tick; the cached lease is reset by Report when renewal fails.
func (r *ServerInfoReporter) Start(ctx context.Context, provider func() ([]byte, error)) error {
	if provider == nil {
		return errors.New("server-info provider is nil")
	}
	if r == nil || r.ttl <= 0 {
		return errors.New("server-info reporter TTL must be positive")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if payload, err := provider(); err != nil {
		return fmt.Errorf("build server-info payload: %w", err)
	} else if err := r.Report(ctx, payload); err != nil {
		return err
	}

	interval := time.Duration(r.ttl) * time.Second / 2
	if interval < time.Second {
		interval = time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				payload, err := provider()
				if err == nil {
					err = r.Report(ctx, payload)
				}
				if err != nil {
					logger.Warnf("server-info report failed: %s", err)
				}
			}
		}
	}()
	return nil
}

// StartServerInfoReporter starts a reporter under this config client's
// configured etcd prefix.
func (c *ConfigClient) StartServerInfoReporter(
	ctx context.Context,
	nodeID string,
	ttl time.Duration,
	provider func() ([]byte, error),
) (*ServerInfoReporter, error) {
	if c == nil || c.client == nil {
		return nil, errors.New("etcd config client is not initialized")
	}
	if strings.Trim(nodeID, "/") == "" {
		return nil, errors.New("server-info node ID is empty")
	}
	reporter := newServerInfoReporter(c.client, serverInfoKey(c.prefix, nodeID), ttl)
	if err := reporter.Start(ctx, provider); err != nil {
		return nil, err
	}
	return reporter, nil
}
