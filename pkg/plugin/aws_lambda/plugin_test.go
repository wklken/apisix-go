package aws_lambda

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
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

func TestHandlerInvokesAWSLambdaWithAPIKey(t *testing.T) {
	var gotMethod, gotQuery, gotBody, gotAPIKey, gotAuthorization string
	lambda := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotQuery = r.URL.RawQuery
		gotAPIKey = r.Header.Get("X-Api-Key")
		gotAuthorization = r.Header.Get("Authorization")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read lambda request body: %v", err)
		}
		gotBody = string(body)

		w.Header().Set("X-Lambda-Result", "ok")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("lambda body"))
	}))
	defer lambda.Close()

	p := newTestPlugin(t, Config{
		FunctionURI: lambda.URL + "/prod/resource",
		Authorization: &Authorization{
			APIKey: "api-key",
		},
	})

	res := performRequest(p, http.MethodPut, "/aws?name=APISIX", "payload", nil)

	if res.Code != http.StatusCreated {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusCreated)
	}
	if got := res.Body.String(); got != "lambda body" {
		t.Fatalf("response body = %q, want lambda body", got)
	}
	if got := res.Header().Get("X-Lambda-Result"); got != "ok" {
		t.Fatalf("X-Lambda-Result = %q, want ok", got)
	}
	if gotMethod != http.MethodPut {
		t.Fatalf("lambda method = %q, want PUT", gotMethod)
	}
	if gotQuery != "name=APISIX" {
		t.Fatalf("lambda query = %q, want name=APISIX", gotQuery)
	}
	if gotBody != "payload" {
		t.Fatalf("lambda body = %q, want payload", gotBody)
	}
	if gotAPIKey != "api-key" {
		t.Fatalf("X-Api-Key = %q, want api-key", gotAPIKey)
	}
	if gotAuthorization != "" {
		t.Fatalf("Authorization = %q, want empty in API key mode", gotAuthorization)
	}
}

func TestHandlerDoesNotOverwriteClientAWSAPIKey(t *testing.T) {
	var gotAPIKey string
	lambda := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("X-Api-Key")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer lambda.Close()

	p := newTestPlugin(t, Config{
		FunctionURI: lambda.URL,
		Authorization: &Authorization{
			APIKey: "configured-key",
		},
	})

	res := performRequest(p, http.MethodGet, "/aws", "", map[string]string{"X-Api-Key": "client-key"})

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusNoContent)
	}
	if gotAPIKey != "client-key" {
		t.Fatalf("X-Api-Key = %q, want client-key", gotAPIKey)
	}
}

func TestHandlerSignsIAMRequestWithAWSV4(t *testing.T) {
	oldNow := now
	now = func() time.Time {
		return time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	}
	defer func() { now = oldNow }()

	var gotAuthorization, gotAmzDate, gotBody, gotQuery string
	lambda := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		gotAmzDate = r.Header.Get("X-Amz-Date")
		gotQuery = r.URL.RawQuery
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read lambda request body: %v", err)
		}
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("signed"))
	}))
	defer lambda.Close()

	p := newTestPlugin(t, Config{
		FunctionURI: lambda.URL + "/prod/resource",
		Authorization: &Authorization{
			IAM: &IAM{
				AccessKey: "AKID",
				SecretKey: "SECRET",
				AWSRegion: "us-west-2",
				Service:   "lambda",
			},
		},
	})

	res := performRequest(p, http.MethodPost, "/aws?b=two&a=one", `{"ok":true}`, nil)

	if res.Code != http.StatusOK {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusOK)
	}
	if gotBody != `{"ok":true}` {
		t.Fatalf("lambda body = %q, want JSON payload", gotBody)
	}
	if gotAmzDate != "20200102T030405Z" {
		t.Fatalf("X-Amz-Date = %q, want fixed signing date", gotAmzDate)
	}
	if gotQuery != "a=one&b=two" {
		t.Fatalf("lambda query = %q, want canonical wire order", gotQuery)
	}
	wantCredential := "AWS4-HMAC-SHA256 Credential=AKID/20200102/us-west-2/lambda/aws4_request"
	if !strings.Contains(gotAuthorization, wantCredential) {
		t.Fatalf("Authorization = %q, want credential scope %q", gotAuthorization, wantCredential)
	}
	if !strings.Contains(gotAuthorization, "SignedHeaders=host;x-amz-date") {
		t.Fatalf("Authorization = %q, want signed host and x-amz-date headers", gotAuthorization)
	}
	signature := strings.TrimPrefix(gotAuthorization[strings.LastIndex(gotAuthorization, "Signature="):], "Signature=")
	if len(signature) != 64 {
		t.Fatalf("signature length = %d, want 64 hex chars; authorization=%q", len(signature), gotAuthorization)
	}
}

func TestHandlerForwardsMatchedExtensionPath(t *testing.T) {
	var gotPath string
	lambda := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer lambda.Close()

	p := newTestPlugin(t, Config{FunctionURI: lambda.URL + "/prod"})
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("ext", "users/42")
	req := httptest.NewRequest(http.MethodGet, "http://example.com/aws/users/42", nil)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeContext))

	rr := httptest.NewRecorder()
	p.Handler(http.NotFoundHandler()).ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", rr.Code, http.StatusNoContent)
	}
	if gotPath != "/prod/users/42" {
		t.Fatalf("lambda path = %q, want /prod/users/42", gotPath)
	}
}

func TestCanonicalRequestComponentsMatchAPISIXNormalization(t *testing.T) {
	if got := canonicalURI("api//v1/../users/"); got != "/api/users" {
		t.Fatalf("canonicalURI() = %q, want /api/users", got)
	}
	if got := canonicalQueryString("z=last&name=APISIX%20Go&a=first"); got != "a=first&name=APISIX%20Go&z=last" {
		t.Fatalf("canonicalQueryString() = %q, want encoded and sorted query", got)
	}
	complexQuery := "with%20space=a%2Fb%20c&multi=m2&multi=m1&flag&a=*&a-=x"
	wantComplex := "a=%2A&a-=x&flag=&multi=m1&multi=m2&with%20space=a%2Fb%20c"
	if got := canonicalQueryString(complexQuery); got != wantComplex {
		t.Fatalf("canonicalQueryString(complex) = %q, want %q", got, wantComplex)
	}

	req := httptest.NewRequest(http.MethodPost, "https://lambda.example/prod", nil)
	req.Header.Set("Content-Type", " application/json;  charset=utf-8 ")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("X-Amz-Date", "20200102T030405Z")
	req.Header.Set("X-Custom", "  first   second ")
	signed, canonical := canonicalHeaders(req)

	if signed != "content-type;host;x-amz-date;x-custom" {
		t.Fatalf("signed headers = %q, want all forwarded headers except connection", signed)
	}
	wantCanonical := "content-type:application/json; charset=utf-8\n" +
		"host:lambda.example\n" +
		"x-amz-date:20200102T030405Z\n" +
		"x-custom:first second\n"
	if canonical != wantCanonical {
		t.Fatalf("canonical headers = %q, want %q", canonical, wantCanonical)
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
