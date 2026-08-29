package internal

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apache/rocketmq-client-go/v2/internal/remote"
	"github.com/apache/rocketmq-client-go/v2/primitive"
)

func TestStartTaskCancellationSkipsDelayedOperation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &rmqClient{taskCtx: ctx}
	var operations atomic.Int32
	client.startTask(func(ctx context.Context) {
		if waitTaskDelay(ctx, time.Hour) {
			operations.Add(1)
		}
	})

	cancel()
	client.taskWG.Wait()
	if operations.Load() != 0 {
		t.Fatal("delayed task executed an operation after cancellation")
	}
}

func TestRMQClientShutdownJoinsAllStartTasks(t *testing.T) {
	namesrv, err := NewNamesrv(
		primitive.NewPassthroughResolver([]string{"127.0.0.1:9876"}),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	client := &rmqClient{
		option: ClientOptions{
			ClientIP:     "127.0.0.1",
			InstanceName: "rocketmq-lifecycle-test",
			Namesrv:      namesrv,
			RemotingClientConfig: &remote.RemotingClientConfig{
				TcpOption: remote.TcpOption{ConnectionTimeout: time.Second},
			},
		},
		remoteClient: remote.NewRemotingClient(nil),
		done:         make(chan struct{}),
	}

	client.Start()
	started := time.Now()
	client.Shutdown()
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Shutdown() did not join cancellable tasks promptly: %s", elapsed)
	}
	if client.taskCtx == nil || client.taskCtx.Err() == nil {
		t.Fatal("Shutdown() did not cancel the task context")
	}
	client.taskWG.Wait()
}
