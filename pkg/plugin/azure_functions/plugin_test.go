package azure_functions

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestHandlerInvokesAzureFunctionAndRelaysResponse(t *testing.T) {
	var gotMethod, gotQuery, gotBody, gotKey, gotClientID, gotHost string
	function := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotQuery = r.URL.RawQuery
		gotHost = r.Host
		gotKey = r.Header.Get("X-Functions-Key")
		gotClientID = r.Header.Get("X-Functions-Clientid")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read function request body: %v", err)
		}
		gotBody = string(body)

		w.Header().Set("X-Function-Result", "azure")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte("hello from azure"))
	}))
	defer function.Close()

	p := newTestPlugin(t, Config{
		FunctionURI: function.URL + "/api/HttpTrigger",
		Authorization: &Authorization{
			APIKey:   "function-key",
			ClientID: "client-id",
		},
	})

	res := performRequest(p, http.MethodPost, "/azure?name=APISIX", "payload", nil)

	if res.Code != http.StatusAccepted {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusAccepted)
	}
	if got := res.Body.String(); got != "hello from azure" {
		t.Fatalf("response body = %q, want function body", got)
	}
	if got := res.Header().Get("X-Function-Result"); got != "azure" {
		t.Fatalf("X-Function-Result = %q, want azure", got)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("function method = %q, want POST", gotMethod)
	}
	if gotQuery != "name=APISIX" {
		t.Fatalf("function query = %q, want name=APISIX", gotQuery)
	}
	if gotBody != "payload" {
		t.Fatalf("function body = %q, want payload", gotBody)
	}
	if gotKey != "function-key" {
		t.Fatalf("X-Functions-Key = %q, want function-key", gotKey)
	}
	if gotClientID != "client-id" {
		t.Fatalf("X-Functions-Clientid = %q, want client-id", gotClientID)
	}
	if !strings.Contains(gotHost, "127.0.0.1") {
		t.Fatalf("function Host = %q, want function host", gotHost)
	}
}

func TestHandlerDoesNotOverwriteClientAzureAuthorization(t *testing.T) {
	var gotKey, gotClientID string
	function := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Functions-Key")
		gotClientID = r.Header.Get("X-Functions-Clientid")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer function.Close()

	p := newTestPlugin(t, Config{
		FunctionURI: function.URL,
		Authorization: &Authorization{
			APIKey:   "configured-key",
			ClientID: "configured-client",
		},
	})

	res := performRequest(p, http.MethodGet, "/azure", "", map[string]string{
		"X-Functions-Key":      "client-key",
		"X-Functions-Clientid": "client-client",
	})

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusNoContent)
	}
	if gotKey != "client-key" {
		t.Fatalf("X-Functions-Key = %q, want client-key", gotKey)
	}
	if gotClientID != "client-client" {
		t.Fatalf("X-Functions-Clientid = %q, want client-client", gotClientID)
	}
}

func TestHandlerFallsBackToAzureMetadataAuthorization(t *testing.T) {
	var gotKey, gotClientID string
	function := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Functions-Key")
		gotClientID = r.Header.Get("X-Functions-Clientid")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer function.Close()

	p := newTestPlugin(t, Config{FunctionURI: function.URL})
	p.metadata = Metadata{
		MasterAPIKey:   "master-key",
		MasterClientID: "master-client",
	}
	res := performRequest(p, http.MethodGet, "/azure", "", nil)

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusNoContent)
	}
	if gotKey != "master-key" || gotClientID != "master-client" {
		t.Fatalf("metadata authorization = key:%q client:%q, want master values", gotKey, gotClientID)
	}
}

func performRequest(
	p *Plugin,
	method string,
	path string,
	body string,
	headers map[string]string,
) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://example.com"+path, strings.NewReader(body))
	for field, value := range headers {
		req.Header.Set(field, value)
	}

	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := http.StatusInternalServerError
		http.Error(w, http.StatusText(t), t)
	})).ServeHTTP(rr, req)
	return rr
}
