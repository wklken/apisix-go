package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryTransportNonIdempotentConnectFailureCanFailOver(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPatch, "LOCK"} {
		for _, body := range []string{"", "payload"} {
			t.Run(method+"/"+body, func(t *testing.T) {
				healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					got, err := io.ReadAll(r.Body)
					if err != nil || string(got) != body {
						t.Errorf("upstream body=%q err=%v", got, err)
					}
					w.WriteHeader(http.StatusNoContent)
				}))
				defer healthy.Close()
				var dials atomic.Int32
				dialer := &net.Dialer{Timeout: time.Second}
				tr := &http.Transport{
					DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
						if dials.Add(1) == 1 {
							return nil, &net.OpError{Op: "dial", Net: network, Err: io.EOF}
						}
						return dialer.DialContext(ctx, network, address)
					},
				}
				defer tr.CloseIdleConnections()
				req, err := http.NewRequest(method, healthy.URL, strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req = WithRetries(req, 1, func(*http.Request) bool { return true })
				response, err := NewRetryTransport(tr).RoundTrip(req)
				if response != nil {
					defer func() { _ = response.Body.Close() }()
				}
				if err != nil || dials.Load() != 2 {
					t.Fatalf("before-send failure: attempts=%d err=%v; want failover success", dials.Load(), err)
				}
			})
		}
	}
}

func TestRetryTransportSentNonIdempotentRequestsIgnoreClientKey(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPatch, "LOCK"} {
		for _, key := range []string{"", "Idempotency-Key", "X-Idempotency-Key"} {
			t.Run(method+"/"+key, func(t *testing.T) {
				var first, second atomic.Int32
				dropped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					first.Add(1)
					c, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = c.Close()
				}))
				defer dropped.Close()
				healthy := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, _ *http.Request) { second.Add(1); w.WriteHeader(http.StatusNoContent) },
					),
				)
				defer healthy.Close()
				tr := &http.Transport{}
				defer tr.CloseIdleConnections()
				req, err := http.NewRequest(method, dropped.URL+"/orders", nil)
				if err != nil {
					t.Fatal(err)
				}
				if key != "" {
					req.Header.Set(key, "client-chosen")
				}
				nextURL, err := url.Parse(healthy.URL + "/orders")
				if err != nil {
					t.Fatal(err)
				}
				req = WithRetries(req, 1, func(r *http.Request) bool { r.URL = nextURL; return true })
				response, err := NewRetryTransport(tr).RoundTrip(req)
				if response != nil {
					defer func() { _ = response.Body.Close() }()
				}
				if first.Load() != 1 || second.Load() != 0 || err == nil {
					t.Fatalf(
						"sent request: first=%d second=%d err=%v; want one attempt and terminal error",
						first.Load(),
						second.Load(),
						err,
					)
				}
			})
		}
	}
}

func TestRetryTransportPartialWriteDoesNotReplayPOST(t *testing.T) {
	attempts := 0
	transport := NewRetryTransport(roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		attempts++
		// WroteHeaders may not run when a large header fails partway through.
		httptrace.ContextClientTrace(r.Context()).WroteRequest(httptrace.WroteRequestInfo{Err: io.ErrUnexpectedEOF})
		return nil, io.ErrUnexpectedEOF
	}))
	request, _ := http.NewRequest(http.MethodPost, "http://upstream.test/", nil)
	request = WithRetries(request, 1, func(*http.Request) bool { return true })
	_, err := transport.RoundTrip(request)
	if err == nil || attempts != 1 {
		t.Fatalf("partial write: attempts=%d err=%v", attempts, err)
	}
}

func TestRetryTransportSentKeyedPOSTCannotReplayOnPooledConnection(t *testing.T) {
	for _, key := range []string{"Idempotency-Key", "X-Idempotency-Key"} {
		t.Run(key, func(t *testing.T) {
			var posts atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if r.Header.Get(key) != "client-key" {
					t.Errorf("upstream lost %s", key)
				}
				if posts.Add(1) == 1 {
					c, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = c.Close()
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer backend.Close()
			base := &http.Transport{}
			defer base.CloseIdleConnections()
			transport := NewRetryTransport(base)
			warm, _ := http.NewRequest(http.MethodGet, backend.URL, nil)
			response, err := transport.RoundTrip(warm)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			request, _ := http.NewRequest(http.MethodPost, backend.URL, nil)
			request.Header.Set(key, "client-key")
			response, err = transport.RoundTrip(request)
			if response != nil {
				_ = response.Body.Close()
			}
			if err == nil || posts.Load() != 1 {
				t.Fatalf("sent POST was implicitly replayed: posts=%d err=%v", posts.Load(), err)
			}
			if request.Header.Get(key) != "client-key" {
				t.Fatal("original request header changed")
			}
		})
	}
}
