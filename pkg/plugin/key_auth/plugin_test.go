package key_auth

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/util"
)

var (
	testStoreOnce sync.Once
	testEvents    chan *store.Event
)

func setupStore(t *testing.T) {
	t.Helper()

	testStoreOnce.Do(func() {
		testEvents = make(chan *store.Event, 16)
		s := store.NewStore(t.TempDir()+"/key-auth.db", testEvents)
		s.Start()
	})
}

func addKeyAuthConsumer(t *testing.T, username, key string) {
	t.Helper()
	setupStore(t)

	consumer := map[string]any{
		"username": username,
		"plugins": map[string]any{
			"key-auth": map[string]any{
				"key": key,
			},
		},
	}
	body, err := json.Marshal(consumer)
	if err != nil {
		t.Fatalf("marshal consumer: %v", err)
	}

	event := store.NewEvent()
	event.Type = store.EventTypePut
	event.Key = []byte("/apisix/consumers/" + username)
	event.Value = body
	testEvents <- event

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := store.GetConsumerByPluginKey(name, key); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("consumer %q was not indexed for key-auth key %q", username, key)
}

func addConsumer(t *testing.T, username string) {
	t.Helper()
	setupStore(t)

	consumer := map[string]any{
		"username": username,
		"plugins":  map[string]any{},
	}
	body, err := json.Marshal(consumer)
	if err != nil {
		t.Fatalf("marshal consumer: %v", err)
	}

	event := store.NewEvent()
	event.Type = store.EventTypePut
	event.Key = []byte("/apisix/consumers/" + username)
	event.Value = body
	testEvents <- event

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := store.GetConsumer(username); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("consumer %q was not stored", username)
}

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

func TestHandlerAcceptsHeaderKeyAndAttachesConsumer(t *testing.T) {
	addKeyAuthConsumer(t, "key-user", "header-key")
	p := newTestPlugin(t, Config{})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	runnerCalled := false
	req = ctx.WithConsumerPluginRunner(req, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		runnerCalled = true
		next.ServeHTTP(w, r)
	})
	req.Header.Set("apikey", "header-key")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "key-user" {
			t.Fatalf("consumer_name = %v, want key-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
	if !runnerCalled {
		t.Fatal("consumer plugin runner was not called")
	}
}

func TestHandlerDoesNotWriteConsumerToStdout(t *testing.T) {
	addKeyAuthConsumer(t, "quiet-key-user", "quiet-key")
	p := newTestPlugin(t, Config{})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("apikey", "quiet-key")
	output := captureStdout(t, func() {
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(httptest.NewRecorder(), req)
	})

	if output != "" {
		t.Fatalf("handler wrote consumer data to stdout: %q", output)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	old := os.Stdout
	os.Stdout = writer
	fn()
	if err := writer.Close(); err != nil {
		t.Fatalf("close stdout pipe: %v", err)
	}
	os.Stdout = old
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read stdout pipe: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("close stdout reader: %v", err)
	}
	return string(output)
}

func TestHandlerRejectsMissingKey(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	if !strings.Contains(rr.Body.String(), "Missing API key in request") {
		t.Fatalf("body = %q, want missing key message", rr.Body.String())
	}
	if got := rr.Body.String(); got != `{"message":"Missing API key in request"}` {
		t.Fatalf("body = %q, want APISIX error JSON", got)
	}
	if got := rr.Header().Get("Content-Type"); got != "text/plain" {
		t.Fatalf("Content-Type = %q, want text/plain", got)
	}
}

func TestHandlerUsesAnonymousConsumerWhenKeyIsMissing(t *testing.T) {
	addConsumer(t, "anonymous-user")
	p := newTestPlugin(t, Config{AnonymousConsumer: "anonymous-user"})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "anonymous-user" {
			t.Fatalf("consumer_name = %v, want anonymous-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func TestHandlerRecordsMissingAnonymousConsumerProbeDiagnostic(t *testing.T) {
	setupStore(t)
	p := newTestPlugin(t, Config{AnonymousConsumer: "missing-key-anonymous"})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	var diagnostics []string
	req = ctx.WithAuthProbeDiagnosticRecorder(req, func(message string) {
		diagnostics = append(diagnostics, message)
	})
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("missing key-auth anonymous consumer reached downstream")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want 401", rr.Code)
	}
	if len(diagnostics) != 1 || diagnostics[0] != "failed to get anonymous consumer missing-key-anonymous" {
		t.Fatalf("probe diagnostics = %v, want missing-anonymous detail", diagnostics)
	}
}

func TestHandlerRejectsInvalidKey(t *testing.T) {
	addKeyAuthConsumer(t, "valid-key-user", "valid-key")
	p := newTestPlugin(t, Config{})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("apikey", "wrong-key")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	if !strings.Contains(rr.Body.String(), "Invalid API key in request") {
		t.Fatalf("body = %q, want invalid key message", rr.Body.String())
	}
	if got := rr.Body.String(); got != `{"message":"Invalid API key in request"}` {
		t.Fatalf("body = %q, want APISIX error JSON", got)
	}
	if got := rr.Header().Get("Content-Type"); got != "text/plain" {
		t.Fatalf("Content-Type = %q, want text/plain", got)
	}
}

func TestHandlerUsesAnonymousConsumerForInvalidKeyAndHidesCredentials(t *testing.T) {
	addKeyAuthConsumer(t, "valid-key-user", "valid-key")
	addConsumer(t, "anonymous-invalid-key-user")
	hideCredentials := true
	p := newTestPlugin(t, Config{
		HideCredentials:   &hideCredentials,
		AnonymousConsumer: "anonymous-invalid-key-user",
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get?apikey=wrong-query&keep=1", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("apikey", "wrong-header")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "anonymous-invalid-key-user" {
			t.Fatalf("consumer_name = %v, want anonymous-invalid-key-user", got)
		}
		if got := r.Header.Get("apikey"); got != "" {
			t.Fatalf("apikey header = %q, want removed", got)
		}
		if got := r.URL.Query().Get("apikey"); got != "" {
			t.Fatalf("apikey query = %q, want removed", got)
		}
		if got := r.URL.Query().Get("keep"); got != "1" {
			t.Fatalf("keep query = %q, want preserved", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func TestHandlerHideCredentialsRemovesQueryKey(t *testing.T) {
	addKeyAuthConsumer(t, "query-user", "query-key")
	hideCredentials := true
	p := newTestPlugin(t, Config{HideCredentials: &hideCredentials})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get?apikey=query-key&keep=1", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("apikey"); got != "" {
			t.Fatalf("apikey query = %q, want removed", got)
		}
		if got := r.URL.Query().Get("keep"); got != "1" {
			t.Fatalf("keep query = %q, want preserved", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func TestSchemaAcceptsAnonymousConsumer(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"anonymous_consumer": "anonymous-user",
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected anonymous_consumer: %v", err)
	}
}
