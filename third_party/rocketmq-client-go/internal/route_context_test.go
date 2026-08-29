package internal

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTopicRouteLockHonorsContext(t *testing.T) {
	server := &namesrvs{}
	server.lockNamesrv.Lock()
	defer server.lockNamesrv.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _, err := server.UpdateTopicRouteInfoWithDefaultContext(ctx, "topic", "", 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("route update error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("route update lock ignored context: elapsed %s", elapsed)
	}
}
