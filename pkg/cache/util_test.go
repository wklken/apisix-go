package cache

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetrieverCoalescesConcurrentGets(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	retriever := NewRetriever(func(key string) (string, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return "value:" + key, nil
	})

	const callers = 8
	results := make([]string, callers)
	errors := make([]error, callers)
	var launched atomic.Int32
	start := make(chan struct{})
	var wait sync.WaitGroup
	for i := range callers {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			launched.Add(1)
			results[index], errors[index] = retriever.Get("same")
		}(i)
	}
	close(start)
	<-entered
	for launched.Load() != callers {
		time.Sleep(time.Millisecond)
	}
	close(release)
	wait.Wait()

	if calls.Load() != 1 {
		t.Fatalf("retrieve calls = %d, want 1", calls.Load())
	}
	for i := range callers {
		if errors[i] != nil {
			t.Fatalf("caller %d error = %v", i, errors[i])
		}
		if results[i] != "value:same" {
			t.Fatalf("caller %d result = %q, want value:same", i, results[i])
		}
	}
}

func TestRetrieverPropagatesLookupError(t *testing.T) {
	retriever := NewRetriever(func(key string) (string, error) {
		return "", errors.New("lookup failed")
	})

	if got, err := retriever.Get("same"); err == nil || err.Error() != "lookup failed" {
		t.Fatalf("Get() = %q/%v, want empty result and lookup failed", got, err)
	} else if got != "" {
		t.Fatalf("Get() result = %q, want empty string on error", got)
	}
}
