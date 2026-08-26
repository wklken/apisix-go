package sls_logger

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
)

type slsScopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type slsScopedSecretBroker struct {
	mu     sync.Mutex
	values map[string]string
	fail   map[string]error
	calls  []slsScopedSecretCall
}

func (*slsScopedSecretBroker) AuthorizeCandidate(
	context.Context, secret.AttemptID, generation.ApplyTicket, generation.PublicationSet,
) error {
	return nil
}

func (*slsScopedSecretBroker) AuthorizeRecovery(
	context.Context, secret.AttemptID, generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return errors.New("recovery is not used by this SLS fixture")
}

func (broker *slsScopedSecretBroker) ResolveScoped(
	ctx context.Context, scope secret.Scope, raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, slsScopedSecretCall{Scope: scope, Raw: raw})
	if err := broker.fail[raw]; err != nil {
		return "", err
	}
	value, ok := broker.values[raw]
	if !ok {
		return "", errors.New("missing private SLS secret test value")
	}
	return value, nil
}

func (*slsScopedSecretBroker) RevokeAttempt(context.Context, secret.AttemptID) error {
	return nil
}

func (broker *slsScopedSecretBroker) callsSnapshot() []slsScopedSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]slsScopedSecretCall(nil), broker.calls...)
}

func newSLSScopedSecretHarness(
	t *testing.T,
	revision uint64,
	resourceID string,
	config Config,
	values map[string]string,
) (secret.GenerationCapability, secret.Scope, *slsScopedSecretBroker, func()) {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: resourceID}
	document, err := json.Marshal(map[string]any{
		"id": resourceID, "plugins": map[string]any{name: config},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{
		Key: key, Value: document,
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
			Key: key, Disposition: generation.DispositionPublished, Code: "sls-test",
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
	broker := &slsScopedSecretBroker{values: values, fail: make(map[string]error)}
	registration, err := secret.NewScopedMaterializer(broker, catalog).RegisterCandidate(
		context.Background(), ticket, publication,
	)
	if err != nil {
		t.Fatal(err)
	}
	capabilityValue, err := secret.NewGenerationCapability(registration, revision)
	if err != nil {
		_ = registration.Close(context.Background())
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
			t.Errorf("close SLS scoped attempt: %v", err)
		}
	}
}

func slsSecretDescriptor(plaintext string) string {
	digest := sha256.Sum256([]byte(plaintext))
	return fmt.Sprintf("plugin_config#sha256:%s", hex.EncodeToString(digest[:]))
}

func TestMaterializeScopedSecretsValidatesAndDropsSLSSecret(t *testing.T) {
	ciphertext := encryptSLSLoggerTestValue(t, "qeddd145sfvddff3", "cipher-private")
	for index, tt := range []struct {
		name     string
		raw      string
		resolved string
	}{
		{name: "ciphertext", raw: ciphertext, resolved: "cipher-private"},
		{name: "environment", raw: "$ENV://SLS_ACCESS_KEY_SECRET", resolved: "env-private"},
		{name: "managed", raw: "$secret://vault/sls/access-key-secret", resolved: "managed-private"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			config := Config{
				Host: "127.0.0.1", Port: 10009, Project: "project-a", Logstore: "store-a",
				AccessKeyID: "id", AccessKeySecret: tt.raw,
			}
			capabilityValue, scope, broker, closeAttempt := newSLSScopedSecretHarness(
				t, uint64(index+1), "sls-"+tt.name, config,
				map[string]string{tt.raw: tt.resolved},
			)
			defer closeAttempt()
			p := &Plugin{config: config}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}
			if err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			); err != nil {
				t.Fatal(err)
			}
			calls := broker.callsSnapshot()
			wantScope := scope
			wantScope.Field = "access_key_secret"
			if len(calls) != 1 || calls[0].Scope != wantScope || calls[0].Raw != tt.raw {
				t.Fatalf("secret calls = %#v, want scope %#v raw %q", calls, wantScope, tt.raw)
			}
			if p.config.AccessKeySecret != slsSecretDescriptor(tt.resolved) {
				t.Fatalf("public access_key_secret = %q, want resolved descriptor", p.config.AccessKeySecret)
			}
			if pluginStructContainsString(reflect.ValueOf(p).Elem(), tt.raw) ||
				pluginStructContainsString(reflect.ValueOf(p).Elem(), tt.resolved) {
				t.Fatal("plugin retained raw or resolved access_key_secret after validation")
			}
			if p.BatchProcessor != nil {
				t.Fatal("materialization caused batch side effects")
			}
			if p.TaskOwner() == nil {
				p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
			}
			if err := p.PostInit(); err != nil {
				t.Fatalf("PostInit() without resolver error = %v", err)
			}
			processor := p.BatchProcessor
			conn := &captureWriteConn{}
			p.dialTLS = func(*net.Dialer, string, string, *tls.Config) (net.Conn, error) {
				return conn, nil
			}
			if _, err := p.SendBatch(
				context.Background(), []map[string]any{{"path": "/orders"}}, 1,
			); err != nil {
				t.Fatalf("SendBatch() error = %v", err)
			}
			message := conn.message()
			if strings.Contains(message, tt.raw) || strings.Contains(message, tt.resolved) {
				t.Fatalf("delivery retained private access key material: %q", message)
			}
			if calls := broker.callsSnapshot(); len(calls) != 1 {
				t.Fatalf("PostInit/delivery resolver calls = %#v, want admission-only call", calls)
			}
			p.Stop()
			if err := processor.Shutdown(context.Background()); err != nil {
				t.Fatalf("batch Shutdown() error = %v", err)
			}
			if p.BatchProcessor != nil || p.secretsPrepared || p.ready {
				t.Fatal("Stop retained SLS generation lifecycle state")
			}
		})
	}

	const failedRaw = "$secret://vault/sls/failure-private"
	config := Config{AccessKeySecret: failedRaw}
	capabilityValue, scope, broker, closeAttempt := newSLSScopedSecretHarness(
		t, 10, "sls-retry", config, map[string]string{failedRaw: "recovered-private"},
	)
	defer closeAttempt()
	broker.fail[failedRaw] = errors.New("resolver leaked " + failedRaw)
	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
	if !errors.Is(err, secret.ErrCredentialUnavailable) || strings.Contains(err.Error(), failedRaw) {
		t.Fatalf("first materialization error = %v, want redacted unavailable", err)
	}
	if p.config.AccessKeySecret != failedRaw || p.BatchProcessor != nil {
		t.Fatal("failed materialization installed partial state")
	}
	broker.mu.Lock()
	delete(broker.fail, failedRaw)
	broker.mu.Unlock()
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("same-instance retry error = %v", err)
	}
	if p.config.AccessKeySecret != slsSecretDescriptor("recovered-private") {
		t.Fatalf("retry descriptor = %q, want resolved digest", p.config.AccessKeySecret)
	}
}

func TestSLSScopedMaterializationIsSingleFlightAndRejectsBlankResolvedSecret(t *testing.T) {
	const raw = "$ENV://SLS_SINGLEFLIGHT_ACCESS_KEY_SECRET"
	config := Config{AccessKeySecret: raw}
	capabilityValue, scope, broker, closeAttempt := newSLSScopedSecretHarness(
		t, 20, "sls-singleflight", config, map[string]string{raw: " \t\n "},
	)
	defer closeAttempt()
	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
	if !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("blank materialization error = %v, want unavailable", err)
	}
	if p.config.AccessKeySecret != raw || p.secretsPrepared {
		t.Fatal("blank materialization installed partial state")
	}
	broker.mu.Lock()
	broker.values[raw] = "singleflight-private"
	broker.calls = nil
	broker.mu.Unlock()

	start := make(chan struct{})
	errs := make(chan error, 16)
	var group sync.WaitGroup
	for range 16 {
		group.Go(func() {
			<-start
			errs <- base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			)
		})
	}
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls := broker.callsSnapshot(); len(calls) != 1 {
		t.Fatalf("singleflight resolver calls = %#v, want one", calls)
	}
	if p.config.AccessKeySecret != slsSecretDescriptor("singleflight-private") {
		t.Fatalf("singleflight descriptor = %q", p.config.AccessKeySecret)
	}
}

func pluginStructContainsString(value reflect.Value, target string) bool {
	switch value.Kind() {
	case reflect.String:
		return value.String() == target
	case reflect.Struct:
		for field := range value.Fields() {
			if pluginStructContainsString(value.FieldByIndex(field.Index), target) {
				return true
			}
		}
	case reflect.Array:
		for index := range value.Len() {
			if pluginStructContainsString(value.Index(index), target) {
				return true
			}
		}
	}
	return false
}

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()
	return newTestPluginWithMetadata(t, cfg, nil)
}

func newTestPluginWithMetadata(t *testing.T, cfg Config, metadata map[string]any) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{
		Tasks:          newLoggerTestTaskOwner(t),
		DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
		Metadata:       mustMetadataView(t, metadata),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeSecrets(); err != nil {
		t.Fatalf("MaterializeSecrets() error = %v", err)
	}
	if p.TaskOwner() == nil {
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	return p
}

func mustMetadataView(t *testing.T, metadata map[string]any) runtime.MetadataView {
	t.Helper()
	if len(metadata) == 0 {
		return runtime.MetadataView{}
	}
	document, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	view, err := runtime.NewMetadataView(map[string][]byte{name: document})
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	return view
}

func TestPostInitRejectsMissingDataEncryptionResolver(t *testing.T) {
	p := &Plugin{}
	if err := p.MaterializeSecrets(); err == nil || err.Error() != "data-encryption resolver is required" {
		t.Fatalf("MaterializeSecrets() error = %v, want missing resolver error", err)
	}
}

func TestDefaultAccessLogFieldsRedactSensitiveHeaders(t *testing.T) {
	requestHeaders := http.Header{
		"Authorization": {"Bearer request-secret"},
		"Cookie":        {"session=request-secret"},
		"X-Trace-ID":    {"trace-a"},
	}
	responseHeaders := http.Header{
		"Set-Cookie": {"session=response-secret"},
		"X-Upstream": {"orders"},
	}

	snapshot := base.LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{
			Method: http.MethodGet,
			URI:    "/orders",
			Host:   "gateway.example",
			Header: requestHeaders,
		},
		Response: apisixlog.ResponseLogSnapshot{Header: responseHeaders},
		Outcome:  apisixctx.ResponseOutcome{Status: http.StatusOK},
	}
	assertSLSHeadersSanitized(t, slsSnapshotDefaultFields(snapshot))

	request := httptest.NewRequest(http.MethodGet, "http://gateway.example/orders", nil)
	request.Header = requestHeaders.Clone()
	assertSLSHeadersSanitized(t, defaultAccessLogFields(request, http.StatusOK, responseHeaders))
}

func assertSLSHeadersSanitized(t *testing.T, fields map[string]any) {
	t.Helper()
	request := fields["request"].(map[string]any)
	requestHeaders := request["headers"].(map[string]any)
	if _, ok := requestHeaders["authorization"]; ok {
		t.Fatalf("request headers contain authorization: %#v", requestHeaders)
	}
	if _, ok := requestHeaders["cookie"]; ok {
		t.Fatalf("request headers contain cookie: %#v", requestHeaders)
	}
	if got := requestHeaders["x-trace-id"]; got != "trace-a" {
		t.Fatalf("request x-trace-id = %#v, want trace-a", got)
	}

	response := fields["response"].(map[string]any)
	responseHeaders := response["headers"].(map[string]any)
	if _, ok := responseHeaders["set-cookie"]; ok {
		t.Fatalf("response headers contain set-cookie: %#v", responseHeaders)
	}
	if got := responseHeaders["x-upstream"]; got != "orders" {
		t.Fatalf("response x-upstream = %#v, want orders", got)
	}
}

func TestPostInitSetsSLSDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		Host:            "127.0.0.1",
		Port:            10009,
		Project:         "project-a",
		Logstore:        "store-a",
		AccessKeyID:     "id",
		AccessKeySecret: "secret",
	})

	if p.config.Timeout != 5000 {
		t.Fatalf("timeout = %d, want 5000", p.config.Timeout)
	}
	if p.addr != "127.0.0.1:10009" {
		t.Fatalf("addr = %q, want 127.0.0.1:10009", p.addr)
	}
	if p.config.BatchMaxSize != 1000 {
		t.Fatalf("batch_max_size = %d, want 1000", p.config.BatchMaxSize)
	}
	if p.config.RetryDelay != 1 {
		t.Fatalf("retry_delay = %d, want 1", p.config.RetryDelay)
	}
	if p.config.BufferDuration != 60 {
		t.Fatalf("buffer_duration = %d, want 60", p.config.BufferDuration)
	}
	if p.config.InactiveTimeout != 5 {
		t.Fatalf("inactive_timeout = %d, want 5", p.config.InactiveTimeout)
	}
	if p.config.SSLVerify == nil || !*p.config.SSLVerify {
		t.Fatalf("ssl_verify = %v, want secure default true", p.config.SSLVerify)
	}
}

func TestPostInitRejectsInvalidEncryptedAccessKeySecret(t *testing.T) {
	p := &Plugin{config: Config{
		Host:            "127.0.0.1",
		Port:            10009,
		Project:         "project-a",
		Logstore:        "store-a",
		AccessKeyID:     "id",
		AccessKeySecret: "not-a-ciphertext",
	}}
	p.SetDependencies(base.Dependencies{
		Tasks:          newLoggerTestTaskOwner(t),
		DataEncryption: testutil.DataEncryptionService(true, []string{"qeddd145sfvddff3"}).Resolver(),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeSecrets(); err == nil {
		t.Fatal("MaterializeSecrets() error = nil, want strict encrypted access_key_secret rejection")
	}
}

func TestMaterializeSecretsValidatesRotatedEncryptedAccessKeySecret(t *testing.T) {
	oldKey := "old-keyring-item"
	newKey := "qeddd145sfvddff3"
	p := &Plugin{config: Config{
		Host:            "127.0.0.1",
		Port:            10009,
		Project:         "project-a",
		Logstore:        "store-a",
		AccessKeyID:     "id",
		AccessKeySecret: encryptSLSLoggerTestValue(t, oldKey, "sls-secret"),
	}}
	p.SetDependencies(base.Dependencies{
		Tasks:          newLoggerTestTaskOwner(t),
		DataEncryption: testutil.DataEncryptionService(true, []string{newKey, oldKey}).Resolver(),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeSecrets(); err != nil {
		t.Fatalf("MaterializeSecrets() error = %v", err)
	}
	if p.TaskOwner() == nil {
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	if p.config.AccessKeySecret != slsSecretDescriptor("sls-secret") {
		t.Fatalf("access_key_secret = %q, want resolved descriptor", p.config.AccessKeySecret)
	}
}

func TestSendMessageReturnsWithinWriteDeadline(t *testing.T) {
	conn := &blockingWriteConn{}
	p := newTestPlugin(t, Config{
		Host:            "127.0.0.1",
		Port:            10009,
		Project:         "project-a",
		Logstore:        "store-a",
		AccessKeyID:     "id",
		AccessKeySecret: "secret",
		Timeout:         100,
	})
	p.dialTLS = func(*net.Dialer, string, string, *tls.Config) (net.Conn, error) {
		return conn, nil
	}

	start := time.Now()
	err := p.sendMessage(context.Background(), "blocked SLS message")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("sendMessage() error = nil, want write deadline error")
	}
	if elapsed >= 200*time.Millisecond {
		t.Fatalf("sendMessage() took %s, want return within twice the configured 100ms timeout", elapsed)
	}
}

func TestBatchProcessorDefaultsMaxPendingEntries(t *testing.T) {
	p := newTestPlugin(t, Config{
		Host:            "127.0.0.1",
		Port:            10009,
		Project:         "project-a",
		Logstore:        "store-a",
		AccessKeyID:     "id",
		AccessKeySecret: "secret",
		BatchMaxSize:    50000,
		BufferDuration:  3600,
		InactiveTimeout: 3600,
	})

	dropped := 0
	for range 10002 {
		if !p.BatchProcessor.Push(map[string]any{"message": "line"}) {
			dropped++
		}
	}
	if dropped != 2 {
		t.Fatalf("dropped = %d, want exactly 2 beyond the default 10000 pending cap", dropped)
	}
}

func TestStopDrainsPendingSLSBatchAndPreventsResurrection(t *testing.T) {
	p := newTestPlugin(t, Config{
		Host: "127.0.0.1", Port: 10009, Project: "project-a", Logstore: "store-a",
		AccessKeyID: "id", AccessKeySecret: "secret", Timeout: 1000,
		BatchMaxSize: 10, BufferDuration: 3600, InactiveTimeout: 3600,
	})
	conn := &captureWriteConn{}
	p.dialTLS = func(*net.Dialer, string, string, *tls.Config) (net.Conn, error) {
		return conn, nil
	}
	if err := p.RunLogPhase(base.LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{Method: http.MethodGet, URI: "/pending"},
		Outcome: apisixctx.ResponseOutcome{Status: http.StatusOK},
	}); err != nil {
		t.Fatalf("RunLogPhase() error = %v", err)
	}

	processor := p.BatchProcessor
	p.Stop()
	if err := processor.Shutdown(context.Background()); err != nil {
		t.Fatalf("batch Shutdown() error = %v", err)
	}
	if message := conn.message(); !strings.Contains(message, `"uri":"/pending"`) {
		t.Fatalf("Stop did not drain pending batch: %q", message)
	}
	if p.BatchProcessor != nil || p.ready || p.secretsPrepared {
		t.Fatal("Stop retained SLS lifecycle state")
	}
	beforeFire := len(p.FireChan)
	if err := p.RunLogPhase(base.LogSnapshot{}); !errors.Is(err, base.ErrLogQueueUnavailable) {
		t.Fatalf("post-Stop RunLogPhase() error = %v", err)
	}
	if _, err := p.SendBatch(
		context.Background(), []map[string]any{{"post_stop": true}}, 1,
	); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("post-Stop SendBatch() error = %v", err)
	}
	p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(
		httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/stopped", nil),
	)
	if got := len(p.FireChan); got != beforeFire {
		t.Fatalf("post-Stop Handler FireChan length = %d, want unchanged %d", got, beforeFire)
	}
	if err := p.MaterializeSecrets(); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("post-Stop MaterializeSecrets() error = %v", err)
	}
	if p.TaskOwner() == nil {
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	}
	if err := p.PostInit(); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("post-Stop PostInit() error = %v", err)
	}
	p.Stop()
}

func TestStopSealsBeforeActiveSLSSendCompletes(t *testing.T) {
	p := newTestPlugin(t, Config{
		Host: "127.0.0.1", Port: 10009, Project: "project-a", Logstore: "store-a",
		AccessKeyID: "id", AccessKeySecret: "secret", Timeout: 5000, BatchMaxSize: 1,
	})
	conn := newGatedWriteConn()
	p.dialTLS = func(*net.Dialer, string, string, *tls.Config) (net.Conn, error) {
		return conn, nil
	}
	sendDone := make(chan error, 1)
	go func() {
		_, err := p.SendBatch(context.Background(), []map[string]any{{"active": true}}, 1)
		sendDone <- err
	}()
	select {
	case <-conn.started:
	case <-time.After(time.Second):
		t.Fatal("active send did not reach socket write")
	}
	processor := p.BatchProcessor
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("Stop waited for active SLS send instead of sealing scheduler admission")
	}
	close(conn.release)
	if err := <-sendDone; err != nil {
		t.Fatalf("active SendBatch() error = %v", err)
	}
	if err := processor.Shutdown(context.Background()); err != nil {
		t.Fatalf("batch Shutdown() error = %v", err)
	}
	if p.BatchProcessor != nil || p.ready || p.secretsPrepared {
		t.Fatal("Stop retained state after active send")
	}
}

func TestBuildMessageUsesRFC5424Shape(t *testing.T) {
	p := newTestPlugin(t, Config{
		Host:            "127.0.0.1",
		Port:            10009,
		Project:         "project-a",
		Logstore:        "store-a",
		AccessKeyID:     "id",
		AccessKeySecret: "secret",
	})

	message := p.buildMessage(map[string]any{
		"path":   "/orders",
		"status": 201,
	})

	if !strings.HasPrefix(message, "<46>1 ") {
		t.Fatalf("message = %q, want RFC5424 SYSLOG/INFO prefix <46>1", message)
	}
	wantStructured := `[logservice project="project-a" logstore="store-a" access-key-id="id"]`
	if !strings.Contains(message, wantStructured) {
		t.Fatalf("message = %q, want SLS structured data %s", message, wantStructured)
	}
	if strings.Contains(message, "secret") {
		t.Fatalf("message = %q, want access key secret omitted", message)
	}
	if !strings.Contains(message, `"path":"/orders"`) {
		t.Fatalf("message = %q, want JSON log entry", message)
	}
	if !strings.HasSuffix(message, "\n") {
		t.Fatalf("message = %q, want newline suffix", message)
	}
}

func TestSendMessageTLSVerifyDefaultsOn(t *testing.T) {
	addr, received := startTLSServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split tls addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:            host,
		Port:            mustAtoi(t, port),
		Project:         "project-a",
		Logstore:        "store-a",
		AccessKeyID:     "id",
		AccessKeySecret: "secret",
		Timeout:         1000,
	})

	p.Send(map[string]any{"path": "/orders"})

	select {
	case message := <-received:
		t.Fatalf("default TLS verification accepted self-signed certificate: %q", message)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestSendMessageAllowsExplicitTLSVerifyOff(t *testing.T) {
	addr, received := startTLSServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split tls addr: %v", err)
	}

	verify := false
	p := newTestPlugin(t, Config{
		Host:            host,
		Port:            mustAtoi(t, port),
		Project:         "project-a",
		Logstore:        "store-a",
		AccessKeyID:     "id",
		AccessKeySecret: "secret",
		SSLVerify:       &verify,
		Timeout:         1000,
	})

	p.Send(map[string]any{"path": "/orders"})

	select {
	case message := <-received:
		if !strings.Contains(message, `"path":"/orders"`) {
			t.Fatalf("message = %q, want JSON log payload", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TLS log message with explicit ssl_verify=false")
	}
}

func TestSendWritesTLSMessage(t *testing.T) {
	addr, received := startTLSServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split tls addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:            host,
		Port:            mustAtoi(t, port),
		Project:         "project-a",
		Logstore:        "store-a",
		AccessKeyID:     "id",
		AccessKeySecret: "secret",
		SSLVerify:       tlsVerifyOff(),
		Timeout:         1000,
	})

	p.Send(map[string]any{"path": "/orders"})

	select {
	case message := <-received:
		if !strings.Contains(message, `[logservice project="project-a" logstore="store-a"`) {
			t.Fatalf("message = %q, want SLS structured data", message)
		}
		if !strings.Contains(message, `"path":"/orders"`) {
			t.Fatalf("message = %q, want JSON log payload", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TLS log message")
	}
}

func TestHandlerBatchesSLSMessages(t *testing.T) {
	addr, received := startTLSServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split tls addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:            host,
		Port:            mustAtoi(t, port),
		Project:         "project-a",
		Logstore:        "store-a",
		AccessKeyID:     "id",
		AccessKeySecret: "secret",
		SSLVerify:       tlsVerifyOff(),
		Timeout:         1000,
		BatchMaxSize:    2,
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/first", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/second", nil))

	select {
	case message := <-received:
		if got := strings.Count(message, "<46>1 "); got != 2 {
			t.Fatalf("message = %q, want two RFC5424 messages", message)
		}
		if got := strings.Count(message, "\n"); got != 2 {
			t.Fatalf("message = %q, want two newline-terminated messages", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for batched TLS log messages")
	}
}

func TestHandlerIncludesRequestAndResponseBody(t *testing.T) {
	addr, received := startTLSServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split tls addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:             host,
		Port:             mustAtoi(t, port),
		Project:          "project-a",
		Logstore:         "store-a",
		AccessKeyID:      "id",
		AccessKeySecret:  "secret",
		SSLVerify:        tlsVerifyOff(),
		Timeout:          1000,
		IncludeReqBody:   true,
		IncludeRespBody:  true,
		MaxReqBodyBytes:  32,
		MaxRespBodyBytes: 32,
		BatchMaxSize:     1,
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", bytes.NewBufferString(`{"order":1}`))
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream request body: %v", err)
		}
		if string(body) != `{"order":1}` {
			t.Fatalf("upstream body = %q, want original request body", body)
		}

		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("response status = %d, want %d", rr.Code, http.StatusCreated)
	}
	if body := rr.Body.String(); body != `{"ok":true}` {
		t.Fatalf("response body = %q, want upstream response body", body)
	}

	select {
	case message := <-received:
		payload := extractJSONPayload(t, message)
		request, ok := payload["request"].(map[string]any)
		if !ok {
			t.Fatalf("payload request = %#v, want object", payload["request"])
		}
		if request["body"] != `{"order":1}` {
			t.Fatalf("payload request body = %#v, want original request body", request["body"])
		}

		response, ok := payload["response"].(map[string]any)
		if !ok {
			t.Fatalf("payload response = %#v, want object", payload["response"])
		}
		if response["body"] != `{"ok":true}` {
			t.Fatalf("payload response body = %#v, want upstream response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TLS log message")
	}
}

func TestHandlerDefaultAccessLogIncludesRequestResponseAndRouteID(t *testing.T) {
	addr, received := startTLSServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split tls addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:            host,
		Port:            mustAtoi(t, port),
		Project:         "project-a",
		Logstore:        "store-a",
		AccessKeyID:     "id",
		AccessKeySecret: "secret",
		SSLVerify:       tlsVerifyOff(),
		Timeout:         1000,
		BatchMaxSize:    1,
	})
	p.RouteID = "route-a"

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders?region=west", nil)
	req.Header.Set("X-Request-ID", "request-a")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream", "orders")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})).ServeHTTP(rr, req)

	select {
	case message := <-received:
		payload := extractJSONPayload(t, message)
		if payload["route_id"] != "route-a" {
			t.Fatalf("payload route_id = %#v, want route-a", payload["route_id"])
		}
		request, ok := payload["request"].(map[string]any)
		if !ok {
			t.Fatalf("payload request = %#v, want object", payload["request"])
		}
		if request["method"] != http.MethodGet || request["uri"] != "/orders?region=west" {
			t.Fatalf("payload request = %#v, want GET /orders?region=west", request)
		}
		response, ok := payload["response"].(map[string]any)
		if !ok {
			t.Fatalf("payload response = %#v, want object", payload["response"])
		}
		if response["status"] != float64(http.StatusCreated) {
			t.Fatalf("payload response status = %#v, want %d", response["status"], http.StatusCreated)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TLS log message")
	}
}

func TestHandlerIncludesBodiesWhenExpressionsMatch(t *testing.T) {
	addr, received := startTLSServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split tls addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:                host,
		Port:                mustAtoi(t, port),
		Project:             "project-a",
		Logstore:            "store-a",
		AccessKeyID:         "id",
		AccessKeySecret:     "secret",
		SSLVerify:           tlsVerifyOff(),
		Timeout:             1000,
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: [][]any{{"status", "==", "201"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
		BatchMaxSize:        1,
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", bytes.NewBufferString(`{"order":2}`))
	req.Header.Set("X-Log-Body", "yes")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	})).ServeHTTP(rr, req)

	select {
	case message := <-received:
		payload := extractJSONPayload(t, message)
		request, ok := payload["request"].(map[string]any)
		if !ok {
			t.Fatalf("payload request = %#v, want object", payload["request"])
		}
		if request["body"] != `{"order":2}` {
			t.Fatalf("payload request body = %#v, want captured request body", request["body"])
		}

		response, ok := payload["response"].(map[string]any)
		if !ok {
			t.Fatalf("payload response = %#v, want object", payload["response"])
		}
		if response["body"] != `{"created":true}` {
			t.Fatalf("payload response body = %#v, want captured response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TLS log message")
	}
}

func TestHandlerSkipsBodiesWhenExpressionsDoNotMatch(t *testing.T) {
	addr, received := startTLSServer(t)
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split tls addr: %v", err)
	}

	p := newTestPlugin(t, Config{
		Host:                host,
		Port:                mustAtoi(t, port),
		Project:             "project-a",
		Logstore:            "store-a",
		AccessKeyID:         "id",
		AccessKeySecret:     "secret",
		SSLVerify:           tlsVerifyOff(),
		Timeout:             1000,
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: [][]any{{"status", "==", "500"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
		BatchMaxSize:        1,
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", bytes.NewBufferString(`{"order":3}`))
	req.Header.Set("X-Log-Body", "no")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream request body: %v", err)
		}
		if string(body) != `{"order":3}` {
			t.Fatalf("upstream body = %q, want original request body", body)
		}

		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":false}`))
	})).ServeHTTP(rr, req)

	select {
	case message := <-received:
		payload := extractJSONPayload(t, message)
		request, ok := payload["request"].(map[string]any)
		if !ok {
			t.Fatalf("payload request = %#v, want default request object", payload["request"])
		}
		if _, ok := request["body"]; ok {
			t.Fatalf("payload request body = %#v, want absent", request["body"])
		}
		response, ok := payload["response"].(map[string]any)
		if !ok {
			t.Fatalf("payload response = %#v, want default response object", payload["response"])
		}
		if _, ok := response["body"]; ok {
			t.Fatalf("payload response body = %#v, want absent", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TLS log message")
	}
}

func TestSchemaAcceptsOfficialBodyExpressionFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"host":                   "127.0.0.1",
		"port":                   10009,
		"project":                "project-a",
		"logstore":               "store-a",
		"access_key_id":          "id",
		"access_key_secret":      "secret",
		"include_req_body_expr":  []any{[]any{"http_x_log_body", "==", "yes"}},
		"include_resp_body_expr": []any{[]any{"status", "==", "201"}},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official body expression fields: %v", err)
	}
}

func TestSchemaAcceptsBatchFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"host":              "127.0.0.1",
		"port":              10009,
		"project":           "project-a",
		"logstore":          "store-a",
		"access_key_id":     "id",
		"access_key_secret": "secret",
		"batch_max_size":    2,
		"max_retry_count":   1,
		"retry_delay":       1,
		"buffer_duration":   60,
		"inactive_timeout":  5,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected batch fields: %v", err)
	}
}

func TestMetadataSchemaRejectsNonObjectLogFormat(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if p.GetMetadataSchema() == "" {
		t.Fatal("metadata schema is empty")
	}

	config := map[string]any{"log_format": "bad plugin metadata"}
	if err := util.Validate(config, p.GetMetadataSchema()); err == nil {
		t.Fatal("metadata schema accepted a non-object log_format")
	}
}

func TestPreparedGenerationsRetainMetadataFormat(t *testing.T) {
	config := Config{
		Host:            "127.0.0.1",
		Port:            10009,
		Project:         "project-a",
		Logstore:        "store-a",
		AccessKeyID:     "id",
		AccessKeySecret: "secret",
	}
	first := newTestPluginWithMetadata(t, config, map[string]any{
		"log_format": map[string]any{"generation": "n"},
	})
	second := newTestPluginWithMetadata(t, config, map[string]any{
		"log_format": map[string]any{"generation": "n-plus-one"},
	})
	routeConfig := config
	routeConfig.LogFormat = map[string]string{"generation": "route"}
	route := newTestPluginWithMetadata(t, routeConfig, map[string]any{
		"log_format": map[string]any{"generation": "metadata"},
	})

	if first.LogFormat["generation"] != "n" {
		t.Fatalf("generation N format = %#v", first.LogFormat)
	}
	if second.LogFormat["generation"] != "n-plus-one" {
		t.Fatalf("generation N+1 format = %#v", second.LogFormat)
	}
	if route.LogFormat["generation"] != "route" {
		t.Fatalf("route format = %#v, want route precedence", route.LogFormat)
	}
}

func TestMetadataDecodeFailsBeforeSLSProcessorAcquisition(t *testing.T) {
	const pattern = "^/sls-metadata-decode-failure-3d8f$"
	expressions := [][]any{{"$uri", "~", pattern}}
	p := &Plugin{config: Config{
		Host:               "127.0.0.1",
		Port:               10009,
		Project:            "project-a",
		Logstore:           "store-a",
		AccessKeyID:        "id",
		AccessKeySecret:    "secret",
		IncludeReqBodyExpr: expressions,
	}}
	p.SetDependencies(base.Dependencies{
		Tasks:          newLoggerTestTaskOwner(t),
		DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
		Metadata: mustMetadataView(t, map[string]any{
			"log_format": map[string]any{"generation": 1},
		}),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeSecrets(); err != nil {
		t.Fatalf("MaterializeSecrets() error = %v", err)
	}
	if p.TaskOwner() == nil {
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	}
	err := p.PostInit()
	defer p.Stop()
	if err == nil {
		t.Fatal("PostInit() error = nil for invalid metadata")
	}
	if p.BatchProcessor != nil {
		t.Fatalf("decode failure acquired processor: %v", p.BatchProcessor)
	}
	request := httptest.NewRequest(http.MethodGet, "/sls-metadata-decode-failure-3d8f", nil)
	if base.ExprMatched(request, expressions, 0) {
		t.Fatal("metadata decode failure retained a prepared expression regexp")
	}
}

func encryptSLSLoggerTestValue(t *testing.T, key string, value string) string {
	t.Helper()
	padding := aes.BlockSize - len(value)%aes.BlockSize
	padded := append([]byte(value), make([]byte, padding)...)
	for i := len(padded) - padding; i < len(padded); i++ {
		padded[i] = byte(padding)
	}
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		t.Fatalf("NewCipher() error = %v", err)
	}
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, []byte(key)).CryptBlocks(ciphertext, padded)
	return base64.StdEncoding.EncodeToString(ciphertext)
}

func extractJSONPayload(t *testing.T, message string) map[string]any {
	t.Helper()

	start := strings.Index(message, "{")
	end := strings.LastIndex(message, "}")
	if start == -1 || end == -1 || end < start {
		t.Fatalf("message = %q, want JSON payload", message)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(message[start:end+1]), &payload); err != nil {
		t.Fatalf("unmarshal SLS payload: %v", err)
	}
	return payload
}

func startTLSServer(t *testing.T) (string, <-chan string) {
	t.Helper()

	cert := testCertificate(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("listen tls: %v", err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
	})

	received := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if err == nil {
			received <- string(buf[:n])
		}
	}()

	return ln.Addr().String(), received
}

func testCertificate(t *testing.T) tls.Certificate {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "localhost",
		},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(time.Hour),
		KeyUsage:  x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("load key pair: %v", err)
	}
	return cert
}

func tlsVerifyOff() *bool {
	verify := false
	return &verify
}

func mustAtoi(t *testing.T, value string) int {
	t.Helper()

	var n int
	for _, r := range value {
		if r < '0' || r > '9' {
			t.Fatalf("invalid integer %q", value)
		}
		n = n*10 + int(r-'0')
	}
	return n
}

type blockingWriteConn struct {
	mu       sync.Mutex
	deadline time.Time
	closed   bool
}

type captureWriteConn struct {
	mu      sync.Mutex
	written []byte
	closed  bool
}

func (c *captureWriteConn) Write(payload []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.written = append(c.written, payload...)
	return len(payload), nil
}

func (c *captureWriteConn) message() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.written)
}

func (*captureWriteConn) Read([]byte) (int, error) { return 0, io.EOF }

func (c *captureWriteConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (*captureWriteConn) LocalAddr() net.Addr              { return slsTestAddr("local") }
func (*captureWriteConn) RemoteAddr() net.Addr             { return slsTestAddr("remote") }
func (*captureWriteConn) SetDeadline(time.Time) error      { return nil }
func (*captureWriteConn) SetReadDeadline(time.Time) error  { return nil }
func (*captureWriteConn) SetWriteDeadline(time.Time) error { return nil }

type gatedWriteConn struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedWriteConn() *gatedWriteConn {
	return &gatedWriteConn{started: make(chan struct{}), release: make(chan struct{})}
}

func (c *gatedWriteConn) Write(payload []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	<-c.release
	return len(payload), nil
}

func (*gatedWriteConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (*gatedWriteConn) Close() error                     { return nil }
func (*gatedWriteConn) LocalAddr() net.Addr              { return slsTestAddr("local") }
func (*gatedWriteConn) RemoteAddr() net.Addr             { return slsTestAddr("remote") }
func (*gatedWriteConn) SetDeadline(time.Time) error      { return nil }
func (*gatedWriteConn) SetReadDeadline(time.Time) error  { return nil }
func (*gatedWriteConn) SetWriteDeadline(time.Time) error { return nil }

func (c *blockingWriteConn) Write([]byte) (int, error) {
	c.mu.Lock()
	deadline := c.deadline
	c.mu.Unlock()
	if deadline.IsZero() {
		time.Sleep(time.Hour)
		return 0, os.ErrDeadlineExceeded
	}
	time.Sleep(time.Until(deadline))
	return 0, os.ErrDeadlineExceeded
}

func (c *blockingWriteConn) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (c *blockingWriteConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *blockingWriteConn) LocalAddr() net.Addr {
	return slsTestAddr("local")
}

func (c *blockingWriteConn) RemoteAddr() net.Addr {
	return slsTestAddr("remote")
}

func (c *blockingWriteConn) SetDeadline(time.Time) error {
	return nil
}

func (c *blockingWriteConn) SetReadDeadline(time.Time) error {
	return nil
}

func (c *blockingWriteConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = deadline
	return nil
}

type slsTestAddr string

func (a slsTestAddr) Network() string {
	return "test"
}

func (a slsTestAddr) String() string {
	return string(a)
}
