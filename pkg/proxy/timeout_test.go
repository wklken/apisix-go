package proxy

import (
	"context"
	"sync"
	"testing"
	"time"
)

type blockingBody struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingBody() *blockingBody { return &blockingBody{closed: make(chan struct{})} }

func (b *blockingBody) Read([]byte) (int, error) {
	<-b.closed
	return 0, context.Canceled
}

func (b *blockingBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestProgressTimeoutBodyClosesStalledRead(t *testing.T) {
	body := newBlockingBody()
	timed := newProgressTimeoutBody(body, 20*time.Millisecond, func() {})
	started := time.Now()
	_, err := timed.Read(make([]byte, 1))
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("Read() error/elapsed = %v/%s", err, time.Since(started))
	}
}
