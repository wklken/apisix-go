package etcd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/wklken/apisix-go/pkg/observability/metrics"
	"github.com/wklken/apisix-go/pkg/store"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestFetchAllRecordsReachabilityAndAppliedRevision(t *testing.T) {
	reachable, revision := installEtcdRuntimeMetrics(t)
	client := &ConfigClient{
		events:         make(chan *store.Event),
		requestTimeout: 50 * time.Millisecond,
		knownKeys:      make(map[string]struct{}),
		loadSnapshot: func(context.Context) (*clientv3.GetResponse, error) {
			return &clientv3.GetResponse{Header: &etcdserverpb.ResponseHeader{Revision: 17}}, nil
		},
	}
	if err := client.FetchAll(); err != nil {
		t.Fatalf("FetchAll() error = %v", err)
	}
	if got := metricGaugeValue(t, reachable); got != 1 {
		t.Fatalf("etcd reachable = %v, want 1", got)
	}
	if got := metricGaugeValue(t, revision); got != 17 {
		t.Fatalf("etcd revision = %v, want 17", got)
	}
}

func TestFetchAllFailureMarksUnreachableWithoutAdvancingRevision(t *testing.T) {
	reachable, revision := installEtcdRuntimeMetrics(t)
	metrics.RecordEtcdAppliedRevision(11)
	client := &ConfigClient{
		requestTimeout: 50 * time.Millisecond,
		knownKeys:      make(map[string]struct{}),
		loadSnapshot: func(context.Context) (*clientv3.GetResponse, error) {
			return nil, errors.New("unavailable")
		},
	}
	if err := client.FetchAll(); err == nil {
		t.Fatal("FetchAll() error = nil, want unavailable")
	}
	if got := metricGaugeValue(t, reachable); got != 0 {
		t.Fatalf("etcd reachable = %v, want 0", got)
	}
	if got := metricGaugeValue(t, revision); got != 11 {
		t.Fatalf("etcd revision = %v, want prior applied revision 11", got)
	}
}

func TestWatchApplyFailureKeepsReachabilityWhileSnapshotRecoveryStarts(t *testing.T) {
	reachable, _ := installEtcdRuntimeMetrics(t)
	watchResponses := make(chan clientv3.WatchResponse, 1)
	watchResponses <- clientv3.WatchResponse{
		Header: etcdserverpb.ResponseHeader{Revision: 19},
		Events: []*clientv3.Event{nil},
	}
	close(watchResponses)
	recoveryStarted := make(chan struct{})
	client := &ConfigClient{
		requestTimeout: time.Second,
		knownKeys:      make(map[string]struct{}),
		openWatch: func(context.Context, int64) clientv3.WatchChan {
			return watchResponses
		},
		loadSnapshot: func(ctx context.Context) (*clientv3.GetResponse, error) {
			close(recoveryStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		client.Watch(ctx)
		close(done)
	}()
	select {
	case <-recoveryStarted:
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("snapshot recovery did not start")
	}
	if got := metricGaugeValue(t, reachable); got != 1 {
		cancel()
		<-done
		t.Fatalf("etcd reachable after local apply failure = %v, want 1", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Watch did not stop after cancellation")
	}
}

func TestWatchHealthCheckRecoversWhileWatchRemainsOpen(t *testing.T) {
	reachable, _ := installEtcdRuntimeMetrics(t)
	watchStream := make(chan clientv3.WatchResponse)
	firstProbe := make(chan struct{})
	releaseRecovery := make(chan struct{})
	client := &ConfigClient{
		requestTimeout:      time.Second,
		healthCheckInterval: time.Millisecond,
		healthCheck: func(ctx context.Context) error {
			select {
			case <-firstProbe:
			default:
				close(firstProbe)
				return errors.New("etcd unavailable")
			}
			select {
			case <-releaseRecovery:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		openWatch: func(context.Context, int64) clientv3.WatchChan {
			return watchStream
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		client.Watch(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Watch did not stop during cleanup")
		}
	})

	select {
	case <-firstProbe:
	case <-time.After(time.Second):
		t.Fatal("health check did not run")
	}
	waitForEtcdGauge(t, reachable, 0)
	select {
	case <-done:
		t.Fatal("Watch returned while its watch channel remained open")
	default:
	}

	close(releaseRecovery)
	waitForEtcdGauge(t, reachable, 1)
	select {
	case <-done:
		t.Fatal("Watch returned before cancellation")
	default:
	}
}

func TestWatchHealthCheckCancellationJoinsMonitor(t *testing.T) {
	monitorStarted := make(chan struct{})
	monitorExited := make(chan struct{})
	probeDeadline := make(chan time.Time, 1)
	watchStream := make(chan clientv3.WatchResponse)
	client := &ConfigClient{
		requestTimeout:      2 * time.Second,
		healthCheckInterval: time.Hour,
		healthCheck: func(ctx context.Context) error {
			deadline, ok := ctx.Deadline()
			if !ok {
				return errors.New("health probe context has no deadline")
			}
			probeDeadline <- deadline
			close(monitorStarted)
			<-ctx.Done()
			close(monitorExited)
			return ctx.Err()
		},
		openWatch: func(context.Context, int64) clientv3.WatchChan {
			return watchStream
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		client.Watch(ctx)
		close(done)
	}()
	select {
	case <-monitorStarted:
	case <-time.After(time.Second):
		t.Fatal("health monitor did not start")
	}
	deadline := <-probeDeadline
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > 2*time.Second {
		t.Fatalf("health probe deadline remaining = %s, want within (0s, 2s]", remaining)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Watch did not return after cancellation")
	}
	select {
	case <-monitorExited:
	default:
		t.Fatal("Watch returned before joining the health monitor")
	}
}

func waitForEtcdGauge(t *testing.T, gauge prometheus.Gauge, want float64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := metricGaugeValue(t, gauge); got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("etcd reachable = %v, want %v", metricGaugeValue(t, gauge), want)
}

func installEtcdRuntimeMetrics(t *testing.T) (prometheus.Gauge, prometheus.Gauge) {
	t.Helper()
	oldReachable, oldRevision := metrics.EtcdReachable, metrics.EtcdRevision
	reachable := prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_watcher_etcd_reachable"})
	revision := prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_watcher_etcd_revision"})
	metrics.EtcdReachable, metrics.EtcdRevision = reachable, revision
	t.Cleanup(func() { metrics.EtcdReachable, metrics.EtcdRevision = oldReachable, oldRevision })
	return reachable, revision
}

func metricGaugeValue(t *testing.T, gauge prometheus.Gauge) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := gauge.Write(metric); err != nil {
		t.Fatalf("write gauge: %v", err)
	}
	return metric.GetGauge().GetValue()
}
