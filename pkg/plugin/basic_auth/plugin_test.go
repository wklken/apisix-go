package basic_auth

import (
	"encoding/base64"
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
)

var (
	testStoreOnce sync.Once
	testStore     *store.Store
	testEvents    chan *store.Event
)

func setupStore(t *testing.T) {
	t.Helper()

	testStoreOnce.Do(func() {
		testEvents = make(chan *store.Event, 16)
		testStore = store.NewStore(t.TempDir()+"/basic-auth.db", testEvents)
		testStore.Start()
	})
}

func addBasicAuthConsumer(t *testing.T, username, password string) {
	t.Helper()
	setupStore(t)

	consumer := map[string]any{
		"username": username,
		"plugins": map[string]any{
			"basic-auth": map[string]any{
				"username": username,
				"password": password,
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
		if _, err := store.GetConsumerByPluginKey(name, username); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("consumer %q was not indexed for basic-auth", username)
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

func TestHandlerAcceptsBasicAuthAndAttachesConsumer(t *testing.T) {
	addBasicAuthConsumer(t, "basic-user", "secret")
	p := newTestPlugin(t, Config{})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("Authorization", basicHeader("basic-user", "secret"))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "basic-user" {
			t.Fatalf("consumer_name = %v, want basic-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func TestHandlerFailsClosedThenRetriesLateEnvironmentPassword(t *testing.T) {
	const environmentName = "BASIC_AUTH_PLUGIN_LATE_PASSWORD"
	previous, existed := os.LookupEnv(environmentName)
	if err := os.Unsetenv(environmentName); err != nil {
		t.Fatalf("Unsetenv() error = %v", err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(environmentName, previous)
		} else {
			_ = os.Unsetenv(environmentName)
		}
	})

	setupStore(t)
	consumer := map[string]any{
		"username": "basic-late-env-user",
		"plugins": map[string]any{
			"basic-auth": map[string]any{
				"username": "basic-late-env-user",
				"password": "$ENV://" + environmentName,
			},
		},
	}
	body, err := json.Marshal(consumer)
	if err != nil {
		t.Fatalf("marshal consumer: %v", err)
	}
	event := store.NewEvent()
	event.Type = store.EventTypePut
	event.Key = []byte("/apisix/consumers/basic-late-env-user")
	event.Value = body
	testEvents <- event
	testStore.Sync()
	if _, err := testStore.GetConsumerNameByPluginKey(name, "basic-late-env-user"); err != nil {
		t.Fatalf("raw consumer index was not installed: %v", err)
	}

	p := newTestPlugin(t, Config{})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	request = ctx.WithApisixVars(request, map[string]string{})
	request.Header.Set("Authorization", basicHeader("basic-late-env-user", "bar"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("response before provisioning = %d, want 401", response.Code)
	}

	if err := os.Setenv(environmentName, "bar"); err != nil {
		t.Fatalf("Setenv() error = %v", err)
	}
	resolved, err := store.GetConsumerByPluginKey(name, "basic-late-env-user")
	if err != nil {
		t.Fatalf("lookup after provisioning error = %v", err)
	}
	config := resolved.Plugins[name].(map[string]any)
	if got := config["password"]; got != "bar" {
		t.Fatalf("resolved password = %#v, want bar", got)
	}
	request = httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	request = ctx.WithApisixVars(request, map[string]string{})
	request.Header.Set("Authorization", basicHeader("basic-late-env-user", "bar"))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("response after provisioning = %d, want 204; body=%s", response.Code, response.Body.String())
	}
}

func TestHandlerNormalizesWhitespaceInCredentials(t *testing.T) {
	addBasicAuthConsumer(t, "normalized-user", "secret")
	p := newTestPlugin(t, Config{})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("Authorization", basicHeader(" normalized-user ", " sec ret "))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "normalized-user" {
			t.Fatalf("consumer_name = %v, want normalized-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func TestHandlerRejectsMissingAuthorization(t *testing.T) {
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
	if got := rr.Header().Get("WWW-Authenticate"); got != `Basic realm="basic"` {
		t.Fatalf("WWW-Authenticate = %q, want %q", got, `Basic realm="basic"`)
	}
	if !strings.Contains(rr.Body.String(), "Missing authorization in request") {
		t.Fatalf("body = %q, want missing authorization message", rr.Body.String())
	}
}

func TestHandlerFormatsMissingAuthorizationLikeAPISIX(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if got := rr.Body.String(); got != `{"message":"Missing authorization in request"}` {
		t.Fatalf("body = %q, want APISIX response JSON", got)
	}
}

func TestHandlerUsesAnonymousConsumerOnMissingAuthorization(t *testing.T) {
	addBasicAuthConsumer(t, "anonymous-basic-user", "unused")
	p := newTestPlugin(t, Config{AnonymousConsumer: "anonymous-basic-user"})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := ctx.GetApisixVar(r, "$consumer_name"); got != "anonymous-basic-user" {
			t.Fatalf("consumer_name = %v, want anonymous-basic-user", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusNoContent)
	}
}

func TestHandlerUsesConfiguredRealm(t *testing.T) {
	p := newTestPlugin(t, Config{Realm: "secure-zone"})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if got := rr.Header().Get("WWW-Authenticate"); got != `Basic realm="secure-zone"` {
		t.Fatalf("WWW-Authenticate = %q, want configured realm", got)
	}
}

func TestHandlerRejectsMalformedAuthorization(t *testing.T) {
	p := newTestPlugin(t, Config{})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("Authorization", "Bearer token")
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusUnauthorized)
	}
	if strings.Contains(rr.Body.String(), "Missing authorization in request") {
		t.Fatalf("body = %q, want malformed authorization message", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Invalid authorization in request") {
		t.Fatalf("body = %q, want invalid authorization message", rr.Body.String())
	}
}

func TestHandlerHideCredentialsRemovesAuthorizationHeader(t *testing.T) {
	addBasicAuthConsumer(t, "hide-basic-user", "secret")
	hideCredentials := true
	p := newTestPlugin(t, Config{HideCredentials: &hideCredentials})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = ctx.WithApisixVars(req, map[string]string{})
	req.Header.Set("Authorization", basicHeader("hide-basic-user", "secret"))
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization header = %q, want removed", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d; body=%s", rr.Code, http.StatusNoContent, rr.Body.String())
	}
}

func TestParseBasicAuthorizationDiagnostics(t *testing.T) {
	tests := []struct {
		name     string
		header   string
		wantUser string
		wantPass string
		wantErr  string
	}{
		{name: "invalid scheme", header: "Bad_header YmFyOmJhcgo=", wantErr: "Invalid authorization header format"},
		{name: "invalid base64", header: "Basic aca_a", wantErr: "Failed to decode authentication header: aca_a"},
		{name: "missing password", header: "Basic YmFy", wantErr: "Split authorization err: invalid decoded data: bar"},
		{name: "case insensitive", header: "bASiC Zm9vOmJhcg==", wantUser: "foo", wantPass: "bar"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			user, pass, err := parseBasicAuthorization(test.header)
			if test.wantErr != "" {
				if err == nil || err.Error() != test.wantErr {
					t.Fatalf("parseBasicAuthorization() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBasicAuthorization() error = %v", err)
			}
			if user != test.wantUser || pass != test.wantPass {
				t.Fatalf("credentials = %q/%q, want %q/%q", user, pass, test.wantUser, test.wantPass)
			}
		})
	}
}

func basicHeader(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}
