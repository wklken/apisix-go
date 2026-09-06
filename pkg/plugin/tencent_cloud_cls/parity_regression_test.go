package tencent_cloud_cls

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParityLargeBatchKeepsEveryEntryAndRetriesOnlyFailedGroup(t *testing.T) {
	for _, failSecond := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail-second-%t", failSecond), func(t *testing.T) {
			var calls atomic.Int32
			bodies := make(chan []byte, 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
				bodies <- body
				if calls.Add(1) == 2 && failSecond {
					w.WriteHeader(503)
					return
				}
				w.WriteHeader(200)
			}))
			defer server.Close()
			p := newTestPlugin(
				t,
				Config{
					Scheme:    "http",
					CLSHost:   strings.TrimPrefix(server.URL, "http://"),
					CLSTopic:  "test",
					SecretID:  "synthetic",
					SecretKey: "synthetic",
					Timeout:   3000,
				},
			)
			t.Cleanup(p.Stop)
			logs := make([]map[string]any, 6)
			for i := range logs {
				logs[i] = map[string]any{"id": fmt.Sprint(i), "payload": strings.Repeat("v", maxSingleValueSize)}
			}
			firstFailed, err := p.SendBatch(context.Background(), logs, 6)
			if failSecond {
				if err == nil || firstFailed != 5 {
					t.Fatalf("partial delivery=(%d,%v), want first failed index 5 (one-based)", firstFailed, err)
				}
				if _, err := p.SendBatch(context.Background(), logs[firstFailed-1:], 6); err != nil {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			wantCalls := int32(2)
			if failSecond {
				wantCalls = 3
			}
			if calls.Load() != wantCalls {
				t.Fatalf("requests=%d, want %d", calls.Load(), wantCalls)
			}
			received := map[string]int{}
			for request := int32(1); request <= wantCalls; request++ {
				body := <-bodies
				entries := decodeCLSBody(t, body)
				if failSecond && request == 2 {
					continue
				}
				for _, entry := range entries {
					received[entry["id"]]++
				}
			}
			for i := range logs {
				if received[fmt.Sprint(i)] != 1 {
					t.Fatalf("entry %d delivery count=%d, want 1", i, received[fmt.Sprint(i)])
				}
			}
		})
	}
}
