package shared

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-resty/resty/v2"
	"github.com/redis/go-redis/v9"
)

func TestSharedClientAcquireReleaseAndFinalClose(t *testing.T) {
	var creates atomic.Int32
	var closes atomic.Int32
	key := "plugin-a:uid-1"
	create := func() (any, error) {
		creates.Add(1)
		return &httpClientStub{id: int(creates.Load())}, nil
	}
	closeFn := func(v any) {
		closes.Add(1)
	}

	first, releaseFirst, err := AcquireClient(key, create, closeFn)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if got := creates.Load(); got != 1 {
		t.Fatalf("creates after first acquire = %d, want 1", got)
	}

	second, releaseSecond, err := AcquireClient(key, create, closeFn)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if got := creates.Load(); got != 1 {
		t.Fatalf("creates after second acquire = %d, want 1 (reuse)", got)
	}
	if first != second {
		t.Fatal("second acquire returned a different client for the same key")
	}

	releaseFirst()
	if got := closes.Load(); got != 0 {
		t.Fatalf("close after releasing one of two refs = %d, want 0", got)
	}

	releaseSecond()
	if got := closes.Load(); got != 1 {
		t.Fatalf("close after final release = %d, want 1", got)
	}

	recreated, _, err := AcquireClient(key, create, closeFn)
	if err != nil {
		t.Fatalf("re-acquire after close: %v", err)
	}
	if got := creates.Load(); got != 2 {
		t.Fatalf("creates after recreation = %d, want 2", got)
	}
	if recreated == first {
		t.Fatal("recreated client reused the closed value")
	}
}

func TestSharedClientDoubleReleaseClosesOnce(t *testing.T) {
	var closes atomic.Int32
	_, release, err := AcquireClient("plugin-b:uid-2", func() (any, error) {
		return &httpClientStub{}, nil
	}, func(v any) {
		closes.Add(1)
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	release()
	release()
	if got := closes.Load(); got != 1 {
		t.Fatalf("close calls after double release = %d, want 1", got)
	}
}

func TestSharedClientDistinctKeysKeepDistinctTypes(t *testing.T) {
	restyValue, releaseResty, err := AcquireClient(
		"plugin-c:uid-3",
		func() (any, error) { return resty.New(), nil },
		func(v any) { v.(*resty.Client).GetClient().CloseIdleConnections() },
	)
	if err != nil {
		t.Fatalf("acquire resty client: %v", err)
	}
	redisValue, releaseRedis, err := AcquireClient(
		"plugin-d:uid-4",
		func() (any, error) { return redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"}), nil },
		func(v any) { _ = v.(*redis.Client).Close() },
	)
	if err != nil {
		t.Fatalf("acquire redis client: %v", err)
	}

	if _, ok := restyValue.(*resty.Client); !ok {
		t.Fatalf("resty key returned %T, want *resty.Client", restyValue)
	}
	if _, ok := redisValue.(*redis.Client); !ok {
		t.Fatalf("redis key returned %T, want *redis.Client", redisValue)
	}
	if restyValue == redisValue {
		t.Fatal("distinct keys returned the same value")
	}
	releaseResty()
	releaseRedis()
}

func TestSharedClientConcurrentAcquireRelease(t *testing.T) {
	const workers = 16
	var creates atomic.Int32
	var closes atomic.Int32
	key := "plugin-e:uid-5"

	start := make(chan struct{})
	acquired := make(chan struct{})
	releases := make(chan func(), workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			<-start
			_, release, err := AcquireClient(key, func() (any, error) {
				creates.Add(1)
				return &httpClientStub{}, nil
			}, func(v any) {
				closes.Add(1)
			})
			if err != nil {
				t.Errorf("concurrent acquire: %v", err)
				return
			}
			releases <- release
			acquired <- struct{}{}
		})
	}

	close(start)
	for range workers {
		<-acquired
	}
	close(releases)
	for release := range releases {
		release()
	}
	wg.Wait()

	if got := creates.Load(); got != 1 {
		t.Fatalf("creates across %d concurrent acquires = %d, want 1", workers, got)
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("closes after all concurrent releases = %d, want 1", got)
	}
}

type httpClientStub struct {
	id int
}
