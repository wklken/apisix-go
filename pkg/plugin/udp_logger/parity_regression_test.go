package udp_logger

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/json"
)

func TestParityExplicitZeroRetryDelay(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"host": "127.0.0.1", "port": 8181, "retry_delay": 0}`), &cfg); err != nil {
		t.Fatal(err)
	}
	p := newTestPlugin(t, cfg)
	t.Cleanup(p.Stop)
	if p.config.RetryDelay != 0 {
		t.Fatalf("retry_delay=%d, want explicit zero", p.config.RetryDelay)
	}
}

type parityRetryConn struct {
	recordingConn
	attempts  atomic.Int32
	delivered chan struct{}
}

func (conn *parityRetryConn) Write(body []byte) (int, error) {
	if conn.attempts.Add(1) == 1 {
		return 0, errors.New("synthetic first delivery failure")
	}
	close(conn.delivered)
	return len(body), nil
}

func TestParityZeroRetryDelayReachesBatchScheduler(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal(
		[]byte(`{"host":"127.0.0.1","port":8181,"retry_delay":0,"max_retry_count":1,"batch_max_size":1}`),
		&cfg,
	); err != nil {
		t.Fatal(err)
	}
	p := newTestPlugin(t, cfg)
	t.Cleanup(p.Stop)
	conn := &parityRetryConn{delivered: make(chan struct{})}
	p.conn = conn
	if err := p.EnqueueLog(map[string]any{"message": "test"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-conn.delivered:
	case <-time.After(750 * time.Millisecond):
		t.Fatal("explicit-zero retry was delayed by the one-second default")
	}
}
