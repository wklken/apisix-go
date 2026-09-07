package opa

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRegressionInformationalDenialDoesNotBecome200(t *testing.T) {
	decisionWritten := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.WriteString(w, `{"result":{"allow":false,"status_code":100,"reason":"denied"}}`); err != nil {
			t.Errorf("write OPA decision: %v", err)
			return
		}
		close(decisionWritten)
	}))
	defer backend.Close()
	p := newTestPlugin(t, Config{Host: backend.URL, Policy: "authz"})
	gateway := httptest.NewServer(p.Handler(http.NotFoundHandler()))
	defer gateway.Close()
	client := &http.Client{Timeout: 250 * time.Millisecond}
	res, err := client.Get(gateway.URL)
	select {
	case <-decisionWritten:
	default:
		t.Fatal("request ended without receiving the OPA decision")
	}
	if err != nil {
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("request ended before cancellation: %v", err)
		}
		return
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	t.Fatalf("OPA deny completed a final response: status=%d body=%q", res.StatusCode, body)
}
