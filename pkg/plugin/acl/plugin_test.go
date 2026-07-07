package acl

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/resource"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestHandlerRejectsMissingAuthentication(t *testing.T) {
	p := newTestPlugin(t, Config{
		AllowLabels: map[string][]string{
			"team": {"edge"},
		},
	})

	res := performRequest(p, nil)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusUnauthorized)
	}
	if !strings.Contains(res.Body.String(), "Missing authentication.") {
		t.Fatalf("body = %q, want missing authentication message", res.Body.String())
	}
}

func TestHandlerAllowsConsumerWithAllowedLabel(t *testing.T) {
	p := newTestPlugin(t, Config{
		AllowLabels: map[string][]string{
			"team": {"edge"},
		},
	})

	res := performRequest(p, map[string]any{"team": "edge"})
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
}

func TestHandlerRejectsConsumerWithoutAllowedLabel(t *testing.T) {
	p := newTestPlugin(t, Config{
		AllowLabels: map[string][]string{
			"team": {"edge"},
		},
	})

	res := performRequest(p, map[string]any{"team": "payments"})
	if res.Code != http.StatusForbidden {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusForbidden)
	}
	if !strings.Contains(res.Body.String(), "The consumer is forbidden.") {
		t.Fatalf("body = %q, want forbidden message", res.Body.String())
	}
}

func TestHandlerRejectsConsumerWithDeniedLabel(t *testing.T) {
	p := newTestPlugin(t, Config{
		DenyLabels: map[string][]string{
			"tier": {"blocked"},
		},
		RejectedCode: http.StatusTooManyRequests,
		RejectedMsg:  "blocked tier",
	})

	res := performRequest(p, map[string]any{"tier": "blocked"})
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusTooManyRequests)
	}
	if !strings.Contains(res.Body.String(), "blocked tier") {
		t.Fatalf("body = %q, want custom rejection message", res.Body.String())
	}
}

func TestHandlerParsesCommaSeparatedConsumerLabel(t *testing.T) {
	p := newTestPlugin(t, Config{
		AllowLabels: map[string][]string{
			"groups": {"edge"},
		},
	})

	res := performRequest(p, map[string]any{"groups": "payments, edge, internal"})
	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
}

func performRequest(p *Plugin, labels map[string]any) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	if labels != nil {
		ctx.AttachConsumer(req, resource.Consumer{
			Username: "alice",
			Labels:   labels,
		})
	}

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)
	return rr
}
