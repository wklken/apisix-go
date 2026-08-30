package ai_aliyun_content_moderation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
)

type scopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type scopedSecretBroker struct {
	values map[string]string
	fail   map[string]error
	calls  []scopedSecretCall
}

func (broker *scopedSecretBroker) ResolveScoped(
	ctx context.Context, scope secret.Scope, raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	broker.calls = append(broker.calls, scopedSecretCall{Scope: scope, Raw: raw})
	if err := broker.fail[raw]; err != nil {
		return "", err
	}
	if value, ok := broker.values[raw]; ok {
		return value, nil
	}
	return raw, nil
}

func newScopedSecretHarness(
	t testing.TB, factory string, values map[string]string,
) (secret.GenerationSecrets, secret.Scope, *scopedSecretBroker, func()) {
	t.Helper()
	const revision = uint64(7)
	key := generation.ResourceKey{Kind: "routes", ID: "r1"}
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{
		Key: key, Value: []byte(`{"plugins":{}}`),
	}}, nil)
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
			Key: key, Disposition: generation.DispositionPublished, Code: "leaf-test",
		}},
	}
	set := generation.PublicationSet{
		DesiredRevision: revision,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: candidate,
		},
	}
	catalog, err := capability.NewSecretDeclarationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	broker := &scopedSecretBroker{values: maps.Clone(values), fail: make(map[string]error)}
	materialization, err := testutil.NewSecretMaterializer(broker, catalog).
		PrepareGeneration(context.Background(), set)
	if err != nil {
		t.Fatal(err)
	}
	baseScope := secret.Scope{
		Generation: revision,
		Domain:     generation.DomainHTTP,
		Plugin:     factory,
		Resource:   key,
		Source:     capability.SecretPluginConfig,
	}
	return materialization.Secrets(), baseScope, broker, func() {
		if err := materialization.Close(context.Background()); err != nil {
			t.Fatalf("close scoped secret materialization: %v", err)
		}
	}
}

func TestScopedSecretsMaterializeAliyunCredentialsAtomically(t *testing.T) {
	const (
		idRef     = "$ENV://ALIYUN_ACCESS_ID"
		secretRef = "$ENV://ALIYUN_ACCESS_SECRET"
	)
	p := &Plugin{config: Config{
		Endpoint:        "https://moderation.example",
		RegionID:        "cn-shanghai",
		AccessKeyID:     idRef,
		AccessKeySecret: secretRef,
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(t, name, map[string]string{
		idRef:     "resolved-access-id",
		secretRef: "resolved-access-secret",
	})
	defer closeAttempt()
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if len(broker.calls) != 2 {
		t.Fatalf("broker calls = %d, want exactly 2: %#v", len(broker.calls), broker.calls)
	}
	if got := []string{
		broker.calls[0].Scope.Field,
		broker.calls[1].Scope.Field,
	}; !equalStrings(
		got,
		[]string{"access_key_id", "access_key_secret"},
	) {
		t.Fatalf("broker fields = %#v, want manifest order", got)
	}
	if broker.calls[0].Raw != idRef || broker.calls[1].Raw != secretRef {
		t.Fatalf("broker raw calls = %#v, want exact references", broker.calls)
	}
	for field, value := range map[string]string{
		"access_key_id":     p.config.AccessKeyID,
		"access_key_secret": p.config.AccessKeySecret,
	} {
		if !strings.HasPrefix(value, "plugin_config#sha256:") || len(value) != len("plugin_config#sha256:")+64 {
			t.Fatalf("%s config = %q, want descriptor-only value", field, value)
		}
		if strings.Contains(value, "ALIYUN") || strings.Contains(value, "resolved-") ||
			strings.Contains(value, "$ENV") {
			t.Fatalf("%s config leaked secret material: %q", field, value)
		}
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	p.now = func() time.Time { return time.Unix(1, 0) }
	p.nonce = func() string { return "nonce" }
	moderation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read moderation request: %v", err)
		}
		form, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatalf("parse moderation request: %v", err)
		}
		if form.Get("AccessKeyId") != "resolved-access-id" {
			t.Errorf("request AccessKeyId = %q, want resolved-access-id", form.Get("AccessKeyId"))
		}
		if form.Get("Signature") == "" {
			t.Error("request Signature is empty")
		}
		if strings.Contains(string(body), p.config.AccessKeyID) ||
			strings.Contains(string(body), p.config.AccessKeySecret) {
			t.Errorf("request retained descriptor: %q", body)
		}
		_, _ = w.Write([]byte(`{"Data":{"RiskLevel":"low"}}`))
	}))
	defer moderation.Close()
	p.config.Endpoint = moderation.URL
	if statusCode, responseBody, err := p.sendModerationRequest(
		context.Background(), "session", "hello", "llm_query_moderation",
	); err != nil {
		t.Fatalf("sendModerationRequest() error = %v", err)
	} else if statusCode != http.StatusOK || string(responseBody) != `{"Data":{"RiskLevel":"low"}}` {
		t.Fatalf("moderation response = (%d, %q), want 200 and JSON body", statusCode, responseBody)
	}
}

func TestScopedSecretsResolveManagedAliyunCredential(t *testing.T) {
	const managed = "$secret://vault/aliyun/access-key-secret"
	p := &Plugin{config: Config{
		Endpoint:        "https://moderation.example",
		RegionID:        "cn-shanghai",
		AccessKeyID:     "access-id",
		AccessKeySecret: managed,
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(t, name, map[string]string{
		managed: "managed-secret",
	})
	defer closeAttempt()
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if len(broker.calls) != 1 || broker.calls[0].Raw != managed {
		t.Fatalf("broker calls = %#v, want only managed secret", broker.calls)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	if err := p.useAliyunCredentials(func(id, resolvedSecret string) error {
		if id != "access-id" || resolvedSecret != "managed-secret" {
			t.Fatalf("credentials = %q/%q, want resolved managed values", id, resolvedSecret)
		}
		return nil
	}); err != nil {
		t.Fatalf("useAliyunCredentials() error = %v", err)
	}
}

func TestScopedSecretsFailureIsAtomicAndRedacted(t *testing.T) {
	const (
		idRef     = "$ENV://ALIYUN_ACCESS_ID"
		secretRef = "$ENV://ALIYUN_ACCESS_SECRET"
	)
	p := &Plugin{config: Config{AccessKeyID: idRef, AccessKeySecret: secretRef}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(t, name, map[string]string{
		idRef: "resolved-access-id",
	})
	defer closeAttempt()
	broker.fail[secretRef] = errors.New("broker failure contains secretRef")
	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p)
	if err == nil {
		t.Fatal("MaterializeScopedPluginSecrets() error = nil")
	}
	if strings.Contains(err.Error(), "secretRef") || strings.Contains(err.Error(), idRef) {
		t.Fatalf("materialization error leaked secret material: %v", err)
	}
	if p.config.AccessKeyID != idRef || p.config.AccessKeySecret != secretRef {
		t.Fatalf("config changed after failed materialization: %#v", p.config)
	}
	if err := p.useAliyunCredentials(func(string, string) error { return nil }); err == nil {
		t.Fatal("useAliyunCredentials() succeeded after atomic failure")
	}
}

func TestScopedSecretsFailureCanRetryWithoutRetainedState(t *testing.T) {
	const (
		idRef     = "$ENV://ALIYUN_ACCESS_ID"
		secretRef = "$ENV://ALIYUN_ACCESS_SECRET"
	)
	p := &Plugin{config: Config{AccessKeyID: idRef, AccessKeySecret: secretRef}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	secrets, scope, broker, closeAttempt := newScopedSecretHarness(t, name, map[string]string{
		idRef:     "resolved-access-id",
		secretRef: "resolved-access-secret",
	})
	defer closeAttempt()
	broker.fail[secretRef] = errors.New("temporary broker failure")
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err == nil {
		t.Fatal("first MaterializeScopedPluginSecrets() error = nil")
	}
	if p.config.AccessKeyID != idRef || p.config.AccessKeySecret != secretRef {
		t.Fatalf("config retained partial state after first failure: %#v", p.config)
	}
	broker.fail[secretRef] = nil
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("retry MaterializeScopedPluginSecrets() error = %v", err)
	}
	if got := len(broker.calls); got != 4 {
		t.Fatalf("broker calls = %d, want id,secret,id,secret", got)
	}
	for index, want := range []string{idRef, secretRef, idRef, secretRef} {
		if broker.calls[index].Raw != want {
			t.Fatalf("broker call %d raw = %q, want %q", index, broker.calls[index].Raw, want)
		}
	}
	idDescriptor, err := secret.NewDescriptor(
		capability.SecretPluginConfig, sha256.Sum256([]byte("resolved-access-id")),
	)
	if err != nil {
		t.Fatal(err)
	}
	secretDescriptor, err := secret.NewDescriptor(
		capability.SecretPluginConfig, sha256.Sum256([]byte("resolved-access-secret")),
	)
	if err != nil {
		t.Fatal(err)
	}
	if p.config.AccessKeyID != idDescriptor.String() ||
		p.config.AccessKeySecret != secretDescriptor.String() {
		t.Fatalf(
			"retry descriptors = (%q, %q), want resolved-value digests",
			p.config.AccessKeyID,
			p.config.AccessKeySecret,
		)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	if err := p.useAliyunCredentials(func(id, resolvedSecret string) error {
		if id != "resolved-access-id" || resolvedSecret != "resolved-access-secret" {
			t.Fatalf("retry credentials = %q/%q, want resolved values", id, resolvedSecret)
		}
		return nil
	}); err != nil {
		t.Fatalf("useAliyunCredentials() after retry error = %v", err)
	}
}

func TestPostInitDoesNotSelfMaterializeAliyunCredentials(t *testing.T) {
	const (
		idRef     = "$ENV://ALIYUN_ACCESS_ID"
		secretRef = "$ENV://ALIYUN_ACCESS_SECRET"
	)
	p := &Plugin{config: Config{
		Endpoint:        "https://moderation.example",
		RegionID:        "cn-shanghai",
		AccessKeyID:     idRef,
		AccessKeySecret: secretRef,
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	err := p.PostInit()
	if err == nil || err.Error() != errAliyunCredentialsUnavailable.Error() {
		t.Fatalf("PostInit() error = %v, want redacted credential-unavailable error", err)
	}
	if p.config.AccessKeyID != idRef || p.config.AccessKeySecret != secretRef {
		t.Fatalf("PostInit() changed raw config: %#v", p.config)
	}
}

func TestAliyunStopIsIdempotentAndConcurrentWithCredentialUse(t *testing.T) {
	p := &Plugin{config: Config{AccessKeyID: "id", AccessKeySecret: "secret"}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	secrets, scope, _, cleanup := newScopedSecretHarness(t, name, nil)
	defer cleanup()
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	useDone := make(chan error, 1)
	go func() {
		useDone <- p.useAliyunCredentials(func(string, string) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		t.Fatal("Stop() completed while credential callback still held")
	default:
	}
	close(release)
	if err := <-useDone; err != nil {
		t.Fatalf("credential use error = %v", err)
	}
	<-stopDone
	p.Stop()
	if err := p.useAliyunCredentials(func(string, string) error { return nil }); err == nil {
		t.Fatal("credential use succeeded after Stop()")
	}
}

type trackingResponseBody struct {
	*strings.Reader
	closed chan struct{}
	once   sync.Once
}

func (body *trackingResponseBody) Close() error {
	body.once.Do(func() { close(body.closed) })
	return nil
}

type blockingRoundTripper struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	body    *trackingResponseBody
	request *http.Request
}

func (transport *blockingRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.request = request
	transport.once.Do(func() { close(transport.entered) })
	<-transport.release
	transport.body = &trackingResponseBody{
		Reader: strings.NewReader(`{"Data":{"RiskLevel":"low"}}`),
		closed: make(chan struct{}),
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       transport.body,
	}, nil
}

func expectedAliyunForm(accessKeyID, accessKeySecret string) ([]byte, string) {
	params := map[string]string{
		"AccessKeyId":       accessKeyID,
		"Action":            "TextModerationPlus",
		"Format":            "JSON",
		"RegionId":          "cn-shanghai",
		"Service":           "llm_query_moderation",
		"ServiceParameters": `{"sessionId":"session","content":"hello"}`,
		"SignatureMethod":   "HMAC-SHA1",
		"SignatureNonce":    "nonce",
		"SignatureVersion":  "1.0",
		"Timestamp":         "1970-01-01T00:00:01Z",
		"Version":           "2022-03-02",
	}
	signature := aliyunSignature(params, accessKeySecret+"&")
	params["Signature"] = signature
	values := make(url.Values, len(params))
	for _, key := range sortedKeys(params) {
		values.Set(key, params[key])
	}
	return []byte(values.Encode()), signature
}

func retainedRequestMaterial(request *http.Request) []byte {
	if request == nil {
		return nil
	}
	var material []byte
	if request.URL != nil {
		material = append(material, request.URL.String()...)
	}
	for key, values := range request.Header {
		material = append(material, key...)
		for _, value := range values {
			material = append(material, value...)
		}
	}
	if request.Body != nil {
		body, _ := io.ReadAll(request.Body)
		material = append(material, body...)
	}
	if request.GetBody != nil {
		body, err := request.GetBody()
		if err == nil {
			defer func() { _ = body.Close() }()
			copyBody, _ := io.ReadAll(body)
			material = append(material, copyBody...)
		}
	}
	return material
}

func TestAliyunRequestStopBoundaryForScopedCredentials(t *testing.T) {
	const (
		accessKeyIDRef     = "$ENV://ALIYUN_ACCESS_KEY_ID"
		accessKeySecretRef = "$ENV://ALIYUN_ACCESS_KEY_SECRET"
		requestAccessKeyID = "resolved-access-id"
		requestSecret      = "resolved-access-secret"
	)
	p := &Plugin{config: Config{
		Endpoint:        "http://moderation.example",
		RegionID:        "cn-shanghai",
		AccessKeyID:     accessKeyIDRef,
		AccessKeySecret: accessKeySecretRef,
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	secrets, scope, _, closeAttempt := newScopedSecretHarness(t, name, map[string]string{
		accessKeyIDRef:     requestAccessKeyID,
		accessKeySecretRef: requestSecret,
	})
	defer closeAttempt()
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	p.now = func() time.Time { return time.Unix(1, 0) }
	p.nonce = func() string { return "nonce" }
	expectedForm, expectedSignature := expectedAliyunForm(
		requestAccessKeyID, requestSecret,
	)
	transport := &blockingRoundTripper{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	p.client = &http.Client{Transport: transport}
	requestDone := make(chan struct{})
	var statusCode int
	var responseBody []byte
	var requestErr error
	go func() {
		statusCode, responseBody, requestErr = p.sendModerationRequest(
			context.Background(), "session", "hello", "llm_query_moderation",
		)
		close(requestDone)
	}()
	<-transport.entered
	stopAttempted := make(chan struct{})
	p.stopStarted = func() { close(stopAttempted) }
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	<-stopAttempted
	if transport.request.GetBody != nil {
		t.Fatal("request retained GetBody after signed request construction")
	}
	select {
	case <-stopDone:
		t.Fatal("Stop() completed before request release")
	case <-time.After(50 * time.Millisecond):
	}
	close(transport.release)
	<-requestDone
	<-stopDone
	if requestErr != nil || statusCode != http.StatusOK ||
		string(responseBody) != `{"Data":{"RiskLevel":"low"}}` {
		t.Fatalf(
			"request result = (%d, %q, %v), want successful response",
			statusCode,
			responseBody,
			requestErr,
		)
	}
	select {
	case <-transport.body.closed:
	default:
		t.Fatal("response body was not closed before request completion")
	}
	if p.scopedCredentialsSet || p.scopedAccessKeyID.Digest() != [32]byte{} ||
		p.scopedAccessKeySecret.Digest() != [32]byte{} {
		t.Fatal("scoped credential values retained after Stop()")
	}
	retained := retainedRequestMaterial(transport.request)
	for _, forbidden := range [][]byte{
		[]byte(requestAccessKeyID), expectedForm, []byte(expectedSignature),
		[]byte(url.QueryEscape(expectedSignature)),
	} {
		if bytes.Contains(retained, forbidden) {
			t.Fatalf("retained request material contains credential/form bytes %q: %q", forbidden, retained)
		}
	}
	p.Stop()
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
