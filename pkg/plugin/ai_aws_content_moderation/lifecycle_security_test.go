package ai_aws_content_moderation

import (
	"context"
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

	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/store"
)

func TestScopedSecretsRejectResolvedBlankCredentialsAndRetryExactFields(t *testing.T) {
	const (
		rawAccess = "$ENV://AWS_VALIDATION_ACCESS"
		rawSecret = "$secret://vault/aws/validation-secret"
		rawToken  = "$secret://vault/aws/validation-token"
	)
	for _, test := range []struct {
		name       string
		invalidRaw string
		blank      string
		wantFields []string
	}{
		{
			name:       "required access key is empty",
			invalidRaw: rawAccess,
			blank:      "",
			wantFields: []string{"comprehend.access_key_id"},
		},
		{
			name:       "required secret key is whitespace",
			invalidRaw: rawSecret,
			blank:      " \t\n",
			wantFields: []string{"comprehend.access_key_id", "comprehend.secret_access_key"},
		},
		{
			name:       "present optional token resolves whitespace",
			invalidRaw: rawToken,
			blank:      "   ",
			wantFields: []string{
				"comprehend.access_key_id",
				"comprehend.secret_access_key",
				"comprehend.session_token",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			values := map[string]string{
				rawAccess: "validation-access",
				rawSecret: "validation-secret",
				rawToken:  "validation-token",
			}
			values[test.invalidRaw] = test.blank
			capabilityValue, scope, broker, closeAttempt := newScopedSecretHarness(t, name, values)
			defer closeAttempt()
			p := &Plugin{config: awsScopedConfig(rawAccess, rawSecret, rawToken, "http://127.0.0.1")}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}

			err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
			if err == nil || err.Error() != "materialize plugin secrets: credential unavailable" {
				t.Fatalf("blank resolved credential error = %v, want redacted credential unavailable", err)
			}
			calls := broker.callsSnapshot()
			if len(calls) != len(test.wantFields) {
				t.Fatalf("broker calls = %#v, want fields %v", calls, test.wantFields)
			}
			for index, field := range test.wantFields {
				if calls[index].Scope.Field != field {
					t.Fatalf("broker call[%d] field = %q, want %q", index, calls[index].Scope.Field, field)
				}
			}
			if p.config.Comprehend.AccessKeyID != rawAccess ||
				p.config.Comprehend.SecretAccessKey != rawSecret ||
				p.config.Comprehend.SessionToken != rawToken ||
				p.legacySet || p.scopedSet || p.scopedSessionTokenSet ||
				p.accessKeyID != nil || p.secretAccessKey != nil || p.sessionToken != nil ||
				p.scopedAccessKeyID != (secret.Value{}) ||
				p.scopedSecretAccessKey != (secret.Value{}) ||
				p.scopedSessionToken != (secret.Value{}) {
				t.Fatalf("blank resolved credential installed state: config=%#v", p.config.Comprehend)
			}

			broker.setValue(test.invalidRaw, "validation-retry")
			broker.resetCalls()
			if err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			); err != nil {
				t.Fatalf("same-instance retry error = %v", err)
			}
			assertAWSScopedCalls(t, scope, broker.callsSnapshot(),
				[]string{
					"comprehend.access_key_id",
					"comprehend.secret_access_key",
					"comprehend.session_token",
				},
				[]string{rawAccess, rawSecret, rawToken},
			)
			resolved := map[string]string{
				rawAccess: "validation-access",
				rawSecret: "validation-secret",
				rawToken:  "validation-token",
			}
			resolved[test.invalidRaw] = "validation-retry"
			assertAWSDescriptors(t, p, resolved[rawAccess], resolved[rawSecret], resolved[rawToken])
		})
	}
}

func TestMaterializeSecretsRejectsResolvedBlankCredentialsAndRetries(t *testing.T) {
	const (
		accessEnv = "AWS_LEGACY_VALIDATION_ACCESS"
		secretEnv = "AWS_LEGACY_VALIDATION_SECRET"
		tokenEnv  = "AWS_LEGACY_VALIDATION_TOKEN"
	)
	for _, test := range []struct {
		name       string
		invalidEnv string
		blank      string
	}{
		{name: "required access key is empty", invalidEnv: accessEnv, blank: ""},
		{name: "required secret key is whitespace", invalidEnv: secretEnv, blank: " \t"},
		{name: "present optional token resolves whitespace", invalidEnv: tokenEnv, blank: "  "},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(accessEnv, "legacy-access")
			t.Setenv(secretEnv, "legacy-secret")
			t.Setenv(tokenEnv, "legacy-token")
			t.Setenv(test.invalidEnv, test.blank)
			p := &Plugin{config: awsScopedConfig(
				"$ENV://"+accessEnv,
				"$ENV://"+secretEnv,
				"$ENV://"+tokenEnv,
				"http://127.0.0.1",
			)}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}
			original := p.config.Comprehend

			if err := p.MaterializeSecrets(); !errors.Is(err, errAWSCredentialsUnavailable) {
				t.Fatalf("MaterializeSecrets() error = %v, want redacted credential unavailable", err)
			}
			if p.config.Comprehend != original || p.legacySet || p.scopedSet ||
				p.accessKeyID != nil || p.secretAccessKey != nil || p.sessionToken != nil {
				t.Fatalf("blank legacy credential installed state: config=%#v", p.config.Comprehend)
			}

			t.Setenv(test.invalidEnv, "legacy-retry")
			if err := p.MaterializeSecrets(); err != nil {
				t.Fatalf("same-instance MaterializeSecrets() retry error = %v", err)
			}
			resolved := map[string]string{
				accessEnv: "legacy-access",
				secretEnv: "legacy-secret",
				tokenEnv:  "legacy-token",
			}
			resolved[test.invalidEnv] = "legacy-retry"
			assertAWSDescriptors(t, p, resolved[accessEnv], resolved[secretEnv], resolved[tokenEnv])
			p.Stop()
		})
	}
}

func TestScopedSecretsConcurrentMaterializeResolvesOnce(t *testing.T) {
	const (
		rawAccess = "$ENV://AWS_SINGLEFLIGHT_ACCESS"
		rawSecret = "$ENV://AWS_SINGLEFLIGHT_SECRET"
		rawToken  = "$ENV://AWS_SINGLEFLIGHT_TOKEN"
		workers   = 32
	)
	capabilityValue, scope, broker, closeAttempt := newScopedSecretHarness(t, name, map[string]string{
		rawAccess: "singleflight-access",
		rawSecret: "singleflight-secret",
		rawToken:  "singleflight-token",
	})
	defer closeAttempt()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	broker.setHook(func(call scopedSecretCall) {
		if call.Scope.Field == "comprehend.access_key_id" {
			once.Do(func() { close(entered) })
			<-release
		}
	})
	p := &Plugin{config: awsScopedConfig(rawAccess, rawSecret, rawToken, "http://127.0.0.1")}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	preparationWaiting := make(chan struct{})
	var preparationOnce sync.Once
	p.setAWSCredentialTestHooks(awsCredentialTestHooks{
		preparation: func(kind awsPreparationKind, phase awsPreparationPhase) {
			if kind == awsPreparationScoped && phase == awsPreparationWaiting {
				preparationOnce.Do(func() { close(preparationWaiting) })
			}
		},
	})
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			<-start
			errs <- base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
		})
	}
	close(start)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first scoped resolution")
	}
	select {
	case <-preparationWaiting:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("timed out waiting for a scoped materializer at the preparation gate")
	}
	state := p.awsCredentialLifecycleSnapshot()
	if !state.preparationActive || state.preparationWaiters == 0 {
		close(release)
		t.Fatalf("preparation state = %#v, want active leader and waiting follower", state)
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent scoped materialization error = %v", err)
		}
	}
	assertAWSScopedCalls(t, scope, broker.callsSnapshot(),
		[]string{"comprehend.access_key_id", "comprehend.secret_access_key", "comprehend.session_token"},
		[]string{rawAccess, rawSecret, rawToken},
	)
}

func TestConcurrentScopedThenLegacyMaterializeKeepsFirstPreparation(t *testing.T) {
	const (
		rawAccess = "$ENV://AWS_CROSS_MODE_ACCESS_NOT_SET"
		rawSecret = "$ENV://AWS_CROSS_MODE_SECRET_NOT_SET"
		rawToken  = "$ENV://AWS_CROSS_MODE_TOKEN_NOT_SET"
	)
	capabilityValue, scope, broker, closeAttempt := newScopedSecretHarness(t, name, map[string]string{
		rawAccess: "cross-mode-access",
		rawSecret: "cross-mode-secret",
		rawToken:  "cross-mode-token",
	})
	defer closeAttempt()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	broker.setHook(func(call scopedSecretCall) {
		if call.Scope.Field == "comprehend.access_key_id" {
			once.Do(func() { close(entered) })
			<-release
		}
	})
	p := &Plugin{config: awsScopedConfig(rawAccess, rawSecret, rawToken, "http://127.0.0.1")}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	legacyWaiting := make(chan struct{})
	var preparationOnce sync.Once
	p.setAWSCredentialTestHooks(awsCredentialTestHooks{
		preparation: func(kind awsPreparationKind, phase awsPreparationPhase) {
			if kind == awsPreparationLegacy && phase == awsPreparationWaiting {
				preparationOnce.Do(func() { close(legacyWaiting) })
			}
		},
	})
	scopedDone := make(chan error, 1)
	go func() {
		scopedDone <- base.MaterializeScopedPluginSecrets(
			context.Background(), scope, capabilityValue, p,
		)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for scoped cross-mode resolution")
	}
	legacyDone := make(chan error, 1)
	go func() {
		legacyDone <- p.MaterializeSecrets()
	}()
	select {
	case <-legacyWaiting:
	case <-time.After(time.Second):
		close(release)
		<-scopedDone
		t.Fatal("timed out waiting for legacy materializer at the preparation gate")
	}
	state := p.awsCredentialLifecycleSnapshot()
	if !state.preparationActive || state.preparationWaiters == 0 {
		close(release)
		<-scopedDone
		t.Fatalf("cross-mode preparation state = %#v, want active leader and waiting follower", state)
	}
	select {
	case err := <-legacyDone:
		close(release)
		<-scopedDone
		t.Fatalf("legacy cross-mode follower returned before scoped preparation: %v", err)
	default:
	}
	close(release)
	if err := <-scopedDone; err != nil {
		t.Fatalf("scoped cross-mode materialization error = %v", err)
	}
	if err := <-legacyDone; err != nil {
		t.Fatalf("legacy cross-mode follower error = %v", err)
	}
	assertAWSScopedCalls(t, scope, broker.callsSnapshot(),
		[]string{"comprehend.access_key_id", "comprehend.secret_access_key", "comprehend.session_token"},
		[]string{rawAccess, rawSecret, rawToken},
	)
	if !p.scopedSet || p.legacySet || p.accessKeyID != nil || p.secretAccessKey != nil || p.sessionToken != nil {
		t.Fatalf(
			"cross-mode follower state: scoped=%v legacy=%v handles=(%v,%v,%v)",
			p.scopedSet,
			p.legacySet,
			p.accessKeyID != nil,
			p.secretAccessKey != nil,
			p.sessionToken != nil,
		)
	}
	p.Stop()
}

func TestMaterializeSecretsConcurrentCallsAreIdempotent(t *testing.T) {
	const (
		accessEnv = "AWS_LEGACY_SINGLEFLIGHT_ACCESS"
		secretEnv = "AWS_LEGACY_SINGLEFLIGHT_SECRET"
		tokenEnv  = "AWS_LEGACY_SINGLEFLIGHT_TOKEN"
		workers   = 64
	)
	t.Setenv(accessEnv, "legacy-singleflight-access")
	t.Setenv(secretEnv, "legacy-singleflight-secret")
	t.Setenv(tokenEnv, "legacy-singleflight-token")
	p := &Plugin{config: awsScopedConfig(
		"$ENV://"+accessEnv,
		"$ENV://"+secretEnv,
		"$ENV://"+tokenEnv,
		"http://127.0.0.1",
	)}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			<-start
			errs <- p.MaterializeSecrets()
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent legacy materialization error = %v", err)
		}
	}
	if !p.legacySet || p.scopedSet || p.accessKeyID == nil || p.secretAccessKey == nil || p.sessionToken == nil {
		t.Fatalf(
			"concurrent legacy state: legacy=%v scoped=%v handles=(%v,%v,%v)",
			p.legacySet,
			p.scopedSet,
			p.accessKeyID != nil,
			p.secretAccessKey != nil,
			p.sessionToken != nil,
		)
	}
	assertAWSDescriptors(
		t, p, "legacy-singleflight-access", "legacy-singleflight-secret", "legacy-singleflight-token",
	)
	owners := []*store.ResolvedSecret{p.accessKeyID, p.secretAccessKey, p.sessionToken}
	p.Stop()
	for index, owner := range owners {
		if got := owner.Bytes(); got != nil {
			t.Fatalf("legacy singleflight owner[%d] after Stop() = %q, want nil", index, got)
		}
	}
}

func TestScopedSecretsStopDuringMaterializeDoesNotRevive(t *testing.T) {
	const (
		rawAccess = "$ENV://AWS_STOP_RACE_ACCESS"
		rawSecret = "$ENV://AWS_STOP_RACE_SECRET"
		rawToken  = "$ENV://AWS_STOP_RACE_TOKEN"
	)
	capabilityValue, scope, broker, closeAttempt := newScopedSecretHarness(t, name, map[string]string{
		rawAccess: "stop-race-access",
		rawSecret: "stop-race-secret",
		rawToken:  "stop-race-token",
	})
	defer closeAttempt()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	broker.setHook(func(call scopedSecretCall) {
		if call.Scope.Field == "comprehend.access_key_id" {
			once.Do(func() { close(entered) })
			<-release
		}
	})
	p := &Plugin{config: awsScopedConfig(rawAccess, rawSecret, rawToken, "http://127.0.0.1")}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	materializeDone := make(chan error, 1)
	go func() {
		materializeDone <- base.MaterializeScopedPluginSecrets(
			context.Background(), scope, capabilityValue, p,
		)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for scoped resolution")
	}
	p.Stop()
	close(release)
	if err := <-materializeDone; err == nil || err.Error() != "materialize plugin secrets: credential unavailable" {
		t.Fatalf("materialization racing Stop() error = %v, want redacted terminal failure", err)
	}
	if p.legacySet || p.scopedSet || p.scopedSessionTokenSet ||
		p.accessKeyID != nil || p.secretAccessKey != nil || p.sessionToken != nil ||
		p.scopedAccessKeyID != (secret.Value{}) ||
		p.scopedSecretAccessKey != (secret.Value{}) ||
		p.scopedSessionToken != (secret.Value{}) {
		t.Fatal("materialization revived stopped plugin credential state")
	}
	callCount := len(broker.callsSnapshot())
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err == nil {
		t.Fatal("scoped materialization after Stop() error = nil, want terminal failure")
	}
	if got := len(broker.callsSnapshot()); got != callCount {
		t.Fatalf("broker calls after terminal Stop() = %d, want %d", got, callCount)
	}
	if err := p.MaterializeSecrets(); !errors.Is(err, errAWSCredentialsUnavailable) {
		t.Fatalf("legacy materialization after Stop() error = %v, want terminal failure", err)
	}
}

type retainingRoundTripper struct {
	request            *http.Request
	response           *http.Response
	requestBody        *retainingBody
	responseBody       *retainingBody
	signedHeaderValues [][]string
	signed             bool
}

func (transport *retainingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	transport.request = req
	transport.signedHeaderValues = [][]string{
		req.Header["Authorization"],
		req.Header["X-Amz-Security-Token"],
		req.Header["X-Amz-Date"],
	}
	transport.signed = true
	for _, values := range transport.signedHeaderValues {
		if len(values) == 0 || values[0] == "" {
			transport.signed = false
		}
	}
	if req.Body != nil {
		transport.requestBody = &retainingBody{ReadCloser: req.Body}
		req.Body = transport.requestBody
		if _, err := io.ReadAll(req.Body); err != nil {
			return nil, err
		}
		if err := req.Body.Close(); err != nil {
			return nil, err
		}
	}
	transport.responseBody = &retainingBody{ReadCloser: io.NopCloser(strings.NewReader(
		`{"ResultList":[{"Toxicity":0,"Labels":[]}]}`,
	))}
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       transport.responseBody,
		Request:    req,
	}
	transport.response = response
	return response, nil
}

type retainingBody struct {
	io.ReadCloser
	read []byte
}

func (body *retainingBody) Read(buffer []byte) (int, error) {
	count, err := body.ReadCloser.Read(buffer)
	body.read = append(body.read, buffer[:count]...)
	return count, err
}

func (body *retainingBody) Close() error {
	return body.ReadCloser.Close()
}

func TestScopedSecretsSigV4ArtifactsAreClearedFromRetainedObjects(t *testing.T) {
	const (
		accessKeyID     = "retained-scan-access"
		secretAccessKey = "retained-scan-secret"
		sessionToken    = "retained-scan-token"
	)
	p, closeAttempt := newScopedAWSPlugin(
		t, "http://127.0.0.1", 31, "retained-artifacts", accessKeyID, secretAccessKey, sessionToken,
	)
	defer closeAttempt()
	transport := &retainingRoundTripper{}
	p.client.Transport = transport
	p.now = func() time.Time { return time.Unix(1, 0) }
	if _, err := p.detectToxicContent(
		httptest.NewRequest(http.MethodPost, "/", nil), "hello",
	); err != nil {
		t.Fatalf("detectToxicContent() error = %v", err)
	}
	if !transport.signed {
		t.Fatal("transport did not observe complete SigV4 headers")
	}
	p.Stop()
	for index, values := range transport.signedHeaderValues {
		for valueIndex, value := range values {
			if value != "" {
				t.Fatalf("retained signed header[%d][%d] = %q, want cleared in place", index, valueIndex, value)
			}
		}
	}
	for _, req := range []*http.Request{transport.request, transport.response.Request} {
		if req == nil {
			t.Fatal("retaining transport did not retain request")
		}
		for _, header := range []string{"Authorization", "X-Amz-Security-Token", "X-Amz-Date"} {
			if values, present := req.Header[header]; present || len(values) != 0 {
				t.Fatalf("retained request header %s = %#v, want removed", header, values)
			}
		}
		if got := req.Header.Get("Content-Type"); got != "application/x-amz-json-1.1" {
			t.Fatalf("retained request Content-Type = %q, want business header preserved", got)
		}
		if got := req.Header.Get("X-Amz-Target"); got != "Comprehend_20171127.DetectToxicContent" {
			t.Fatalf("retained request X-Amz-Target = %q, want business header preserved", got)
		}
	}
	assertObjectGraphExcludes(
		t,
		[]any{transport.request, transport.response, transport, p.client},
		[]string{
			accessKeyID,
			secretAccessKey,
			sessionToken,
			"Authorization",
			"AWS4-HMAC-SHA256",
			"Signature=",
			"X-Amz-Date",
			"19700101T000001Z",
		},
	)
}

func assertObjectGraphExcludes(t *testing.T, roots []any, forbidden []string) {
	t.Helper()
	type visit struct {
		typeName string
		pointer  uintptr
	}
	seen := make(map[visit]struct{})
	var inspect func(reflect.Value, string, int)
	inspect = func(value reflect.Value, path string, depth int) {
		if !value.IsValid() || depth > 48 {
			return
		}
		for value.Kind() == reflect.Interface {
			if value.IsNil() {
				return
			}
			value = value.Elem()
		}
		if value.Kind() == reflect.Pointer && value.CanInterface() {
			switch typed := value.Interface().(type) {
			case *http.Request:
				inspect(reflect.ValueOf(typed.URL), path+".URL", depth+1)
				inspect(reflect.ValueOf(typed.Header), path+".Header", depth+1)
				inspect(reflect.ValueOf(typed.Trailer), path+".Trailer", depth+1)
				inspect(reflect.ValueOf(typed.Host), path+".Host", depth+1)
				inspect(reflect.ValueOf(typed.Body), path+".Body", depth+1)
				if typed.GetBody != nil {
					body, err := typed.GetBody()
					if err != nil {
						t.Fatalf("%s.GetBody() error = %v", path, err)
					}
					bodyBytes, err := io.ReadAll(body)
					_ = body.Close()
					if err != nil {
						t.Fatalf("read %s.GetBody() = %v", path, err)
					}
					inspect(reflect.ValueOf(bodyBytes), path+".GetBody", depth+1)
				}
				return
			case *http.Response:
				inspect(reflect.ValueOf(typed.Header), path+".Header", depth+1)
				inspect(reflect.ValueOf(typed.Trailer), path+".Trailer", depth+1)
				inspect(reflect.ValueOf(typed.Request), path+".Request", depth+1)
				inspect(reflect.ValueOf(typed.Body), path+".Body", depth+1)
				return
			case *http.Client:
				inspect(reflect.ValueOf(typed.Transport), path+".Transport", depth+1)
				return
			case *retainingRoundTripper:
				inspect(reflect.ValueOf(typed.request), path+".request", depth+1)
				inspect(reflect.ValueOf(typed.response), path+".response", depth+1)
				inspect(reflect.ValueOf(typed.requestBody), path+".requestBody", depth+1)
				inspect(reflect.ValueOf(typed.responseBody), path+".responseBody", depth+1)
				inspect(reflect.ValueOf(typed.signedHeaderValues), path+".signedHeaderValues", depth+1)
				return
			case *retainingBody:
				inspect(reflect.ValueOf(typed.read), path+".read", depth+1)
				return
			}
		}
		switch value.Kind() {
		case reflect.Pointer:
			if value.IsNil() {
				return
			}
			key := visit{typeName: value.Type().String(), pointer: value.Pointer()}
			if _, ok := seen[key]; ok {
				return
			}
			seen[key] = struct{}{}
			inspect(value.Elem(), path, depth+1)
		case reflect.String:
			text := value.String()
			for _, sensitive := range forbidden {
				if sensitive != "" && strings.Contains(text, sensitive) {
					t.Fatalf("retained object %s contains %q in %q", path, sensitive, text)
				}
			}
		case reflect.Struct:
			for index := range value.NumField() {
				inspect(value.Field(index), fmt.Sprintf("%s.%s", path, value.Type().Field(index).Name), depth+1)
			}
		case reflect.Map:
			if value.IsNil() {
				return
			}
			iterator := value.MapRange()
			for iterator.Next() {
				inspect(iterator.Key(), path+".key", depth+1)
				inspect(iterator.Value(), path+".value", depth+1)
			}
		case reflect.Array, reflect.Slice:
			if value.Type().Elem().Kind() == reflect.Uint8 {
				bytes := make([]byte, value.Len())
				for index := range value.Len() {
					bytes[index] = byte(value.Index(index).Uint())
				}
				text := string(bytes)
				for _, sensitive := range forbidden {
					if sensitive != "" && strings.Contains(text, sensitive) {
						t.Fatalf("retained byte slice %s contains %q", path, sensitive)
					}
				}
				return
			}
			for index := range value.Len() {
				inspect(value.Index(index), fmt.Sprintf("%s[%d]", path, index), depth+1)
			}
		}
	}
	for index, root := range roots {
		inspect(reflect.ValueOf(root), fmt.Sprintf("root[%d]", index), 0)
	}
}

func TestLegacyAWSStopWaitsForResponseDestroysHandlesAndConcurrentStopsAreSafe(t *testing.T) {
	const (
		accessEnv = "AWS_LEGACY_STOP_ACCESS"
		secretEnv = "AWS_LEGACY_STOP_SECRET"
		tokenEnv  = "AWS_LEGACY_STOP_TOKEN"
	)
	t.Setenv(accessEnv, "legacy-stop-access")
	t.Setenv(secretEnv, "legacy-stop-secret")
	t.Setenv(tokenEnv, "legacy-stop-token")
	responseStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(responseStarted)
		<-releaseResponse
		_, _ = w.Write([]byte(`{"ResultList":[{"Toxicity":0,"Labels":[]}]}`))
	}))
	t.Cleanup(moderation.Close)
	p := newTestPlugin(t, awsScopedConfig(
		"$ENV://"+accessEnv,
		"$ENV://"+secretEnv,
		"$ENV://"+tokenEnv,
		moderation.URL,
	))
	drainStarted := make(chan struct{})
	var drainOnce sync.Once
	p.setAWSCredentialTestHooks(awsCredentialTestHooks{
		lifecycle: func(event awsCredentialLifecycleEvent) {
			if event == awsCredentialDrainStarted {
				drainOnce.Do(func() { close(drainStarted) })
			}
		},
	})
	owners := []*store.ResolvedSecret{p.accessKeyID, p.secretAccessKey, p.sessionToken}
	requestDone := make(chan error, 1)
	go func() {
		_, err := p.detectToxicContent(httptest.NewRequest(http.MethodPost, "/", nil), "hello")
		requestDone <- err
	}()
	select {
	case <-responseStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for legacy request response")
	}

	const stoppers = 16
	stopDone := make(chan struct{}, stoppers)
	for range stoppers {
		go func() {
			p.Stop()
			stopDone <- struct{}{}
		}()
	}
	select {
	case <-drainStarted:
	case <-time.After(time.Second):
		close(releaseResponse)
		<-requestDone
		t.Fatal("timed out waiting for legacy Stop() to enter credential drain")
	}
	state := p.awsCredentialLifecycleSnapshot()
	if !state.retired || state.activeUses == 0 {
		close(releaseResponse)
		<-requestDone
		t.Fatalf("legacy Stop() drain state = %#v, want retired with an active use", state)
	}
	select {
	case <-stopDone:
		close(releaseResponse)
		<-requestDone
		t.Fatal("concurrent Stop() returned while legacy request was in flight")
	default:
	}
	close(releaseResponse)
	if err := <-requestDone; err != nil {
		t.Fatalf("legacy in-flight request error = %v", err)
	}
	for range stoppers {
		select {
		case <-stopDone:
		case <-time.After(time.Second):
			t.Fatal("concurrent Stop() did not finish after legacy request")
		}
	}
	for index, owner := range owners {
		if got := owner.Bytes(); got != nil {
			t.Fatalf("legacy owner[%d] bytes after concurrent Stop() = %q, want nil", index, got)
		}
	}
	state = p.awsCredentialLifecycleSnapshot()
	if state.accessKeyIDSet || state.secretAccessKeySet || state.sessionTokenSet || state.legacySet {
		t.Fatal("legacy state remained after concurrent Stop()")
	}
}
