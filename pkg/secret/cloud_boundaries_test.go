package secret

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
)

func cloudConfigForTest(t *testing.T, backend, endpoint string, verify *bool) []byte {
	t.Helper()
	if backend == "aws" {
		raw, err := json.Marshal(
			map[string]any{
				"access_key_id":     "synthetic-access",
				"secret_access_key": "synthetic-key",
				"endpoint_url":      endpoint,
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"ssl_verify": verify, "auth_config": map[string]any{
		"client_email": "service@example.test",
		"private_key": string(
			pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
		),
		"project_id":  "project",
		"token_uri":   endpoint + "/token",
		"entries_uri": endpoint + "/v1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func writeCloudFixture(w http.ResponseWriter, r *http.Request, backend string) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/token" {
		_, _ = w.Write([]byte(`{"access_token":"synthetic-oauth","expires_in":3600,"token_type":"Bearer"}`))
		return
	}
	if backend == "aws" {
		_, _ = w.Write([]byte(`{"SecretString":"synthetic-secret"}`))
		return
	}
	_ = json.NewEncoder(w).
		Encode(map[string]any{"payload": map[string]string{"data": base64.StdEncoding.EncodeToString([]byte("synthetic-secret"))}})
}

func TestCloudScopeRejectsBeforeBackendUseAndCloseZerosCache(t *testing.T) {
	for _, backend := range []string{"aws", "gcp"} {
		t.Run(backend, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(
				http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) { calls.Add(1); writeCloudFixture(w, r, backend) },
				),
			)
			defer server.Close()
			view, scope := openGenerationResolverView(t, cloudConfigForTest(t, backend, server.URL, nil), nil, backend)
			reference := "$secret://" + backend + "/test1/credentials"
			for _, change := range []func(*Scope){func(s *Scope) { s.Generation++ }, func(s *Scope) { s.Domain = generation.DomainStream }, func(s *Scope) { s.Resource.ID = "unpublished" }} {
				other := scope
				change(&other)
				if _, err := view.ResolveReference(
					context.Background(),
					other,
					reference,
				); !errors.Is(
					err,
					ErrCapabilityScopeMismatch,
				) {
					t.Fatalf("scope mismatch=%v", err)
				}
			}
			if _, err := view.ResolveReference(
				context.Background(),
				scope,
				"$secret://"+backend+"/missing/credentials",
			); !errors.Is(
				err,
				ErrCapabilityScopeMismatch,
			) {
				t.Fatalf("missing resource=%v", err)
			}
			if calls.Load() != 0 {
				t.Fatal("invalid scope contacted backend")
			}
			value, err := view.ResolveReference(context.Background(), scope, reference)
			if err != nil || value != "synthetic-secret" {
				t.Fatalf("value=%q err=%v", value, err)
			}
			var retained [][]byte
			view.cache.mu.Lock()
			for _, entry := range view.cache.entries {
				retained = append(retained, entry.value)
			}
			view.cache.mu.Unlock()
			if len(retained) == 0 {
				t.Fatal("no successful response cache")
			}
			if err := view.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, value := range retained {
				for _, b := range value {
					if b != 0 {
						t.Fatal("close retained secret cache bytes")
					}
				}
			}
			before := calls.Load()
			if _, err := view.ResolveReference(context.Background(), scope, reference); err == nil {
				t.Fatal("closed view resolved reference")
			}
			if calls.Load() != before {
				t.Fatal("closed view contacted backend")
			}
		})
	}
}

func TestCloudBackendFailureIsRedactedAndRetryable(t *testing.T) {
	for _, backend := range []string{"aws", "gcp"} {
		t.Run(backend, func(t *testing.T) {
			var fail atomic.Bool
			fail.Store(true)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/token" {
					calls.Add(1)
					if fail.Load() {
						w.WriteHeader(403)
						_, _ = w.Write([]byte("backend-credential-diagnostic"))
						return
					}
				}
				writeCloudFixture(w, r, backend)
			}))
			defer server.Close()
			view, scope := openGenerationResolverView(t, cloudConfigForTest(t, backend, server.URL, nil), nil, backend)
			reference := "$secret://" + backend + "/test1/credentials"
			if _, err := view.ResolveReference(
				context.Background(),
				scope,
				reference,
			); !errors.Is(err, ErrCredentialUnavailable) ||
				strings.Contains(err.Error(), "diagnostic") {
				t.Fatalf("unredacted backend error=%v", err)
			}
			fail.Store(false)
			if value, err := view.ResolveReference(
				context.Background(),
				scope,
				reference,
			); err != nil ||
				value != "synthetic-secret" {
				t.Fatalf("retry value=%q err=%v", value, err)
			}
			if calls.Load() != 2 {
				t.Fatalf("secret calls=%d want failed then successful attempt", calls.Load())
			}
		})
	}
}

func TestCloudEmptyMainKeyNeverContactsBackend(t *testing.T) {
	for _, backend := range []string{"aws", "gcp"} {
		t.Run(backend, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(
				http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) { calls.Add(1); writeCloudFixture(w, r, backend) },
				),
			)
			defer server.Close()
			view, scope := openGenerationResolverView(t, cloudConfigForTest(t, backend, server.URL, nil), nil, backend)
			if _, err := view.ResolveReference(
				context.Background(),
				scope,
				"$secret://"+backend+"/test1//field",
			); !errors.Is(
				err,
				ErrCredentialUnavailable,
			) {
				t.Fatalf("empty main key=%v", err)
			}
			if calls.Load() != 0 {
				t.Fatalf("empty main key made %d backend calls", calls.Load())
			}
		})
	}
}

func TestGCPExplicitTLSOptionDoesNotMutateResolverClient(t *testing.T) {
	server := httptest.NewTLSServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeCloudFixture(w, r, "gcp") }),
	)
	defer server.Close()
	for _, verify := range []*bool{nil, new(true), new(false)} {
		view, scope := openGenerationResolverView(t, cloudConfigForTest(t, "gcp", server.URL, verify), nil, "gcp")
		_, err := view.ResolveReference(context.Background(), scope, "$secret://gcp/test1/credentials")
		if verify != nil && !*verify {
			if err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(err, ErrCredentialUnavailable) {
			t.Fatalf("untrusted TLS=%v", err)
		}
		transport := view.resolver.client.Transport.(*http.Transport)
		if transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
			t.Fatal("backend override mutated shared resolver transport")
		}
	}
}

func TestCloudRequestCancellationPropagates(t *testing.T) {
	for _, backend := range []string{"aws", "gcp"} {
		t.Run(backend, func(t *testing.T) {
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					writeCloudFixture(w, r, backend)
					return
				}
				_, _ = io.Copy(io.Discard, r.Body)
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer server.Close()
			defer close(release)
			view, scope := openGenerationResolverView(t, cloudConfigForTest(t, backend, server.URL, nil), nil, backend)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if _, err := view.ResolveReference(
				ctx,
				scope,
				"$secret://"+backend+"/test1/credentials",
			); !errors.Is(
				err,
				context.DeadlineExceeded,
			) {
				t.Fatalf("cancellation=%v", err)
			}
		})
	}
}
