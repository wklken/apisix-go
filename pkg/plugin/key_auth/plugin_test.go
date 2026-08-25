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
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/resource"
	"github.com/wklken/apisix-go/pkg/store"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
)

type keyAuthConsumerLookup struct {
	mu      sync.RWMutex
	byKey   map[string]resource.Consumer
	byID    map[string]resource.Consumer
	keyCall []string
	idCall  []string
	closed  bool
}

func (lookup *keyAuthConsumerLookup) ConsumerByPluginKey(plugin, key string) (resource.Consumer, bool) {
	lookup.mu.Lock()
	defer lookup.mu.Unlock()
	lookup.keyCall = append(lookup.keyCall, plugin+"\x00"+key)
	if lookup.closed {
		return resource.Consumer{}, false
	}
	consumer, ok := lookup.byKey[key]
	return consumer, ok
}

func (lookup *keyAuthConsumerLookup) ConsumerByID(id string) (resource.Consumer, bool) {
	lookup.mu.Lock()
	defer lookup.mu.Unlock()
	lookup.idCall = append(lookup.idCall, id)
	if lookup.closed {
		return resource.Consumer{}, false
	}
	consumer, ok := lookup.byID[id]
	return consumer, ok
}

func (*keyAuthConsumerLookup) ConsumerGroupByID(string) (resource.ConsumerGroup, bool) {
	return resource.ConsumerGroup{}, false
}

func (lookup *keyAuthConsumerLookup) close() {
	lookup.mu.Lock()
	defer lookup.mu.Unlock()
	lookup.closed = true
	lookup.byKey = nil
	lookup.byID = nil
}

func newLookupTestPlugin(t *testing.T, cfg Config, lookup base.ConsumerLookup) *Plugin {
	t.Helper()
	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{Consumers: lookup})
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	return p
}

var (
	testStoreOnce sync.Once
	testEvents    chan *store.Event
)

type storeBackedKeyAuthLookup struct{}

func (storeBackedKeyAuthLookup) ConsumerByPluginKey(plugin, key string) (resource.Consumer, bool) {
	consumer, err := store.GetConsumerByPluginKey(plugin, key)
	return consumer, err == nil
}

func (storeBackedKeyAuthLookup) ConsumerByID(id string) (resource.Consumer, bool) {
	consumer, err := store.GetConsumer(id)
	return consumer, err == nil
}

func (storeBackedKeyAuthLookup) ConsumerGroupByID(string) (resource.ConsumerGroup, bool) {
	return resource.ConsumerGroup{}, false
}

func setupStore(t *testing.T) {
	t.Helper()

	testStoreOnce.Do(func() {
		testEvents = make(chan *store.Event, 16)
		s, err := store.GetStore(t.TempDir()+"/key-auth.db", testEvents, testutil.DataEncryptionService(false, nil))
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
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
	p.SetDependencies(base.Dependencies{Consumers: storeBackedKeyAuthLookup{}})
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
}

func TestHandlerUsesInjectedConsumerLookupAuthoritatively(t *testing.T) {
	addKeyAuthConsumer(t, "store-key-user", "overlap-key")
	lookup := &keyAuthConsumerLookup{byKey: map[string]resource.Consumer{
		"overlap-key": {Username: "lookup-key-user", Plugins: map[string]resource.PluginConfig{}},
	}}
	hideCredentials := true
	p := newLookupTestPlugin(t, Config{HideCredentials: &hideCredentials}, lookup)

	t.Run("lookup consumer wins and header precedence is preserved", func(t *testing.T) {
		request := httptest.NewRequest(
			http.MethodGet, "http://example.com/get?apikey=query-poison&keep=1", nil,
		)
		request = ctx.WithApisixVars(request, map[string]string{})
		request.Header.Set("apikey", "overlap-key")
		response := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := ctx.GetApisixVar(r, "$consumer_name"); got != "lookup-key-user" {
				t.Fatalf("consumer_name = %v, want lookup-key-user", got)
			}
			if got := r.Header.Get("apikey"); got != "" {
				t.Fatalf("apikey header = %q, want hidden", got)
			}
			if got := r.URL.Query().Get("apikey"); got != "query-poison" {
				t.Fatalf("query apikey = %q, want untouched header-precedence value", got)
			}
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("response code = %d, want 204; body=%s", response.Code, response.Body.String())
		}
	})

	t.Run("lookup miss never falls through to Store", func(t *testing.T) {
		miss := newLookupTestPlugin(t, Config{}, &keyAuthConsumerLookup{})
		request := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
		request.Header.Set("apikey", "overlap-key")
		response := httptest.NewRecorder()
		miss.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("non-nil lookup miss reached Store poison consumer")
		})).ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("response code = %d, want 401", response.Code)
		}
	})

	lookup.mu.RLock()
	defer lookup.mu.RUnlock()
	if len(lookup.keyCall) != 1 || lookup.keyCall[0] != name+"\x00overlap-key" {
		t.Fatalf("lookup calls = %#v, want exact factory/key", lookup.keyCall)
	}
}

func TestHandlerUsesInjectedAnonymousConsumerByID(t *testing.T) {
	addConsumer(t, "key-lookup-anonymous")
	lookup := &keyAuthConsumerLookup{byID: map[string]resource.Consumer{
		"key-lookup-anonymous": {Username: "lookup-key-anonymous", Plugins: map[string]resource.PluginConfig{}},
	}}
	p := newLookupTestPlugin(t, Config{AnonymousConsumer: "key-lookup-anonymous"}, lookup)
	request := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	request = ctx.WithApisixVars(request, map[string]string{})
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "lookup-key-anonymous" {
			t.Fatalf("consumer_name = %v, want lookup-key-anonymous", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204", response.Code)
	}
	lookup.mu.RLock()
	defer lookup.mu.RUnlock()
	if len(lookup.idCall) != 1 || lookup.idCall[0] != "key-lookup-anonymous" {
		t.Fatalf("anonymous lookup calls = %#v", lookup.idCall)
	}

	miss := newLookupTestPlugin(
		t, Config{AnonymousConsumer: "key-lookup-anonymous"}, &keyAuthConsumerLookup{},
	)
	missResponse := httptest.NewRecorder()
	miss.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("non-nil anonymous lookup miss reached Store poison consumer")
	})).ServeHTTP(missResponse, httptest.NewRequest(http.MethodGet, "http://example.com/get", nil))
	if missResponse.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous miss response code = %d, want 401", missResponse.Code)
	}
}

func TestInjectedLookupInvalidKeyUsesAnonymousAndHidesAllCredentials(t *testing.T) {
	hideCredentials := true
	lookup := &keyAuthConsumerLookup{byID: map[string]resource.Consumer{
		"lookup-anonymous": {Username: "lookup-anonymous", Plugins: map[string]resource.PluginConfig{}},
	}}
	p := newLookupTestPlugin(t, Config{
		HideCredentials: &hideCredentials, AnonymousConsumer: "lookup-anonymous",
	}, lookup)
	request := httptest.NewRequest(
		http.MethodGet, "http://example.com/get?apikey=query-credential&keep=1", nil,
	)
	request = ctx.WithApisixVars(request, map[string]string{})
	request.Header.Set("apikey", "invalid-header-credential")
	response := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "lookup-anonymous" {
			t.Fatalf("consumer_name = %v, want lookup-anonymous", got)
		}
		if r.Header.Get("apikey") != "" || r.URL.Query().Get("apikey") != "" {
			t.Fatal("anonymous fallback retained injected key credentials")
		}
		if got := r.URL.Query().Get("keep"); got != "1" {
			t.Fatalf("keep query = %q, want 1", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want 204; body=%s", response.Code, response.Body.String())
	}
}

func TestKeyAuthConsumerLookupsAreGenerationIsolated(t *testing.T) {
	firstLookup := &keyAuthConsumerLookup{byKey: map[string]resource.Consumer{
		"overlap": {Username: "key-generation-n", Plugins: map[string]resource.PluginConfig{}},
	}}
	secondLookup := &keyAuthConsumerLookup{byKey: map[string]resource.Consumer{
		"overlap": {Username: "key-generation-n-plus-one", Plugins: map[string]resource.PluginConfig{}},
	}}
	first := newLookupTestPlugin(t, Config{}, firstLookup)
	second := newLookupTestPlugin(t, Config{}, secondLookup)

	assertConsumer := func(p *Plugin, want string) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
		request = ctx.WithApisixVars(request, map[string]string{})
		request.Header.Set("apikey", "overlap")
		response := httptest.NewRecorder()
		p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := ctx.GetApisixVar(r, "$consumer_name"); got != want {
				t.Errorf("consumer_name = %v, want %s", got, want)
			}
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Errorf("response code = %d, want 204; body=%s", response.Code, response.Body.String())
		}
	}

	var group sync.WaitGroup
	for range 16 {
		group.Go(func() { assertConsumer(first, "key-generation-n") })
		group.Go(func() { assertConsumer(second, "key-generation-n-plus-one") })
	}
	group.Wait()
	firstLookup.close()
	assertConsumer(second, "key-generation-n-plus-one")
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

func TestHandlerRejectsConsumerLookupError(t *testing.T) {
	setupStore(t)

	consumer := map[string]any{
		"username": "broken-ref-user",
		"plugins": map[string]any{
			"key-auth": map[string]any{
				"key": "$secret://vault/broken",
			},
		},
	}
	body, err := json.Marshal(consumer)
	if err != nil {
		t.Fatalf("marshal consumer: %v", err)
	}
	event := store.NewEvent()
	event.Type = store.EventTypePut
	event.Key = []byte("/apisix/consumers/broken-ref-user")
	event.Value = body
	testEvents <- event
	t.Cleanup(func() {
		deleteEvent := store.NewEvent()
		deleteEvent.Type = store.EventTypeDelete
		deleteEvent.Key = []byte("/apisix/consumers/broken-ref-user")
		testEvents <- deleteEvent
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := store.GetConsumer("broken-ref-user"); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("broken-ref-user consumer was not removed")
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := store.GetConsumer("broken-ref-user"); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("apikey", "$secret://vault/broken")
	rr := httptest.NewRecorder()

	nextCalled := false
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if nextCalled {
		t.Fatal("next handler was invoked on consumer lookup error")
	}
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusUnauthorized, rr.Body.String())
	}
	if got := rr.Header().Get("X-Consumer-Username"); got != "" {
		t.Fatalf("X-Consumer-Username = %q, want unset", got)
	}
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
	if got := rr.Header().Get("WWW-Authenticate"); got != `apikey realm="key"` {
		t.Fatalf("WWW-Authenticate = %q, want default key-auth realm", got)
	}
}

func TestHandlerUsesConfiguredRealm(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Parse(map[string]any{"realm": "my-custom-realm"}, p.Config()); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if got := rr.Header().Get("WWW-Authenticate"); got != `apikey realm="my-custom-realm"` {
		t.Fatalf("WWW-Authenticate = %q, want configured realm", got)
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

func TestHandlerRecordsInvalidKeyProbeDiagnostic(t *testing.T) {
	setupStore(t)
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	var diagnostics []string
	req = ctx.WithAuthProbeDiagnosticRecorder(req, func(message string) {
		diagnostics = append(diagnostics, message)
	})
	req.Header.Set("apikey", "invalid-key-diagnostic")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid key reached downstream")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want 401", rr.Code)
	}
	if len(diagnostics) != 1 || diagnostics[0] != "Invalid API key in request" {
		t.Fatalf("probe diagnostics = %v, want invalid API key response detail", diagnostics)
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
		if !ctx.IsSensitiveQueryName(r, "apikey") {
			t.Fatal("key-auth did not register configured query key")
		}
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

func TestHandlerRegistersCustomQueryNameForLogging(t *testing.T) {
	p := newTestPlugin(t, Config{Query: "custom_key"})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get?custom_key=secret", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid custom key reached downstream")
	})).ServeHTTP(rr, req)

	if !ctx.IsSensitiveQueryName(req, "custom_key") {
		t.Fatal("key-auth did not register custom query name")
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

func TestSchemaValidatesRealm(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	valid := []struct {
		name  string
		realm string
	}{
		{name: "minimum length", realm: " "},
		{name: "printable range boundaries", realm: "!#[]~"},
		{name: "maximum length", realm: strings.Repeat("~", 128)},
	}
	for _, test := range valid {
		t.Run(test.name, func(t *testing.T) {
			if err := util.Validate(map[string]any{"realm": test.realm}, p.GetSchema()); err != nil {
				t.Fatalf("schema rejected realm %q: %v", test.realm, err)
			}
		})
	}

	invalid := []struct {
		name  string
		realm any
	}{
		{name: "non-string", realm: 123},
		{name: "empty", realm: ""},
		{name: "over maximum length", realm: strings.Repeat("a", 129)},
		{name: "quote", realm: `contains"quote`},
		{name: "backslash", realm: `contains\backslash`},
		{name: "non-printable", realm: "contains\nnewline"},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if err := util.Validate(map[string]any{"realm": test.realm}, p.GetSchema()); err == nil {
				t.Fatalf("schema accepted invalid realm %#v", test.realm)
			}
		})
	}
}
