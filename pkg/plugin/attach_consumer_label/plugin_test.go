package attach_consumer_label

import (
	"net/http"
	"net/http/httptest"
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

func TestHandlerAttachesConfiguredConsumerLabels(t *testing.T) {
	p := newTestPlugin(t, Config{
		Headers: map[string]string{
			"X-Consumer-Department": "$department",
			"X-Consumer-Company":    "$company",
			"X-Consumer-Role":       "$role",
		},
	})

	var gotDepartment, gotCompany, gotRole string
	res := performRequest(p, map[string]any{
		"department": "devops",
		"company":    "api7",
	}, func(r *http.Request) {
		gotDepartment = r.Header.Get("X-Consumer-Department")
		gotCompany = r.Header.Get("X-Consumer-Company")
		gotRole = r.Header.Get("X-Consumer-Role")
	})

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
	if gotDepartment != "devops" {
		t.Fatalf("X-Consumer-Department = %q, want devops", gotDepartment)
	}
	if gotCompany != "api7" {
		t.Fatalf("X-Consumer-Company = %q, want api7", gotCompany)
	}
	if gotRole != "" {
		t.Fatalf("X-Consumer-Role = %q, want empty missing-label header", gotRole)
	}
}

func TestHandlerPassesThroughWithoutAuthenticatedConsumer(t *testing.T) {
	p := newTestPlugin(t, Config{
		Headers: map[string]string{
			"X-Consumer-Department": "$department",
		},
	})

	var gotDepartment string
	res := performRequest(p, nil, func(r *http.Request) {
		gotDepartment = r.Header.Get("X-Consumer-Department")
	})

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", res.Code, http.StatusNoContent, res.Body.String())
	}
	if gotDepartment != "" {
		t.Fatalf("X-Consumer-Department = %q, want empty without authenticated consumer", gotDepartment)
	}
}

func performRequest(p *Plugin, labels map[string]any, inspect func(*http.Request)) *httptest.ResponseRecorder {
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
		if inspect != nil {
			inspect(r)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)
	return rr
}
