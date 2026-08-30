package aws_lambda

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/function_upstream"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
)

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	capabilityValue, scope, _, cleanup := newAWSLambdaScopedSecretHarness(t, 1, "test-route", cfg, nil)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
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

func TestMaterializeScopedSecretsOwnsAWSLambdaCredentials(t *testing.T) {
	contextual, err := testutil.DataEncryptionService(true, []string{"0123456789abcdef"}).
		EncryptForContext("resolved-contextual-secret", "aws-lambda.authorization.iam.secretkey")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		auth       *Authorization
		values     map[string]string
		failRaw    string
		wantFields []string
		wantErr    bool
	}{
		{
			name:       "api only environment",
			auth:       &Authorization{APIKey: "$ENV://AWS_LAMBDA_API_KEY"},
			values:     map[string]string{"$ENV://AWS_LAMBDA_API_KEY": "resolved-api-key"},
			wantFields: []string{"authorization.apikey"},
		},
		{
			name: "iam managed and contextual ciphertext",
			auth: &Authorization{IAM: &IAM{
				AccessKey: "$secret://aws/access", SecretKey: contextual,
			}},
			values: map[string]string{
				"$secret://aws/access": "resolved-access",
				contextual:             "resolved-contextual-secret",
			},
			wantFields: []string{"authorization.iam.accesskey"},
		},
		{
			name: "all literal fields",
			auth: &Authorization{APIKey: "literal-api", IAM: &IAM{
				AccessKey: "literal-access", SecretKey: "literal-secret",
			}},
			wantFields: nil,
		},
		{
			name:    "missing iam member",
			auth:    &Authorization{IAM: &IAM{AccessKey: "access-only"}},
			wantErr: true,
		},
		{
			name: "third field failure is atomic",
			auth: &Authorization{APIKey: "$ENV://AWS_API", IAM: &IAM{
				AccessKey: "$secret://aws/access", SecretKey: "$secret://aws/fail",
			}},
			values: map[string]string{
				"$ENV://AWS_API":       "private-api",
				"$secret://aws/access": "private-access",
				"$secret://aws/fail":   "private-secret",
			},
			failRaw: "$secret://aws/fail",
			wantFields: []string{
				"authorization.apikey",
				"authorization.iam.accesskey",
				"authorization.iam.secretkey",
			},
			wantErr: true,
		},
	}

	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := cloneAuthorization(tt.auth)
			cfg := Config{FunctionURI: "http://lambda.invalid", Authorization: tt.auth}
			capabilityValue, scope, broker, closeAttempt := newAWSLambdaScopedSecretHarness(
				t, uint64(index+1), "aws-lambda-materialize-"+fmt.Sprint(index), cfg, tt.values,
				"0123456789abcdef",
			)
			defer closeAttempt()
			broker.failRaw = tt.failRaw
			p := &Plugin{config: cfg}

			err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			)
			if (err != nil) != tt.wantErr {
				t.Fatalf("MaterializeScopedPluginSecrets() error = %v, wantErr %v", err, tt.wantErr)
			}
			calls := broker.scopedCalls()
			if len(calls) != len(tt.wantFields) {
				t.Fatalf("scoped calls = %#v, want fields %v", calls, tt.wantFields)
			}
			for i, field := range tt.wantFields {
				if calls[i].Scope.Field != field || calls[i].Scope.Plugin != name ||
					calls[i].Scope.Resource.ID != "aws-lambda-materialize-"+fmt.Sprint(index) ||
					calls[i].Scope.Source != capability.SecretPluginConfig {
					t.Fatalf("scoped call %d = %#v, want exact field %q authority", i, calls[i], field)
				}
			}
			if tt.wantErr {
				if !authorizationsEqual(p.config.Authorization, original) {
					t.Fatalf("failed materialization changed config = %#v, want %#v", p.config.Authorization, original)
				}
				if err != nil {
					for _, forbidden := range []string{tt.failRaw, "private-api", "private-access", "private-secret"} {
						if forbidden != "" && strings.Contains(err.Error(), forbidden) {
							t.Fatalf("materialization error leaked %q: %v", forbidden, err)
						}
					}
				}
				return
			}
			assertAWSLambdaAuthorizationDescriptors(t, p.config.Authorization, original, tt.values)
		})
	}
}

func TestMaterializeScopedSecretsRejectsResolvedAWSLambdaCredentialsAndRetries(t *testing.T) {
	tests := []struct {
		name       string
		auth       *Authorization
		raw        string
		resolved   string
		wantCalls  int
		wantFields []string
	}{
		{
			name: "api key",
			auth: &Authorization{APIKey: "$ENV://AWS_LAMBDA_EMPTY_API"},
			raw:  "$ENV://AWS_LAMBDA_EMPTY_API", resolved: "", wantCalls: 1,
			wantFields: []string{"authorization.apikey"},
		},
		{
			name: "iam access key",
			auth: &Authorization{IAM: &IAM{
				AccessKey: "$secret://aws/empty-access", SecretKey: "valid-secret",
			}},
			raw: "$secret://aws/empty-access", resolved: " \t\n", wantCalls: 1,
			wantFields: []string{"authorization.iam.accesskey"},
		},
		{
			name: "iam secret key",
			auth: &Authorization{IAM: &IAM{
				AccessKey: "valid-access", SecretKey: "$ENV://AWS_LAMBDA_EMPTY_SECRET",
			}},
			raw: "$ENV://AWS_LAMBDA_EMPTY_SECRET", resolved: "", wantCalls: 1,
			wantFields: []string{"authorization.iam.secretkey"},
		},
	}
	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := cloneAuthorization(tt.auth)
			cfg := Config{FunctionURI: "http://lambda.invalid", Authorization: tt.auth}
			capabilityValue, scope, broker, closeAttempt := newAWSLambdaScopedSecretHarness(
				t, uint64(index+30), "aws-lambda-empty-"+fmt.Sprint(index), cfg,
				map[string]string{tt.raw: tt.resolved},
			)
			defer closeAttempt()
			p := &Plugin{config: cfg}

			err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
			if !errors.Is(err, secret.ErrCredentialUnavailable) {
				t.Fatalf("MaterializeScopedPluginSecrets() error = %v, want credential unavailable", err)
			}
			if !authorizationsEqual(p.config.Authorization, original) || p.credentialsInstalledLocked() {
				t.Fatalf("resolved-empty failure retained state: config=%#v plugin=%#v", p.config.Authorization, p)
			}
			calls := broker.scopedCalls()
			if len(calls) != tt.wantCalls {
				t.Fatalf("scoped calls = %#v, want %d", calls, tt.wantCalls)
			}
			for i, wantField := range tt.wantFields {
				if calls[i].Scope.Field != wantField {
					t.Fatalf("call %d field = %q, want %q", i, calls[i].Scope.Field, wantField)
				}
			}

			broker.setValue(tt.raw, "retry-credential")
			if err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			); err != nil {
				t.Fatalf("same-instance retry error = %v", err)
			}
			if !p.credentialsInstalledLocked() {
				t.Fatal("same-instance retry did not install credential state")
			}
		})
	}
}

func TestAWSLambdaAPIKeyPrecedenceAndGenerationIsolation(t *testing.T) {
	newGeneration := func(t *testing.T, revision uint64, apiKey, accessKey, secretKey string) (*Plugin, func()) {
		t.Helper()
		auth := &Authorization{
			APIKey: "$ENV://AWS_LAMBDA_API_" + fmt.Sprint(revision),
			IAM: &IAM{
				AccessKey: "$secret://aws/access/" + fmt.Sprint(revision),
				SecretKey: "$secret://aws/secret/" + fmt.Sprint(revision),
			},
		}
		cfg := Config{FunctionURI: "http://lambda.invalid", Authorization: auth}
		values := map[string]string{
			auth.APIKey: apiKey, auth.IAM.AccessKey: accessKey, auth.IAM.SecretKey: secretKey,
		}
		capabilityValue, scope, _, closeAttempt := newAWSLambdaScopedSecretHarness(
			t, revision, "shared-route", cfg, values,
		)
		p := &Plugin{config: cfg}
		if err := base.MaterializeScopedPluginSecrets(
			context.Background(), scope, capabilityValue, p,
		); err != nil {
			closeAttempt()
			t.Fatal(err)
		}
		return p, closeAttempt
	}

	n, closeN := newGeneration(t, 50, "api-n", "access-n", "secret-n")
	defer closeN()
	next, closeNext := newGeneration(t, 51, "api-next", "access-next", "secret-next")
	defer closeNext()

	for _, test := range []struct {
		name string
		p    *Plugin
		want string
	}{{"n", n, "api-n"}, {"n+1", next, "api-next"}} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://lambda.invalid", strings.NewReader("body"))
			req.Header.Set("X-Api-Key", "client-key")
			req.Header.Set("Authorization", "Bearer client-token")
			test.p.processRequest(req, function_upstream.Config{})
			if got := req.Header.Get("X-Api-Key"); got != test.want {
				t.Fatalf("X-Api-Key = %q, want %q", got, test.want)
			}
			if got := req.Header.Get("Authorization"); got != "Bearer client-token" {
				t.Fatalf("Authorization = %q, want API-key precedence without IAM signing", got)
			}
		})
	}

	n.Stop()
	req := httptest.NewRequest(http.MethodGet, "http://lambda.invalid", nil)
	next.processRequest(req, function_upstream.Config{})
	if got := req.Header.Get("X-Api-Key"); got != "api-next" {
		t.Fatalf("N+1 X-Api-Key after retiring N = %q, want api-next", got)
	}
}

func TestAWSLambdaIAMSignatureGenerationIsolationAndHeaderCleanup(t *testing.T) {
	oldNow := now
	now = func() time.Time { return time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC) }
	defer func() { now = oldNow }()

	newGeneration := func(t *testing.T, revision uint64, accessKey, secretKey string) (*Plugin, func()) {
		t.Helper()
		auth := &Authorization{IAM: &IAM{
			AccessKey: "$secret://aws/access/" + fmt.Sprint(revision),
			SecretKey: "$secret://aws/secret/" + fmt.Sprint(revision),
			AWSRegion: "us-west-2", Service: "lambda",
		}}
		cfg := Config{FunctionURI: "http://lambda.invalid", Authorization: auth}
		capabilityValue, scope, _, closeAttempt := newAWSLambdaScopedSecretHarness(
			t, revision, "same-resource", cfg,
			map[string]string{auth.IAM.AccessKey: accessKey, auth.IAM.SecretKey: secretKey},
		)
		p := &Plugin{config: cfg}
		if err := base.MaterializeScopedPluginSecrets(
			context.Background(), scope, capabilityValue, p,
		); err != nil {
			closeAttempt()
			t.Fatal(err)
		}
		return p, closeAttempt
	}

	n, closeN := newGeneration(t, 60, "AKID-N", "SECRET-N")
	defer closeN()
	next, closeNext := newGeneration(t, 61, "AKID-NEXT", "SECRET-NEXT")
	defer closeNext()
	for _, test := range []struct {
		name       string
		p          *Plugin
		credential string
	}{{"n", n, "AKID-N"}, {"n+1", next, "AKID-NEXT"}} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(
				http.MethodPost, "http://lambda.invalid/path?b=two&a=one", strings.NewReader("payload"),
			)
			for key, value := range map[string]string{
				"Authorization": "old-authorization", "X-Amz-Date": "old-date",
				"X-Amz-Credential": "old-credential", "X-Amz-Signature": "old-signature",
				"X-Amz-SignedHeaders": "old-headers", "X-Amz-Security-Token": "old-token",
				"x-aMz-CoNtEnT-sHa256": "old-content-digest",
			} {
				req.Header.Set(key, value)
			}
			test.p.processRequest(req, function_upstream.Config{})
			want := "AWS4-HMAC-SHA256 Credential=" + test.credential + "/20200102/us-west-2/lambda/aws4_request"
			if got := req.Header.Get("Authorization"); !strings.Contains(got, want) {
				t.Fatalf("Authorization = %q, want %q", got, want)
			}
			if req.URL.RawQuery != "a=one&b=two" {
				t.Fatalf("signed query = %q, want canonical wire order", req.URL.RawQuery)
			}
			for _, key := range []string{
				"X-Amz-Credential", "X-Amz-Signature", "X-Amz-SignedHeaders",
				"X-Amz-Security-Token", "X-Amz-Content-Sha256",
			} {
				if got := req.Header.Get(key); got != "" {
					t.Fatalf("%s = %q, want stale client header removed", key, got)
				}
			}
		})
	}

	n.Stop()
	req := httptest.NewRequest(http.MethodGet, "http://lambda.invalid", nil)
	next.processRequest(req, function_upstream.Config{})
	if got := req.Header.Get("Authorization"); !strings.Contains(got, "Credential=AKID-NEXT/") {
		t.Fatalf("N+1 signature after retiring N = %q", got)
	}
}

func TestAWSLambdaStopRetiresScopedAndLegacyCredentialsFailClosed(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T) (*Plugin, func())
	}{
		{
			name: "scoped",
			prepare: func(t *testing.T) (*Plugin, func()) {
				auth := &Authorization{APIKey: "$ENV://AWS_STOP_API", IAM: &IAM{
					AccessKey: "$secret://aws/stop-access", SecretKey: "$secret://aws/stop-secret",
				}}
				cfg := Config{FunctionURI: "http://lambda.invalid", Authorization: auth}
				capabilityValue, scope, _, closeAttempt := newAWSLambdaScopedSecretHarness(
					t, 70, "aws-lambda-stop-scoped", cfg, map[string]string{
						auth.APIKey: "stop-api", auth.IAM.AccessKey: "stop-access", auth.IAM.SecretKey: "stop-secret",
					},
				)
				p := &Plugin{config: cfg}
				if err := p.Init(); err != nil {
					closeAttempt()
					t.Fatal(err)
				}
				if err := base.MaterializeScopedPluginSecrets(
					context.Background(), scope, capabilityValue, p,
				); err != nil {
					closeAttempt()
					t.Fatal(err)
				}
				if err := p.PostInit(); err != nil {
					closeAttempt()
					t.Fatal(err)
				}
				return p, closeAttempt
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p, cleanup := test.prepare(t)
			defer cleanup()
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					p.Stop()
				})
			}
			wg.Wait()
			p.Stop()

			if !p.retired || p.apiKeySet || p.iamSet || p.apiKey != (secret.Value{}) ||
				p.iamAccessKey != (secret.Value{}) || p.iamSecretKey != (secret.Value{}) {
				t.Fatalf("post-Stop credential state retained: %#v", p)
			}
			if err := p.PostInit(); !errors.Is(err, secret.ErrCredentialUnavailable) {
				t.Fatalf("post-Stop PostInit() error = %v, want credential unavailable", err)
			}

			req := httptest.NewRequest(http.MethodGet, "http://lambda.invalid", nil)
			for field, value := range map[string]string{
				"X-Api-Key": "retained-api", "Authorization": "retained-authorization",
				"X-Amz-Date": "retained-date", "X-Amz-Credential": "retained-credential",
				"X-Amz-Signature": "retained-signature", "X-Amz-SignedHeaders": "retained-headers",
				"X-Amz-Security-Token": "retained-token",
			} {
				req.Header.Set(field, value)
			}
			p.processRequest(req, function_upstream.Config{})
			for _, field := range []string{
				"X-Api-Key", "Authorization", "X-Amz-Date", "X-Amz-Credential",
				"X-Amz-Signature", "X-Amz-SignedHeaders", "X-Amz-Security-Token",
			} {
				if got := req.Header.Get(field); got != "" {
					t.Fatalf("post-Stop %s = %q, want fail-closed cleanup", field, got)
				}
			}
		})
	}
}

func TestAWSLambdaCredentialAndSignatureDataAreAbsentFromSharedClientIdentity(t *testing.T) {
	tests := []struct {
		name      string
		auth      *Authorization
		forbidden []string
	}{
		{
			name:      "api key",
			auth:      &Authorization{APIKey: "identity-private-api"},
			forbidden: []string{"identity-private-api"},
		},
		{
			name: "iam",
			auth: &Authorization{IAM: &IAM{
				AccessKey: "IDENTITY-ACCESS", SecretKey: "identity-private-secret",
				AWSRegion: "us-east-1", Service: "execute-api",
			}},
			forbidden: []string{"IDENTITY-ACCESS", "identity-private-secret"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				FunctionURI: "http://lambda.invalid", Authorization: test.auth,
			})
			client := p.Client
			req := httptest.NewRequest(http.MethodPost, "http://lambda.invalid", strings.NewReader("payload"))
			p.processRequest(req, function_upstream.Config{})
			for _, forbidden := range test.forbidden {
				if awsLambdaObjectGraphContainsExactString(
					reflect.ValueOf(client), forbidden, make(map[uintptr]struct{}), 0,
				) {
					t.Fatalf("shared client identity retained credential %q", forbidden)
				}
			}
			p.Stop()
			for _, forbidden := range test.forbidden {
				if awsLambdaObjectGraphContainsExactString(
					reflect.ValueOf(p), forbidden, make(map[uintptr]struct{}), 0,
				) {
					t.Fatalf("retired plugin retained credential %q", forbidden)
				}
			}
		})
	}
}

func TestRunRequestPhasePublishesUpstreamSource(t *testing.T) {
	lambda := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer lambda.Close()
	p := newTestPlugin(t, Config{FunctionURI: lambda.URL})
	request := httptest.NewRequest(http.MethodGet, "http://example.com/lambda", nil)
	lifecycle := apisixctx.NewRequestLifecycle(time.Now())
	request = apisixctx.WithRequestLifecycle(request, lifecycle)
	response := httptest.NewRecorder()

	result := p.RunRequestPhase(response, request)
	if result.Decision != 1 || result.Source != apisixctx.ResponseSourceUpstream {
		t.Fatalf("result = %+v, want upstream stop", result)
	}
	if lifecycle.ResponseSource() != apisixctx.ResponseSourceUpstream {
		t.Fatalf("source = %q, want upstream", lifecycle.ResponseSource())
	}
}

func TestHandlerOverwritesClientAWSAPIKey(t *testing.T) {
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
	if gotAPIKey != "configured-key" {
		t.Fatalf("X-Api-Key = %q, want configured-key", gotAPIKey)
	}
}

func TestHandlerReplacesClientIAMCredentialHeaders(t *testing.T) {
	oldNow := now
	now = func() time.Time {
		return time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	}
	defer func() { now = oldNow }()

	var got http.Header
	lambda := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer lambda.Close()

	p := newTestPlugin(t, Config{
		FunctionURI: lambda.URL,
		Authorization: &Authorization{
			IAM: &IAM{AccessKey: "AKID", SecretKey: "SECRET", AWSRegion: "us-west-2", Service: "lambda"},
		},
	})

	res := performRequest(p, http.MethodGet, "/aws", "", map[string]string{
		"Authorization":        "Bearer client-token",
		"X-Amz-Date":           "19990101T000000Z",
		"X-Amz-Credential":     "client-credential",
		"X-Amz-Signature":      "client-signature",
		"X-Amz-SignedHeaders":  "host",
		"X-Amz-Security-Token": "client-session",
		"X-Trace":              "keep",
	})

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusNoContent)
	}
	if got == nil {
		t.Fatal("lambda did not receive request")
	}
	wantCredential := "AWS4-HMAC-SHA256 Credential=AKID/20200102/us-west-2/lambda/aws4_request"
	if authorization := got.Get("Authorization"); !strings.Contains(authorization, wantCredential) {
		t.Fatalf("Authorization = %q, want configured credential %q", authorization, wantCredential)
	}
	if got.Get("X-Amz-Date") != "20200102T030405Z" {
		t.Fatalf("X-Amz-Date = %q, want signer value", got.Get("X-Amz-Date"))
	}
	for _, name := range []string{"X-Amz-Credential", "X-Amz-Signature", "X-Amz-SignedHeaders", "X-Amz-Security-Token"} {
		if got.Get(name) != "" {
			t.Errorf("%s = %q, want client credential removed", name, got.Get(name))
		}
	}
	if got.Get("X-Trace") != "keep" {
		t.Errorf("X-Trace = %q, want preserved", got.Get("X-Trace"))
	}
}

func TestHandlerPreservesClientHeadersWithoutGatewayAuth(t *testing.T) {
	var got http.Header
	lambda := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer lambda.Close()

	p := newTestPlugin(t, Config{FunctionURI: lambda.URL})
	res := performRequest(p, http.MethodGet, "/aws", "", map[string]string{
		"Authorization": "Bearer client-token",
		"X-Api-Key":     "client-key",
		"X-Amz-Target":  "client-target",
	})

	if res.Code != http.StatusNoContent {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusNoContent)
	}
	if got.Get("Authorization") != "Bearer client-token" {
		t.Errorf("Authorization = %q, want client value", got.Get("Authorization"))
	}
	if got.Get("X-Api-Key") != "client-key" {
		t.Errorf("X-Api-Key = %q, want client value", got.Get("X-Api-Key"))
	}
	if got.Get("X-Amz-Target") != "client-target" {
		t.Errorf("X-Amz-Target = %q, want client value", got.Get("X-Amz-Target"))
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

func cloneAuthorization(auth *Authorization) *Authorization {
	if auth == nil {
		return nil
	}
	clone := *auth
	if auth.IAM != nil {
		iam := *auth.IAM
		clone.IAM = &iam
	}
	return &clone
}

func authorizationsEqual(got, want *Authorization) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	if got.APIKey != want.APIKey || (got.IAM == nil) != (want.IAM == nil) {
		return false
	}
	return got.IAM == nil || *got.IAM == *want.IAM
}

func assertAWSLambdaAuthorizationDescriptors(
	t *testing.T,
	got *Authorization,
	original *Authorization,
	values map[string]string,
) {
	t.Helper()
	if got == nil || original == nil {
		if got != nil || original != nil {
			t.Fatalf("authorization = %#v, want %#v", got, original)
		}
		return
	}
	if original.APIKey != "" {
		assertAWSLambdaDescriptor(t, got.APIKey, resolveAWSLambdaTestValue(original.APIKey, values))
	}
	if original.IAM != nil {
		if got.IAM == nil {
			t.Fatal("IAM config = nil after successful materialization")
		}
		assertAWSLambdaDescriptor(
			t, got.IAM.AccessKey, resolveAWSLambdaTestValue(original.IAM.AccessKey, values),
		)
		assertAWSLambdaDescriptor(
			t, got.IAM.SecretKey, resolveAWSLambdaTestValue(original.IAM.SecretKey, values),
		)
		if got.IAM.AWSRegion != original.IAM.AWSRegion || got.IAM.Service != original.IAM.Service {
			t.Fatalf("materialization changed non-secret IAM config = %#v, want %#v", got.IAM, original.IAM)
		}
	}
}

func resolveAWSLambdaTestValue(raw string, values map[string]string) string {
	if resolved, ok := values[raw]; ok {
		return resolved
	}
	return raw
}

func assertAWSLambdaDescriptor(t *testing.T, got, plaintext string) {
	t.Helper()
	digest := sha256.Sum256([]byte(plaintext))
	want := "plugin_config#sha256:" + hex.EncodeToString(digest[:])
	if got != want {
		t.Fatalf("descriptor = %q, want %q", got, want)
	}
}

type awsLambdaScopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type awsLambdaScopedSecretBroker struct {
	mu      sync.Mutex
	values  map[string]string
	failRaw string
	calls   []awsLambdaScopedSecretCall
}

func (*awsLambdaScopedSecretBroker) AuthorizeCandidate(
	context.Context, secret.AttemptID, generation.ApplyTicket, generation.PublicationSet,
) error {
	return nil
}

func (*awsLambdaScopedSecretBroker) AuthorizeRecovery(
	context.Context, secret.AttemptID, generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return nil
}

func (broker *awsLambdaScopedSecretBroker) ResolveScoped(
	_ context.Context, scope secret.Scope, raw string,
) (string, error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, awsLambdaScopedSecretCall{Scope: scope, Raw: raw})
	if raw == broker.failRaw {
		return "", fmt.Errorf("resolver failed for %s private-aws-lambda-credential", raw)
	}
	if value, ok := broker.values[raw]; ok {
		return value, nil
	}
	return raw, nil
}

func (*awsLambdaScopedSecretBroker) RevokeAttempt(context.Context, secret.AttemptID) error {
	return nil
}

func (broker *awsLambdaScopedSecretBroker) scopedCalls() []awsLambdaScopedSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]awsLambdaScopedSecretCall(nil), broker.calls...)
}

func (broker *awsLambdaScopedSecretBroker) setValue(raw, value string) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.values[raw] = value
}

func newAWSLambdaScopedSecretHarness(
	t *testing.T,
	revision uint64,
	resourceID string,
	config Config,
	values map[string]string,
	keyring ...string,
) (secret.GenerationCapability, secret.Scope, *awsLambdaScopedSecretBroker, func()) {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: resourceID}
	document, err := json.Marshal(map[string]any{
		"id":      resourceID,
		"plugins": map[string]any{name: config},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{Key: key, Value: document}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate := generation.PublicationCandidate{
		Artifact: generation.GenerationArtifact{
			Domain: generation.DomainHTTP, Revision: revision,
			Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot,
		Closure:  []generation.ResourceKey{key},
		Decisions: []generation.ResourceDecision{{
			Key: key, Disposition: generation.DispositionPublished, Code: "aws-lambda-test",
		}},
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: revision,
		DesiredDigest:   snapshot.Digest(),
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}
	publication := generation.PublicationSet{
		DesiredRevision: revision,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: candidate,
		},
	}
	manifest, err := capability.Load()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := capability.NewSecretDeclarationCatalog(manifest)
	if err != nil {
		t.Fatal(err)
	}
	broker := &awsLambdaScopedSecretBroker{values: values}
	registration, err := testutil.NewSecretMaterializerWithKeyring(broker, catalog, keyring).RegisterCandidate(
		context.Background(), ticket, publication,
	)
	if err != nil {
		t.Fatal(err)
	}
	capabilityValue, err := secret.NewGenerationCapability(registration, revision)
	if err != nil {
		t.Fatal(err)
	}
	scope := secret.Scope{
		Generation: revision,
		Attempt:    registration.AttemptID(),
		Domain:     generation.DomainHTTP,
		Plugin:     name,
		Resource:   key,
		Source:     capability.SecretPluginConfig,
	}
	return capabilityValue, scope, broker, func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Fatalf("close scoped secret registration: %v", err)
		}
	}
}

func awsLambdaObjectGraphContainsExactString(
	value reflect.Value,
	want string,
	visited map[uintptr]struct{},
	depth int,
) bool {
	if !value.IsValid() || depth > 24 {
		return false
	}
	for value.Kind() == reflect.Interface {
		if value.IsNil() {
			return false
		}
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.String:
		return value.String() == want
	case reflect.Pointer:
		if value.IsNil() {
			return false
		}
		pointer := value.Pointer()
		if _, ok := visited[pointer]; ok {
			return false
		}
		visited[pointer] = struct{}{}
		return awsLambdaObjectGraphContainsExactString(value.Elem(), want, visited, depth+1)
	case reflect.Struct:
		for _, field := range value.Fields() {
			if awsLambdaObjectGraphContainsExactString(field, want, visited, depth+1) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		if value.Type().Elem().Kind() == reflect.Uint8 && value.Len() == len(want) {
			matched := true
			for index := range value.Len() {
				if byte(value.Index(index).Uint()) != want[index] {
					matched = false
					break
				}
			}
			if matched {
				return true
			}
		}
		for index := range value.Len() {
			if awsLambdaObjectGraphContainsExactString(value.Index(index), want, visited, depth+1) {
				return true
			}
		}
	case reflect.Map:
		if value.IsNil() {
			return false
		}
		iterator := value.MapRange()
		for iterator.Next() {
			if awsLambdaObjectGraphContainsExactString(iterator.Key(), want, visited, depth+1) ||
				awsLambdaObjectGraphContainsExactString(iterator.Value(), want, visited, depth+1) {
				return true
			}
		}
	}
	return false
}
