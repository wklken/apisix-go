package pluginintegration

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFirstChunkDeadlineIncludesDelayedResponseHeaders(t *testing.T) {
	frame := "data: first\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(30 * time.Millisecond)
		_, _ = w.Write([]byte(frame))
	}))
	defer server.Close()
	started := time.Now()
	response, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	_, err = readAndAssertResponseChunks(
		t,
		response.Body,
		[]Matcher{{Equals: &frame}},
		started.Add(10*time.Millisecond),
	)
	if err == nil || !strings.Contains(err.Error(), "first_chunk_less_than") {
		t.Fatalf("error=%v, buffered headers must fail first-frame deadline", err)
	}
}
