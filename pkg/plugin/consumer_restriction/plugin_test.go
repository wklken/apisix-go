package consumer_restriction

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/util"
)

func TestMissingConsumerReturnsOfficialMessage(t *testing.T) {
	whitelist := []string{"alice"}
	p := newTestPlugin(t, Config{Whitelist: &whitelist})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("consumer-restriction should not call the next handler")
	})).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/restricted", nil))

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"message":"Missing authentication or identity verification."}` {
		t.Fatalf("body = %q", got)
	}
	if got := rr.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
}

func TestDefaultRejectMessageIncludesPeriod(t *testing.T) {
	blacklist := []string{"alice"}
	p := newTestPlugin(t, Config{Blacklist: &blacklist})
	req := httptest.NewRequest(http.MethodGet, "/restricted", nil)
	req = ctx.WithApisixVars(req, map[string]string{"$consumer_name": "alice"})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("consumer-restriction should not call the next handler")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"message":"The consumer_name is forbidden."}` {
		t.Fatalf("body = %q", got)
	}
}

func TestCustomRejectMessage(t *testing.T) {
	blacklist := []string{"alice"}
	p := newTestPlugin(t, Config{
		Blacklist:   &blacklist,
		RejectedMsg: "nope",
	})
	req := httptest.NewRequest(http.MethodGet, "/restricted", nil)
	req = ctx.WithApisixVars(req, map[string]string{"$consumer_name": "alice"})

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("consumer-restriction should not call the next handler")
	})).ServeHTTP(rr, req)

	if got := strings.TrimSpace(rr.Body.String()); got != `{"message":"nope"}` {
		t.Fatalf("body = %q", got)
	}
}

func TestConsumerGroupRestrictionUsesAttachedConsumerGroupID(t *testing.T) {
	whitelist := []string{"gold"}
	p := newTestPlugin(t, Config{
		Type:      "consumer_group_id",
		Whitelist: &whitelist,
	})
	req := ctx.WithApisixVars(httptest.NewRequest(http.MethodGet, "/restricted", nil), map[string]string{})
	ctx.AttachConsumer(req, resource.Consumer{Username: "alice", GroupID: "gold"})

	nextCalled := false
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if !nextCalled {
		t.Fatal("consumer-restriction rejected an allowed consumer group")
	}
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestSchemaValidatesAllowedByMethodsEnum(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	valid := map[string]any{
		"allowed_by_methods": []any{map[string]any{
			"user":    "alice",
			"methods": []any{"GET", "PURGE"},
		}},
	}
	if err := util.Validate(valid, p.GetSchema()); err != nil {
		t.Fatalf("valid methods should pass schema: %v", err)
	}

	invalid := map[string]any{
		"allowed_by_methods": []any{map[string]any{
			"user":    "alice",
			"methods": []any{"INVALID"},
		}},
	}
	if err := util.Validate(invalid, p.GetSchema()); err == nil {
		t.Fatal("invalid method should fail schema")
	}
}

func newTestPlugin(t *testing.T, config Config) *Plugin {
	t.Helper()

	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	return p
}
