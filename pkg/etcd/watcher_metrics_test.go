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
