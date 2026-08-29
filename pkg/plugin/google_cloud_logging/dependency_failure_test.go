package google_cloud_logging

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSendBatchClassifiesGoogleCloudDependencyRejections(t *testing.T) {
	t.Run("oauth rejection", func(t *testing.T) {
		var entryCalls atomic.Int32
		tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		}))
		t.Cleanup(tokenServer.Close)
		entryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			entryCalls.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(entryServer.Close)

		p := newGoogleDependencyTestPlugin(t, tokenServer.URL, entryServer.URL)
		_, err := p.SendBatch(context.Background(), []map[string]any{{"path": "/orders"}}, 1)
		if err == nil || !strings.Contains(err.Error(), "failed to get google-cloud-logging oauth token") ||
			!strings.Contains(err.Error(), "invalid_grant") {
			t.Fatalf("SendBatch() error = %v, want classified OAuth rejection", err)
		}
		if got := entryCalls.Load(); got != 0 {
			t.Fatalf("entries calls = %d, want none after OAuth rejection", got)
		}
	})

	t.Run("malformed oauth response", func(t *testing.T) {
		tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":`))
		}))
		t.Cleanup(tokenServer.Close)
		entryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(entryServer.Close)

		p := newGoogleDependencyTestPlugin(t, tokenServer.URL, entryServer.URL)
		_, err := p.SendBatch(context.Background(), []map[string]any{{"path": "/orders"}}, 1)
		if err == nil || !strings.Contains(err.Error(), "failed to get google-cloud-logging oauth token") {
			t.Fatalf("SendBatch() error = %v, want malformed OAuth response failure", err)
		}
	})

	t.Run("entries rejection", func(t *testing.T) {
		tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"token-a","token_type":"Bearer","expires_in":3600}`))
		}))
		t.Cleanup(tokenServer.Close)
		entryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"quota exceeded"}`))
		}))
		t.Cleanup(entryServer.Close)

		p := newGoogleDependencyTestPlugin(t, tokenServer.URL, entryServer.URL)
		_, err := p.SendBatch(context.Background(), []map[string]any{{"path": "/orders"}}, 1)
		if err == nil || !strings.Contains(err.Error(), "status code [429]") ||
			!strings.Contains(err.Error(), "quota exceeded") {
			t.Fatalf("SendBatch() error = %v, want entries status and rejection body", err)
		}
	})
}

func newGoogleDependencyTestPlugin(t *testing.T, tokenURI, entriesURI string) *Plugin {
	t.Helper()
	privateKey, _ := testPrivateKey(t)
	return newTestPlugin(t, Config{
		AuthConfig: &AuthConfig{
			ClientEmail: "svc@example.iam.gserviceaccount.com",
			PrivateKey:  privateKey,
			ProjectID:   "project-a",
			TokenURI:    tokenURI,
			EntriesURI:  entriesURI,
		},
		LogFormat: map[string]string{"path": "$uri"},
	})
}
