package etcd

import (
	"context"
	"errors"
	"fmt"
	"net/url"
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

// ServerVersion returns the first reachable etcd server version using the
// existing configuration client and its request timeout.
func (c *ConfigClient) ServerVersion(ctx context.Context) (string, error) {
	if c == nil || c.status == nil || len(c.endpoints) == 0 {
		return "", errors.New("etcd config client cannot query server version")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := c.requestTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	var failures []error
	for _, endpoint := range c.endpoints {
		statusCtx, cancel := context.WithTimeout(ctx, timeout)
		response, err := c.status(statusCtx, endpoint)
		cancel()
		if err != nil {
			failures = append(failures, fmt.Errorf("status %s: %w", redactEndpointUserinfo(endpoint), err))
			continue
		}
		if response == nil || strings.TrimSpace(response.Version) == "" {
			failures = append(
				failures,
				fmt.Errorf("status %s returned an empty version", redactEndpointUserinfo(endpoint)),
			)
			continue
		}
		return strings.TrimSpace(response.Version), nil
	}
	return "", errors.Join(failures...)
}

func redactEndpointUserinfo(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.User == nil {
		return endpoint
	}
	parsed.User = nil
	return parsed.String()
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

	lifecycleMu sync.Mutex
	cancel      context.CancelFunc
	done        chan struct{}
	started     bool
	stopOnce    sync.Once
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
	r.lifecycleMu.Lock()
	if r.started {
		r.lifecycleMu.Unlock()
		return errors.New("server-info reporter is already started")
	}
	r.started = true
	runCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.done = make(chan struct{})
	done := r.done
	r.lifecycleMu.Unlock()

	if payload, err := provider(); err != nil {
		cancel()
		close(done)
		return fmt.Errorf("build server-info payload: %w", err)
	} else if err := r.Report(runCtx, payload); err != nil {
		cancel()
		close(done)
		return err
	}

	interval := max(time.Duration(r.ttl)*time.Second/2, time.Second)
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				payload, err := provider()
				if err == nil {
					err = r.Report(runCtx, payload)
				}
				if err != nil {
					logger.Warnf("server-info report failed: %s", err)
				}
			}
		}
	}()
	return nil
}

// Stop cancels and joins the reporter. Repeated calls replay the first result.
func (r *ServerInfoReporter) Stop() error {
	if r == nil {
		return nil
	}
	r.stopOnce.Do(func() {
		r.lifecycleMu.Lock()
		cancel, done, started := r.cancel, r.done, r.started
		r.lifecycleMu.Unlock()
		if !started {
			return
		}
		if cancel != nil {
			cancel()
		}
		if done != nil {
			<-done
		}
	})
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
	if ctx == nil {
		ctx = context.Background()
	}
	reporter := newServerInfoReporter(c.client, serverInfoKey(c.prefix, nodeID), ttl)
	c.lifecycleMu.Lock()
	if c.closed {
		c.lifecycleMu.Unlock()
		return nil, errors.New("etcd config client is closed")
	}
	c.ensureLifetimeLocked()
	lifetime := c.lifetimeCtx
	c.reporterStarts.Add(1)
	c.lifecycleMu.Unlock()
	defer c.reporterStarts.Done()

	reporterCtx, cancelReporter := context.WithCancel(ctx)
	stopLifetimeCancel := context.AfterFunc(lifetime, cancelReporter)
	if err := reporter.Start(reporterCtx, provider); err != nil {
		stopLifetimeCancel()
		cancelReporter()
		return nil, err
	}
	c.lifecycleMu.Lock()
	if c.closed {
		c.lifecycleMu.Unlock()
		stopLifetimeCancel()
		cancelReporter()
		_ = reporter.Stop()
		return nil, errors.New("etcd config client closed while starting server-info reporter")
	}
	c.reporters[reporter] = struct{}{}
	c.lifecycleMu.Unlock()
	return reporter, nil
}
