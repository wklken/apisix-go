package mcp_bridge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUnknownSessionMessageIsAccepted(t *testing.T) {
	p := newTestPlugin(t, Config{Command: "cat"})
	req := httptest.NewRequest(http.MethodPost, "/message?sessionId=missing", strings.NewReader(`{"jsonrpc":"2.0"}`))
	rr := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("unknown session status=%d, want 202", rr.Code)
	}
}

// The production stdin is *os.File from exec.Cmd.StdinPipe. Its Write lock
// spans the entire buffer, including buffers larger than the OS pipe capacity.
func TestConcurrentMessagesRemainIntactOnProductionPipe(t *testing.T) {
	p := newTestPlugin(t, Config{Command: "cat"})
	sess, err := p.startSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.closeSession(sess) })
	expected := make(map[string]bool)
	var wg sync.WaitGroup
	for i := range 8 {
		message := fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%d,"data":"%s"}`,
			i,
			strings.Repeat(string(rune('a'+i)), 128*1024),
		)
		expected[message] = true
		wg.Go(func() {
			response := httptest.NewRecorder()
			p.handleMessage(
				response,
				httptest.NewRequest(http.MethodPost, "/message?sessionId="+sess.id, strings.NewReader(message)),
			)
			if response.Code != 202 {
				t.Errorf("status=%d", response.Code)
			}
		})
	}
	wg.Wait()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for range 8 {
		select {
		case event := <-sess.events:
			if !expected[event.data] {
				t.Fatalf("unexpected or interleaved message length=%d", len(event.data))
			}
			delete(expected, event.data)
		case <-deadline.C:
			t.Fatal("timed out waiting for process messages")
		}
	}
	if len(expected) != 0 {
		t.Fatalf("missing messages=%d", len(expected))
	}
}
