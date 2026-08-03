package base

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReplaceRequestBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)

	const newBody = `{"role":"user","content":"hello"}`
	ReplaceRequestBody(r, []byte(newBody))

	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read replaced body: %v", err)
	}
	if got := string(body); got != newBody {
		t.Fatalf("Body = %q, want %q", got, newBody)
	}

	if got := r.ContentLength; got != int64(len(newBody)) {
		t.Fatalf("ContentLength = %d, want %d", got, len(newBody))
	}
	if got := r.Header.Get("Content-Length"); got != fmt.Sprint(len(newBody)) {
		t.Fatalf("Content-Length header = %q, want %q", got, fmt.Sprint(len(newBody)))
	}

	refreshed, err := r.GetBody()
	if err != nil {
		t.Fatalf("GetBody() error = %v", err)
	}
	refreshedBody, err := io.ReadAll(refreshed)
	if err != nil {
		t.Fatalf("read GetBody content: %v", err)
	}
	if got := string(refreshedBody); got != newBody {
		t.Fatalf("GetBody content = %q, want %q", got, newBody)
	}

	secondRefresh, err := r.GetBody()
	if err != nil {
		t.Fatalf("second GetBody() error = %v", err)
	}
	secondBody, err := io.ReadAll(secondRefresh)
	if err != nil {
		t.Fatalf("read second GetBody content: %v", err)
	}
	if got := string(secondBody); got != newBody {
		t.Fatalf("second GetBody content = %q, want %q", got, newBody)
	}
}
