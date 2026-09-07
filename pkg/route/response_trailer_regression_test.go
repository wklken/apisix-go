package route

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wklken/apisix-go/pkg/resource"
)

func TestUpstreamTrailersFollowHTTPAndGRPCContract(t *testing.T) {
	for _, tc := range []struct {
		name, scheme string
		announced    bool
		want         string
	}{
		{"HTTP announced", "http", true, ""},
		{"HTTP unannounced", "http", false, ""},
		{"gRPC TLS", "grpcs", true, "trail-value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Upstream-Protocol", r.Proto)
				if tc.announced {
					w.Header().Set("Trailer", "X-Upstream-Trailer")
				}
				_, _ = io.WriteString(w, "body")
				w.(http.Flusher).Flush()
				name := "X-Upstream-Trailer"
				if !tc.announced {
					name = http.TrailerPrefix + name
				}
				w.Header().Set(name, "trail-value")
			}))
			if tc.scheme == "grpcs" {
				backend.EnableHTTP2 = true
				backend.StartTLS()
			} else {
				backend.Start()
			}
			defer backend.Close()
			handler := testPreparedProxyHandler(
				t,
				resource.Route{
					Upstream: resource.Upstream{
						Scheme: tc.scheme,
						Nodes:  []resource.Node{upstreamNode(t, backend.URL)},
					},
				},
				resource.Service{},
				testEffectiveConfig(),
			)
			gateway := httptest.NewServer(handler)
			defer gateway.Close()
			response, err := gateway.Client().Get(gateway.URL)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != 200 || string(body) != "body" {
				t.Fatalf("response=%d/%q", response.StatusCode, body)
			}
			if _, ok := response.Trailer["X-Upstream-Trailer"]; tc.announced && !ok {
				t.Fatal("upstream trailer announcement lost")
			}
			if got := response.Trailer.Get("X-Upstream-Trailer"); got != tc.want {
				t.Fatalf("trailer=%q want %q", got, tc.want)
			}
			if tc.scheme == "grpcs" && response.Header.Get("X-Upstream-Protocol") != "HTTP/2.0" {
				t.Fatal("gRPC did not use HTTP2")
			}
		})
	}
}
