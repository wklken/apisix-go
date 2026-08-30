package tencent_cloud_cls

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
	"google.golang.org/protobuf/encoding/protowire"
)

type clsScopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

func TestResolveSourceIPCachesFirstResolvedAddressAndFailsClosed(t *testing.T) {
	lookups := 0
	p := &Plugin{lookupHostIP: func(host string) ([]net.IP, error) {
		lookups++
		if strings.TrimSpace(host) == "" {
			t.Fatal("lookup hostname is empty")
		}
		return []net.IP{net.ParseIP("192.0.2.10"), net.ParseIP("192.0.2.11")}, nil
	}}
	for range 2 {
		got, err := p.resolveSourceIP()
		if err != nil || got != "192.0.2.10" {
			t.Fatalf("resolveSourceIP() = %q, %v, want 192.0.2.10", got, err)
		}
	}
	if lookups != 1 {
		t.Fatalf("lookups = %d, want cached single lookup", lookups)
	}

	failed := &Plugin{lookupHostIP: func(string) ([]net.IP, error) {
		return nil, errors.New("dns unavailable")
	}}
	if _, err := failed.resolveSourceIP(); err == nil || !strings.Contains(err.Error(), "dns unavailable") {
		t.Fatalf("resolveSourceIP() error = %v, want DNS failure", err)
	}
}

type clsScopedSecretBroker struct {
	mu     sync.Mutex
	values map[string]string
	fail   map[string]error
	calls  []clsScopedSecretCall
}

func (broker *clsScopedSecretBroker) ResolveScoped(
	ctx context.Context, scope secret.Scope, raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, clsScopedSecretCall{Scope: scope, Raw: raw})
	if err := broker.fail[raw]; err != nil {
		return "", err
	}
	value, ok := broker.values[raw]
	if !ok {
		return "", errors.New("missing private CLS test value")
	}
	return value, nil
}

func (broker *clsScopedSecretBroker) scopedCalls() []clsScopedSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]clsScopedSecretCall(nil), broker.calls...)
}

func newCLSScopedSecretHarness(
	t *testing.T,
	revision uint64,
	resourceID string,
	config Config,
	values map[string]string,
	keyring ...string,
) (secret.GenerationSecrets, secret.Scope, *clsScopedSecretBroker, func()) {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: resourceID}
	document, err := json.Marshal(map[string]any{
		"id": resourceID, "plugins": map[string]any{name: config},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := generation.NewSnapshot(
		revision, []generation.Resource{{Key: key, Value: document}}, nil,
	)
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
			Key: key, Disposition: generation.DispositionPublished, Code: "cls-test",
		}},
	}
	publication := generation.PublicationSet{
		DesiredRevision: revision,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: candidate,
		},
	}
	catalog, err := capability.NewSecretDeclarationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	broker := &clsScopedSecretBroker{values: values, fail: make(map[string]error)}
	materialization, err := testutil.NewSecretMaterializerWithKeyring(broker, catalog, keyring).
		PrepareGeneration(context.Background(), publication)
	if err != nil {
		t.Fatal(err)
	}
	secrets := materialization.Secrets()
	scope := secret.Scope{
		Generation: revision, Domain: generation.DomainHTTP,
		Plugin: name, Resource: key, Source: capability.SecretPluginConfig,
	}
	return secrets, scope, broker, func() {
		if err := materialization.Close(context.Background()); err != nil {
			t.Errorf("close CLS scoped generation: %v", err)
		}
	}
}

func clsSecretDescriptor(plaintext string) string {
	digest := sha256.Sum256([]byte(plaintext))
	return fmt.Sprintf("plugin_config#sha256:%s", hex.EncodeToString(digest[:]))
}

func TestMaterializeScopedSecretsOwnsTencentCLSSecretKey(t *testing.T) {
	const raw = "$ENV://CLS_SECRET_KEY"
	const plaintext = "resolved-cls-secret-key"
	config := Config{
		CLSHost: "cls.example.com", CLSTopic: "topic-a",
		SecretID: "ordinary-secret-id", SecretKey: raw,
	}
	secrets, scope, broker, closeAttempt := newCLSScopedSecretHarness(
		t, 101, "route-cls", config, map[string]string{raw: plaintext},
	)
	defer closeAttempt()
	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	calls := broker.scopedCalls()
	if len(calls) != 1 || calls[0].Raw != raw || calls[0].Scope.Field != "secret_key" {
		t.Fatalf("scoped calls = %#v, want exact secret_key authority", calls)
	}
	if p.config.SecretID != config.SecretID {
		t.Fatalf("secret_id = %q, want ordinary value preserved", p.config.SecretID)
	}
	if p.config.SecretKey != clsSecretDescriptor(plaintext) {
		t.Fatalf("public secret_key = %q, want resolved descriptor", p.config.SecretKey)
	}
}

func TestMaterializeScopedSecretsSupportsCLSReferences(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "environment", raw: "$ENV://CLS_SECRET_KEY"},
		{name: "managed", raw: "$secret://vault/cls/key"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plaintext := "resolved-" + test.name
			config := Config{SecretID: "opaque-secret-id", SecretKey: test.raw}
			secrets, scope, broker, closeAttempt := newCLSScopedSecretHarness(
				t, uint64(110+index), "route-"+test.name, config,
				map[string]string{test.raw: plaintext},
			)
			defer closeAttempt()
			p := &Plugin{config: config}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}
			if err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, secrets, p,
			); err != nil {
				t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
			}
			calls := broker.scopedCalls()
			if len(calls) != 1 || calls[0].Raw != test.raw || calls[0].Scope.Field != "secret_key" {
				t.Fatalf("scoped calls = %#v, want only exact secret_key", calls)
			}
			if p.config.SecretID != config.SecretID || p.config.SecretKey != clsSecretDescriptor(plaintext) {
				t.Fatalf("public config = %#v, want ordinary ID and descriptor-only key", p.config)
			}
		})
	}
}

func TestMaterializeScopedSecretsRejectsSecretIDReferenceWithoutBrokerCall(t *testing.T) {
	const rawKey = "$ENV://CLS_SECRET_KEY"
	config := Config{SecretID: "$secret://must/not/resolve", SecretKey: rawKey}
	secrets, scope, broker, closeAttempt := newCLSScopedSecretHarness(
		t, 120, "route-secret-id", config, map[string]string{rawKey: "private-key"},
	)
	defer closeAttempt()
	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p)
	if err == nil || strings.Contains(err.Error(), config.SecretID) ||
		strings.Contains(err.Error(), rawKey) {
		t.Fatalf("materialization error = %v, want redacted fail-closed rejection", err)
	}
	if calls := broker.scopedCalls(); len(calls) != 0 {
		t.Fatalf("broker calls = %#v, want zero when secret_id resembles a reference", calls)
	}
	if p.config.SecretID != config.SecretID || p.config.SecretKey != config.SecretKey ||
		p.secretKeySet || p.secretsPrepared {
		t.Fatalf(
			"failed state = %#v set=%t prepared=%t, want atomic original state",
			p.config, p.secretKeySet, p.secretsPrepared,
		)
	}
}

func TestMaterializeScopedSecretsFailureIsAtomicAndRetryable(t *testing.T) {
	const raw = "$secret://vault/cls/key"
	const plaintext = "resolved-private-cls-key"
	config := Config{SecretID: "secret-id", SecretKey: raw}
	secrets, scope, broker, closeAttempt := newCLSScopedSecretHarness(
		t, 121, "route-retry", config, map[string]string{raw: plaintext},
	)
	defer closeAttempt()
	broker.fail[raw] = errors.New("resolver leaked " + raw + " " + plaintext)
	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p)
	if err == nil || strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), plaintext) {
		t.Fatalf("first materialization error = %v, want redacted error", err)
	}
	if p.config.SecretID != config.SecretID || p.config.SecretKey != config.SecretKey ||
		p.secretKeySet || p.secretsPrepared {
		t.Fatalf(
			"failed state = %#v set=%t prepared=%t, want no partial install",
			p.config, p.secretKeySet, p.secretsPrepared,
		)
	}
	broker.mu.Lock()
	delete(broker.fail, raw)
	broker.mu.Unlock()
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatalf("retry MaterializeScopedPluginSecrets() error = %v", err)
	}
	if p.config.SecretKey != clsSecretDescriptor(plaintext) || !p.secretKeySet || !p.secretsPrepared {
		t.Fatalf(
			"retry state = %#v set=%t prepared=%t, want atomic install",
			p.config, p.secretKeySet, p.secretsPrepared,
		)
	}
}

func TestMaterializeScopedSecretsSingleflight(t *testing.T) {
	const raw = "$ENV://CLS_SECRET_KEY"
	config := Config{SecretID: "secret-id", SecretKey: raw}
	secrets, scope, broker, closeAttempt := newCLSScopedSecretHarness(
		t, 122, "route-singleflight", config, map[string]string{raw: "resolved-key"},
	)
	defer closeAttempt()
	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			errs <- base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p)
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent materialization error = %v", err)
		}
	}
	if calls := broker.scopedCalls(); len(calls) != 1 {
		t.Fatalf("broker calls = %d, want one", len(calls))
	}
}

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()
	if len(cfg.LogFormat) == 0 {
		cfg.LogFormat = map[string]string{"request_id": "$request_id"}
	}

	p := &Plugin{config: cfg}
	p.SetDependencies(
		base.Dependencies{
			Tasks:          newLoggerTestTaskOwner(t),
			DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
		},
	)
	p.now = func() time.Time { return time.Unix(1710000000, 0) }
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.now = func() time.Time { return time.Unix(1710000000, 0) }
	secrets, scope, _, cleanup := newCLSScopedSecretHarness(
		t, 1, "test-route", cfg, map[string]string{cfg.SecretKey: cfg.SecretKey},
	)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	return p
}

func TestEffectiveLogFormatRouteWins(t *testing.T) {
	route := map[string]string{"route": "$request_id"}
	metadata := map[string]string{"metadata": "$route_id"}

	p := newRawTestPlugin(t, Config{
		CLSHost:   "cls.example.com",
		CLSTopic:  "topic-a",
		SecretID:  "id",
		SecretKey: "key",
		LogFormat: route,
	}, mustMetadataView(t, map[string]any{"log_format": metadata}))
	if len(p.LogFormat) != 1 || p.LogFormat["route"] != route["route"] {
		t.Fatalf("effective format = %#v, want route format over metadata %#v", p.LogFormat, metadata)
	}
	route["route"] = "mutated"
	if p.LogFormat["route"] == "mutated" {
		t.Fatal("effective route format was not cloned")
	}
}

func TestEffectiveLogFormatUsesMetadataFallback(t *testing.T) {
	metadata := map[string]string{"route": "$route_id"}

	p := newRawTestPlugin(t, Config{
		CLSHost:   "cls.example.com",
		CLSTopic:  "topic-a",
		SecretID:  "id",
		SecretKey: "key",
	}, mustMetadataView(t, map[string]any{"log_format": metadata}))
	if len(p.LogFormat) != 1 || p.LogFormat["route"] != metadata["route"] {
		t.Fatalf("effective format = %#v, want metadata format %#v", p.LogFormat, metadata)
	}
	metadata["route"] = "mutated"
	if p.LogFormat["route"] == "mutated" {
		t.Fatal("effective metadata format was not cloned")
	}
}

func TestEffectiveLogFormatRejectsEmptyBeforeSideEffects(t *testing.T) {
	p := &Plugin{config: Config{
		CLSHost:   "cls.example.com",
		CLSTopic:  "topic-a",
		SecretID:  "id",
		SecretKey: "key",
	}}
	p.SetDependencies(
		base.Dependencies{
			Tasks:          newLoggerTestTaskOwner(t),
			DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
		},
	)
	p.now = func() time.Time { return time.Unix(1710000000, 0) }
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	secrets, scope, _, cleanup := newCLSScopedSecretHarness(
		t, 1, "empty-log-format", p.config, map[string]string{p.config.SecretKey: p.config.SecretKey},
	)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	t.Cleanup(p.Stop)
	err := p.PostInit()
	if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "log_format") {
		t.Fatalf("PostInit() error = %v, want %s log_format rejection", err, name)
	}
	if p.client != nil || p.clientRelease != nil || p.BatchProcessor != nil {
		t.Fatalf(
			"PostInit() side effects = client=%v release=%v batch=%v, want none",
			p.client != nil,
			p.clientRelease != nil,
			p.BatchProcessor != nil,
		)
	}
}

func newRawTestPlugin(t *testing.T, cfg Config, metadata runtime.MetadataView) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{
		Tasks:          newLoggerTestTaskOwner(t),
		DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
		Metadata:       metadata,
	})
	p.now = func() time.Time { return time.Unix(1710000000, 0) }
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	secrets, scope, _, cleanup := newCLSScopedSecretHarness(
		t, 1, "raw-metadata", p.config, map[string]string{p.config.SecretKey: p.config.SecretKey},
	)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	return p
}

func mustMetadataView(t *testing.T, metadata map[string]any) runtime.MetadataView {
	t.Helper()
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

func TestPostInitAppliesCLSDefaults(t *testing.T) {
	p := newTestPlugin(t, Config{
		CLSHost:   "cls.example.com",
		CLSTopic:  "topic-a",
		SecretID:  "id",
		SecretKey: "key",
	})

	if p.config.Scheme != "https" {
		t.Fatalf("scheme = %q, want https", p.config.Scheme)
	}
	if !p.sslVerify() {
		t.Fatal("ssl_verify = false, want true by default")
	}
	if p.config.SampleRatio != 1 {
		t.Fatalf("sample_ratio = %v, want 1", p.config.SampleRatio)
	}
	if p.config.MaxReqBodyBytes != 524288 {
		t.Fatalf("max_req_body_bytes = %d, want 524288", p.config.MaxReqBodyBytes)
	}
	if p.config.MaxRespBodyBytes != 524288 {
		t.Fatalf("max_resp_body_bytes = %d, want 524288", p.config.MaxRespBodyBytes)
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
}

func TestRunLogPhasePreservesSampleRatio(t *testing.T) {
	p := &Plugin{config: Config{SampleRatio: 0.5}, sample: func() float64 { return 0.75 }}
	if err := p.RunLogPhase(base.LogSnapshot{}); err != nil {
		t.Fatalf("sampled-out RunLogPhase() error = %v", err)
	}
	p.sample = func() float64 { return 0.25 }
	if err := p.RunLogPhase(base.LogSnapshot{}); err == nil {
		t.Fatal("sampled-in RunLogPhase() error = nil, want unavailable processor")
	}
}

func TestPostInitResolvesRotatedEncryptedSecretKey(t *testing.T) {
	oldKey := "old-keyring-item"
	newKey := "qeddd145sfvddff3"
	p := &Plugin{config: Config{
		CLSHost:   "cls.example.com",
		CLSTopic:  "topic-a",
		SecretID:  "id",
		SecretKey: encryptTencentCLSTestValue(t, oldKey, "cls-secret"),
		LogFormat: map[string]string{"request_id": "$request_id"},
	}}
	p.SetDependencies(base.Dependencies{
		Tasks:          newLoggerTestTaskOwner(t),
		DataEncryption: testutil.DataEncryptionService(true, []string{newKey, oldKey}).Resolver(),
	})
	p.now = func() time.Time { return time.Unix(1710000000, 0) }
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	secrets, scope, _, cleanup := newCLSScopedSecretHarness(
		t, 1, "rotated-secret", p.config, map[string]string{p.config.SecretKey: "cls-secret"},
		newKey, oldKey,
	)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	if p.config.SecretKey != clsSecretDescriptor("cls-secret") {
		t.Fatalf("secret_key = %q, want resolved descriptor", p.config.SecretKey)
	}
}

func TestSendPostsCLSProtobufPayload(t *testing.T) {
	requests := make(chan *http.Request, 1)
	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		requests <- r
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		Scheme:    "http",
		CLSHost:   strings.TrimPrefix(server.URL, "http://"),
		CLSTopic:  "topic-a",
		SecretID:  "secret-id",
		SecretKey: "secret-key",
		GlobalTag: map[string]string{"env": "test"},
		Timeout:   1000,
	})

	p.Send(map[string]any{
		"route_id": "r1",
		"status":   200,
		"nested":   map[string]any{"ok": true},
	})

	req := waitRequest(t, requests)
	if req.Method != http.MethodPost {
		t.Fatalf("method = %s, want POST", req.Method)
	}
	if req.URL.Path != "/structuredlog" {
		t.Fatalf("path = %q, want /structuredlog", req.URL.Path)
	}
	if got := req.URL.Query().Get("topic_id"); got != "topic-a" {
		t.Fatalf("topic_id = %q, want topic-a", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/x-protobuf" {
		t.Fatalf("Content-Type = %q, want application/x-protobuf", got)
	}
	auth := req.Header.Get("Authorization")
	if !strings.Contains(auth, "q-sign-algorithm=sha1") || !strings.Contains(auth, "q-ak=secret-id") ||
		!strings.Contains(auth, "q-signature=") {
		t.Fatalf("Authorization = %q, want Tencent CLS signature", auth)
	}

	logs := decodeCLSBody(t, waitBody(t, bodies))
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}
	if logs[0]["route_id"] != "r1" {
		t.Fatalf("route_id = %q, want r1", logs[0]["route_id"])
	}
	if logs[0]["status"] != "200" {
		t.Fatalf("status = %q, want 200", logs[0]["status"])
	}
	if logs[0]["nested"] != `{"ok":true}` {
		t.Fatalf("nested = %q, want JSON object string", logs[0]["nested"])
	}
	if logs[0]["env"] != "test" {
		t.Fatalf("env = %q, want global tag", logs[0]["env"])
	}
}

func newScopedReadyCLSPlugin(
	t *testing.T,
	revision uint64,
	resourceID string,
	config Config,
	resolved string,
) *Plugin {
	t.Helper()
	if len(config.LogFormat) == 0 {
		config.LogFormat = map[string]string{"request_id": "$request_id"}
	}
	secrets, scope, _, closeAttempt := newCLSScopedSecretHarness(
		t, revision, resourceID, config, map[string]string{config.SecretKey: resolved},
	)
	t.Cleanup(closeAttempt)
	p := &Plugin{config: config}
	p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	p.now = func() time.Time { return time.Unix(1710000000, 0) }
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	return p
}

func TestCLSGenerationsShareOnlyNeutralClientAndKeepSignaturesIsolated(t *testing.T) {
	fixedNow := time.Unix(1710000000, 0)
	authorizations := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	p1 := newScopedReadyCLSPlugin(t, 130, "route-n", Config{
		Scheme: "http", CLSHost: host, CLSTopic: "topic-a", SecretID: "same-id",
		SecretKey: "$secret://cls/generation-n", Timeout: 1731,
	}, "generation-n-private-key")
	p2 := newScopedReadyCLSPlugin(t, 131, "route-n-plus-one", Config{
		Scheme: "http", CLSHost: host, CLSTopic: "topic-b", SecretID: "same-id",
		SecretKey: "$secret://cls/generation-n-plus-one", Timeout: 1731,
	}, "generation-n-plus-one-private-key")
	if p1.client != p2.client {
		t.Fatal("equal TLS/timeout generations did not share credential-neutral Resty client")
	}
	if _, err := p1.SendBatch(
		context.Background(), []map[string]any{{"generation": "n"}}, 1,
	); err != nil {
		t.Fatalf("generation N SendBatch() error = %v", err)
	}
	if _, err := p2.SendBatch(
		context.Background(), []map[string]any{{"generation": "n+1"}}, 1,
	); err != nil {
		t.Fatalf("generation N+1 SendBatch() error = %v", err)
	}
	authN := <-authorizations
	authNext := <-authorizations
	wantN := referenceCLSAuthorization("same-id", "generation-n-private-key", fixedNow)
	wantNext := referenceCLSAuthorization("same-id", "generation-n-plus-one-private-key", fixedNow)
	if authN != wantN || authNext != wantNext {
		t.Fatalf(
			"generation signatures = %q / %q, want independent references %q / %q",
			authN, authNext, wantN, wantNext,
		)
	}
	for label, mutant := range map[string]string{
		"raw reference N": referenceCLSAuthorization(
			"same-id", "$secret://cls/generation-n", fixedNow,
		),
		"descriptor N": referenceCLSAuthorization("same-id", p1.config.SecretKey, fixedNow),
		"raw reference N+1": referenceCLSAuthorization(
			"same-id", "$secret://cls/generation-n-plus-one", fixedNow,
		),
		"descriptor N+1": referenceCLSAuthorization("same-id", p2.config.SecretKey, fixedNow),
	} {
		if authN == mutant || authNext == mutant {
			t.Fatalf("generation Authorization matched %s mutant: %q", label, mutant)
		}
	}
	if strings.Contains(authN, "generation-n-private-key") ||
		strings.Contains(authNext, "generation-n-plus-one-private-key") {
		t.Fatal("raw secret key escaped into Authorization")
	}
}

func TestCLSSigningCallbackOwnsRequestResponseLifecycle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Authorization", "private-response-authorization")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("private-response-body"))
	}))
	t.Cleanup(server.Close)
	p := newScopedReadyCLSPlugin(t, 132, "route-callback-lifecycle", Config{
		Scheme: "http", CLSHost: strings.TrimPrefix(server.URL, "http://"), CLSTopic: "topic-a",
		SecretID: "secret-id", SecretKey: "$ENV://CLS_CALLBACK_KEY", Timeout: 1732,
	}, "callback-private-key")

	var retainedRequest *resty.Request
	var retainedResponse *resty.Response
	var retainedRawResponse *http.Response
	p.client.OnAfterResponse(func(_ *resty.Client, response *resty.Response) error {
		retainedRequest = response.Request
		retainedResponse = response
		retainedRawResponse = response.RawResponse
		return nil
	})
	callbackReturned := make(chan struct{})
	releaseCallback := make(chan struct{})
	p.testLifecycleHook = func(event string) {
		if event != lifecycleSigningCallbackReturned {
			return
		}
		close(callbackReturned)
		<-releaseCallback
	}
	sendDone := make(chan error, 1)
	go func() {
		_, err := p.SendBatch(
			context.Background(), []map[string]any{{"message": "private-body"}}, 1,
		)
		sendDone <- err
	}()
	select {
	case <-callbackReturned:
	case <-time.After(2 * time.Second):
		close(releaseCallback)
		t.Fatal("timed out waiting for signing callback return barrier")
	}

	problems := retainedCLSGraphProblems(retainedRequest, retainedResponse, retainedRawResponse)
	select {
	case err := <-sendDone:
		problems = append(problems, fmt.Sprintf("SendBatch returned before callback release: %v", err))
	default:
	}
	close(releaseCallback)
	var sendErr error
	select {
	case sendErr = <-sendDone:
	case <-time.After(2 * time.Second):
		t.Fatal("SendBatch did not return after signing callback release")
	}
	if len(problems) > 0 {
		t.Fatalf("request/response graph escaped signing callback: %s", strings.Join(problems, "; "))
	}
	if sendErr == nil || !strings.Contains(sendErr.Error(), "status code [502]") ||
		!strings.Contains(sendErr.Error(), "private-response-body") {
		t.Fatalf("SendBatch() error = %v, want callback-owned status/body result", sendErr)
	}
}

func TestCLSSendScrubsRetainedRequestAndResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Authorization", "private-response-authorization")
		_, _ = w.Write([]byte("private-response-body"))
	}))
	t.Cleanup(server.Close)
	p := newScopedReadyCLSPlugin(t, 132, "route-retained", Config{
		Scheme: "http", CLSHost: strings.TrimPrefix(server.URL, "http://"), CLSTopic: "topic-a",
		SecretID: "secret-id", SecretKey: "$ENV://CLS_RETAINED_KEY", Timeout: 1732,
	}, "retained-private-key")
	var retainedRequest *resty.Request
	var retainedResponse *resty.Response
	var retainedRawResponse *http.Response
	p.client.OnAfterResponse(func(_ *resty.Client, response *resty.Response) error {
		retainedRequest = response.Request
		retainedResponse = response
		retainedRawResponse = response.RawResponse
		return nil
	})
	if _, err := p.SendBatch(
		context.Background(), []map[string]any{{"message": "private-body"}}, 1,
	); err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}
	if retainedRequest == nil || retainedResponse == nil || retainedRawResponse == nil {
		t.Fatal("Resty hook did not retain request/response/raw response")
	}
	if retainedRequest.Header.Get("Authorization") != "" || retainedRequest.Body != nil {
		t.Fatalf("retained request still holds Authorization/body: %#v", retainedRequest)
	}
	if retainedRequest.RawRequest == nil ||
		retainedRequest.RawRequest.Header.Get("Authorization") != "" ||
		retainedRequest.RawRequest.Body != http.NoBody || retainedRequest.RawRequest.GetBody != nil {
		t.Fatalf("retained raw request was not scrubbed: %#v", retainedRequest.RawRequest)
	}
	if retainedResponse.Request != nil || retainedResponse.RawResponse != nil ||
		len(retainedResponse.Body()) != 0 {
		t.Fatalf("retained response still links private request/body: %#v", retainedResponse)
	}
	if got := retainedRawResponse.Header.Get("Authorization"); got != "" {
		t.Fatalf("retained raw response Authorization = %q", got)
	}
	if retainedRawResponse.Body != http.NoBody || retainedRawResponse.Request != nil {
		t.Fatalf("retained raw response still owns request/body: %#v", retainedRawResponse)
	}
}

func retainedCLSGraphProblems(
	request *resty.Request,
	response *resty.Response,
	rawResponse *http.Response,
) []string {
	problems := make([]string, 0, 8)
	if request == nil || response == nil || rawResponse == nil {
		return append(problems, "Resty hook did not retain the complete graph")
	}
	if request.Header.Get("Authorization") != "" {
		problems = append(problems, "request Authorization retained")
	}
	if request.Body != nil {
		problems = append(problems, "request body retained")
	}
	if request.RawRequest == nil {
		problems = append(problems, "raw request missing")
	} else {
		if request.RawRequest.Header.Get("Authorization") != "" {
			problems = append(problems, "raw request Authorization retained")
		}
		if request.RawRequest.Body != http.NoBody || request.RawRequest.GetBody != nil {
			problems = append(problems, "raw request body retained")
		}
	}
	if response.Request != nil || response.RawResponse != nil || len(response.Body()) != 0 {
		problems = append(problems, "Resty response graph retained")
	}
	if rawResponse.Header.Get("Authorization") != "" {
		problems = append(problems, "raw response Authorization retained")
	}
	if rawResponse.Body != http.NoBody || rawResponse.Request != nil {
		problems = append(problems, "raw response request/body retained")
	}
	return problems
}

func referenceCLSAuthorization(secretID, secretKey string, now time.Time) string {
	signTime := fmt.Sprintf("%d;%d", now.Unix(), now.Unix()+authExpireSeconds)
	httpRequestInfo := fmt.Sprintf("%s\n%s\n%s\n%s\n", "post", clsAPIPath, "", "")
	httpRequestDigest := sha1.Sum([]byte(httpRequestInfo))
	stringToSign := fmt.Sprintf(
		"%s\n%s\n%s\n", "sha1", signTime, hex.EncodeToString(httpRequestDigest[:]),
	)
	hmacHex := func(key, value []byte) string {
		mac := hmac.New(sha1.New, key)
		_, _ = mac.Write(value)
		return hex.EncodeToString(mac.Sum(nil))
	}
	signKey := hmacHex([]byte(secretKey), []byte(signTime))
	signature := hmacHex([]byte(signKey), []byte(stringToSign))
	return "q-sign-algorithm=sha1" +
		"&q-ak=" + secretID +
		"&q-sign-time=" + signTime +
		"&q-key-time=" + signTime +
		"&q-header-list=" +
		"&q-url-param-list=" +
		"&q-signature=" + signature
}

func TestCLSStopDrainsActiveSendAndPreventsResurrection(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	p := newScopedReadyCLSPlugin(t, 133, "route-stop", Config{
		Scheme: "http", CLSHost: strings.TrimPrefix(server.URL, "http://"), CLSTopic: "topic-a",
		SecretID: "secret-id", SecretKey: "$ENV://CLS_STOP_KEY", Timeout: 1733,
	}, "stop-private-key")
	processor := p.BatchProcessor
	sendDone := make(chan error, 1)
	go func() {
		_, err := p.SendBatch(context.Background(), []map[string]any{{"message": "blocked"}}, 1)
		sendDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for active CLS send")
	}
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	secondStopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(secondStopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not seal admission while active send remained blocked")
	}
	select {
	case <-secondStopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent Stop() did not observe sealed admission")
	}
	if p.client == nil || p.BatchProcessor == nil || !p.secretKeySet || !p.ready {
		t.Fatal("Stop() cleaned active CLS resources before delivery returned")
	}
	if !p.stopped.Load() {
		t.Fatal("Stop() did not seal the CLS log entrypoint")
	}
	close(release)
	if err := <-sendDone; err != nil {
		t.Fatalf("active SendBatch() error = %v", err)
	}
	if err := processor.Shutdown(context.Background()); err != nil {
		t.Fatalf("batch Shutdown() error = %v", err)
	}
	if p.client != nil || p.BatchProcessor != nil || p.secretKeySet || p.secretsPrepared || p.ready {
		t.Fatalf("retired state retained resources: client=%v batch=%v key=%t prepared=%t ready=%t",
			p.client != nil, p.BatchProcessor != nil, p.secretKeySet, p.secretsPrepared, p.ready)
	}
	before := len(p.FireChan)
	if err := p.RunLogPhase(base.LogSnapshot{}); !errors.Is(err, base.ErrLogQueueUnavailable) {
		t.Fatalf("post-Stop RunLogPhase() error = %v", err)
	}
	if len(p.FireChan) != before {
		t.Fatal("post-Stop log phase enqueued work")
	}
	if _, err := p.SendBatch(context.Background(), []map[string]any{{"late": true}}, 1); err == nil {
		t.Fatal("SendBatch() after Stop error = nil, want fail closed")
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() after Stop error = nil, want no resurrection")
	}
	p.Stop()
}

func TestCLSStopFlushesPendingBatchBeforeCleanup(t *testing.T) {
	received := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	p := newScopedReadyCLSPlugin(t, 134, "route-pending", Config{
		Scheme: "http", CLSHost: strings.TrimPrefix(server.URL, "http://"), CLSTopic: "topic-a",
		SecretID: "secret-id", SecretKey: "$secret://cls/pending", Timeout: 1734,
		BatchMaxSize: 100, BufferDuration: 60, InactiveTimeout: 60,
	}, "pending-private-key")
	p.BatchProcessor.Push(map[string]any{"message": "pending"})
	processor := p.BatchProcessor
	p.Stop()
	if err := processor.Shutdown(context.Background()); err != nil {
		t.Fatalf("batch Shutdown() error = %v", err)
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not flush pending CLS batch")
	}
	if p.client != nil || p.BatchProcessor != nil || p.secretKeySet || p.ready {
		t.Fatal("Stop() cleanup retained CLS resources after pending flush")
	}
}

func TestCLSStopBeforePostInitPreventsPublication(t *testing.T) {
	config := Config{
		CLSHost: "cls.example.com", CLSTopic: "topic-a", SecretID: "secret-id",
		SecretKey: "$ENV://CLS_PREPARED_KEY", LogFormat: map[string]string{"id": "$request_id"},
	}
	secrets, scope, _, closeAttempt := newCLSScopedSecretHarness(
		t, 135, "route-prepared", config,
		map[string]string{config.SecretKey: "prepared-private-key"},
	)
	defer closeAttempt()
	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	p.Stop()
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() after prepared Stop error = nil, want fail closed")
	}
	if p.client != nil || p.BatchProcessor != nil || p.secretKeySet || p.secretsPrepared || p.ready {
		t.Fatal("Stop-before-PostInit published or retained CLS state")
	}
}

func TestHandlerSendsFormattedRequestLog(t *testing.T) {
	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		Scheme:    "http",
		CLSHost:   strings.TrimPrefix(server.URL, "http://"),
		CLSTopic:  "topic-a",
		SecretID:  "secret-id",
		SecretKey: "secret-key",
		LogFormat: map[string]string{
			"method": "$request_method",
			"path":   "$request_uri",
			"plugin": "tencent-cloud-cls",
		},
		Timeout:      1000,
		BatchMaxSize: 1,
	})

	req := httptest.NewRequest(http.MethodPatch, "http://example.com/orders/1?debug=true", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rr.Code)
	}

	logs := decodeCLSBody(t, waitBody(t, bodies))
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}
	if logs[0]["method"] != http.MethodPatch {
		t.Fatalf("method = %q, want PATCH", logs[0]["method"])
	}
	if logs[0]["path"] != "/orders/1?debug=true" {
		t.Fatalf("path = %q, want request URI", logs[0]["path"])
	}
	if logs[0]["plugin"] != "tencent-cloud-cls" {
		t.Fatalf("plugin = %q, want tencent-cloud-cls", logs[0]["plugin"])
	}
}

func TestHandlerIncludesRequestAndResponseBody(t *testing.T) {
	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		Scheme:           "http",
		CLSHost:          strings.TrimPrefix(server.URL, "http://"),
		CLSTopic:         "topic-a",
		SecretID:         "secret-id",
		SecretKey:        "secret-key",
		IncludeReqBody:   true,
		IncludeRespBody:  true,
		MaxReqBodyBytes:  32,
		MaxRespBodyBytes: 32,
		Timeout:          1000,
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

	logs := decodeCLSBody(t, waitBody(t, bodies))
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}

	request := decodeJSONStringField(t, logs[0]["request"])
	if request["body"] != `{"order":1}` {
		t.Fatalf("request body = %#v, want original request body", request["body"])
	}

	response := decodeJSONStringField(t, logs[0]["response"])
	if response["body"] != `{"ok":true}` {
		t.Fatalf("response body = %#v, want upstream response body", response["body"])
	}
}

func TestHandlerIncludesBodiesWhenExpressionsMatch(t *testing.T) {
	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		Scheme:              "http",
		CLSHost:             strings.TrimPrefix(server.URL, "http://"),
		CLSTopic:            "topic-a",
		SecretID:            "secret-id",
		SecretKey:           "secret-key",
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: [][]any{{"status", "==", "201"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
		Timeout:             1000,
		BatchMaxSize:        1,
	})

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", bytes.NewBufferString(`{"order":2}`))
	req.Header.Set("X-Log-Body", "yes")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	})).ServeHTTP(rr, req)

	logs := decodeCLSBody(t, waitBody(t, bodies))
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}

	request := decodeJSONStringField(t, logs[0]["request"])
	if request["body"] != `{"order":2}` {
		t.Fatalf("request body = %#v, want captured request body", request["body"])
	}

	response := decodeJSONStringField(t, logs[0]["response"])
	if response["body"] != `{"created":true}` {
		t.Fatalf("response body = %#v, want captured response body", response["body"])
	}
}

func TestHandlerSkipsBodiesWhenExpressionsDoNotMatch(t *testing.T) {
	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		Scheme:              "http",
		CLSHost:             strings.TrimPrefix(server.URL, "http://"),
		CLSTopic:            "topic-a",
		SecretID:            "secret-id",
		SecretKey:           "secret-key",
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: [][]any{{"status", "==", "500"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
		Timeout:             1000,
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

	logs := decodeCLSBody(t, waitBody(t, bodies))
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}
	if _, ok := logs[0]["request"]; ok {
		t.Fatalf("request field = %q, want no request body", logs[0]["request"])
	}
	if _, ok := logs[0]["response"]; ok {
		t.Fatalf("response field = %q, want no response body", logs[0]["response"])
	}
}

func TestSchemaAcceptsOfficialBodyExpressionFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"cls_host":               "cls.example.com",
		"cls_topic":              "topic-a",
		"secret_id":              "secret-id",
		"secret_key":             "secret-key",
		"include_req_body_expr":  []any{[]any{"http_x_log_body", "==", "yes"}},
		"include_resp_body_expr": []any{[]any{"status", "==", "201"}},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official body expression fields: %v", err)
	}
}

func TestSchemaAcceptsBatchAndMaxPendingFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"cls_host":            "cls.example.com",
		"cls_topic":           "topic-a",
		"secret_id":           "secret-id",
		"secret_key":          "secret-key",
		"batch_max_size":      2,
		"max_retry_count":     1,
		"retry_delay":         1,
		"buffer_duration":     60,
		"inactive_timeout":    5,
		"max_pending_entries": 100,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected batch and max pending fields: %v", err)
	}
}

func TestMetadataSchemaAcceptsLogFormatAndPendingLimit(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Validate(map[string]any{
		"log_format":          map[string]any{"generation": "$route_id"},
		"max_pending_entries": 1,
	}, p.GetMetadataSchema()); err != nil {
		t.Fatalf("valid metadata rejected: %v", err)
	}
	for _, metadata := range []map[string]any{
		{"log_format": "$route_id"},
		{"log_format": map[string]any{"generation": 1}},
		{"max_pending_entries": 0},
	} {
		if err := util.Validate(metadata, p.GetMetadataSchema()); err == nil {
			t.Fatalf("invalid metadata accepted: %#v", metadata)
		}
	}
}

func TestPreparedGenerationsRetainMetadataFormat(t *testing.T) {
	config := Config{
		CLSHost:   "cls.example.com",
		CLSTopic:  "topic-a",
		SecretID:  "id",
		SecretKey: "key",
	}
	first := newRawTestPlugin(t, config, mustMetadataView(t, map[string]any{
		"log_format":          map[string]any{"generation": "n"},
		"max_pending_entries": 11,
	}))
	second := newRawTestPlugin(t, config, mustMetadataView(t, map[string]any{
		"log_format":          map[string]any{"generation": "n-plus-one"},
		"max_pending_entries": 12,
	}))

	if first.LogFormat["generation"] != "n" || first.config.MaxPendingEntries != 11 {
		t.Fatalf("generation N metadata = %#v/%d", first.LogFormat, first.config.MaxPendingEntries)
	}
	if second.LogFormat["generation"] != "n-plus-one" || second.config.MaxPendingEntries != 12 {
		t.Fatalf("generation N+1 metadata = %#v/%d", second.LogFormat, second.config.MaxPendingEntries)
	}
}

func TestMetadataDecodeFailsBeforeCLSClientAndProcessorAcquisition(t *testing.T) {
	p := &Plugin{config: Config{
		CLSHost:   "cls.example.com",
		CLSTopic:  "topic-a",
		SecretID:  "id",
		SecretKey: "key",
		LogFormat: map[string]string{"generation": "route"},
	}}
	p.SetDependencies(base.Dependencies{
		Tasks:          newLoggerTestTaskOwner(t),
		DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
		Metadata: mustMetadataView(t, map[string]any{
			"max_pending_entries": "invalid",
		}),
	})
	p.now = func() time.Time { return time.Unix(1710000000, 0) }
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	secrets, scope, _, cleanup := newCLSScopedSecretHarness(
		t, 1, "invalid-metadata", p.config, map[string]string{p.config.SecretKey: p.config.SecretKey},
	)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	err := p.PostInit()
	defer p.Stop()
	if err == nil {
		t.Fatal("PostInit() error = nil for invalid metadata")
	}
	if p.client != nil || p.clientRelease != nil || p.BatchProcessor != nil {
		t.Fatalf(
			"decode failure acquired resources: client=%v release=%v processor=%v",
			p.client,
			p.clientRelease != nil,
			p.BatchProcessor,
		)
	}
}

func TestHandlerBatchesCLSLogs(t *testing.T) {
	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		Scheme:       "http",
		CLSHost:      strings.TrimPrefix(server.URL, "http://"),
		CLSTopic:     "topic-a",
		SecretID:     "secret-id",
		SecretKey:    "secret-key",
		Timeout:      1000,
		BatchMaxSize: 2,
		LogFormat: map[string]string{
			"path": "$uri",
		},
	})

	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/first", nil))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example.com/second", nil))

	logs := decodeCLSBody(t, waitBody(t, bodies))
	if len(logs) != 2 {
		t.Fatalf("logs = %d, want 2", len(logs))
	}
	if logs[0]["path"] != "/first" || logs[1]["path"] != "/second" {
		t.Fatalf("paths = %q, %q; want /first, /second", logs[0]["path"], logs[1]["path"])
	}
}

func decodeJSONStringField(t *testing.T, value string) map[string]any {
	t.Helper()

	var out map[string]any
	if err := json.Unmarshal([]byte(value), &out); err != nil {
		t.Fatalf("unmarshal JSON string field %q: %v", value, err)
	}
	return out
}

func waitRequest(t *testing.T, requests <-chan *http.Request) *http.Request {
	t.Helper()

	select {
	case req := <-requests:
		return req
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for CLS request")
		return nil
	}
}

func waitBody(t *testing.T, bodies <-chan []byte) []byte {
	t.Helper()

	select {
	case body := <-bodies:
		return body
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for CLS body")
		return nil
	}
}

func decodeCLSBody(t *testing.T, body []byte) []map[string]string {
	t.Helper()

	var logs []map[string]string
	for len(body) > 0 {
		num, typ, n := protowire.ConsumeTag(body)
		if n < 0 {
			t.Fatalf("consume LogGroupList tag: %v", protowire.ParseError(n))
		}
		body = body[n:]
		if num != 1 || typ != protowire.BytesType {
			t.Fatalf("LogGroupList field = %d/%v, want 1/bytes", num, typ)
		}
		group, n := protowire.ConsumeBytes(body)
		if n < 0 {
			t.Fatalf("consume LogGroup bytes: %v", protowire.ParseError(n))
		}
		body = body[n:]
		logs = append(logs, decodeLogGroup(t, group)...)
	}
	return logs
}

func decodeLogGroup(t *testing.T, group []byte) []map[string]string {
	t.Helper()

	var logs []map[string]string
	for len(group) > 0 {
		num, typ, n := protowire.ConsumeTag(group)
		if n < 0 {
			t.Fatalf("consume LogGroup tag: %v", protowire.ParseError(n))
		}
		group = group[n:]
		if typ != protowire.BytesType {
			t.Fatalf("LogGroup field %d type = %v, want bytes", num, typ)
		}
		value, n := protowire.ConsumeBytes(group)
		if n < 0 {
			t.Fatalf("consume LogGroup value: %v", protowire.ParseError(n))
		}
		group = group[n:]
		if num == 1 {
			logs = append(logs, decodeLog(t, value))
		}
	}
	return logs
}

func decodeLog(t *testing.T, logBody []byte) map[string]string {
	t.Helper()

	out := map[string]string{}
	for len(logBody) > 0 {
		num, typ, n := protowire.ConsumeTag(logBody)
		if n < 0 {
			t.Fatalf("consume Log tag: %v", protowire.ParseError(n))
		}
		logBody = logBody[n:]
		switch num {
		case 1:
			if typ != protowire.VarintType {
				t.Fatalf("Log.time type = %v, want varint", typ)
			}
			_, n := protowire.ConsumeVarint(logBody)
			if n < 0 {
				t.Fatalf("consume Log.time: %v", protowire.ParseError(n))
			}
			logBody = logBody[n:]
		case 2:
			if typ != protowire.BytesType {
				t.Fatalf("Log.contents type = %v, want bytes", typ)
			}
			content, n := protowire.ConsumeBytes(logBody)
			if n < 0 {
				t.Fatalf("consume Log.contents: %v", protowire.ParseError(n))
			}
			logBody = logBody[n:]
			key, value := decodeContent(t, content)
			out[key] = value
		default:
			t.Fatalf("unexpected Log field %d", num)
		}
	}
	return out
}

func decodeContent(t *testing.T, content []byte) (string, string) {
	t.Helper()

	var key, value string
	for len(content) > 0 {
		num, typ, n := protowire.ConsumeTag(content)
		if n < 0 {
			t.Fatalf("consume Content tag: %v", protowire.ParseError(n))
		}
		content = content[n:]
		if typ != protowire.BytesType {
			t.Fatalf("Content field %d type = %v, want bytes", num, typ)
		}
		raw, n := protowire.ConsumeBytes(content)
		if n < 0 {
			t.Fatalf("consume Content value: %v", protowire.ParseError(n))
		}
		content = content[n:]
		switch num {
		case 1:
			key = string(raw)
		case 2:
			value = string(raw)
		default:
			t.Fatalf("unexpected Content field %d", num)
		}
	}
	return key, value
}

func encryptTencentCLSTestValue(t *testing.T, key string, value string) string {
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

func waitCLSEntry(t *testing.T, entries <-chan logger.Entry, substring string) logger.Entry {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case entry := <-entries:
			if strings.Contains(entry.Message, substring) {
				return entry
			}
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Fatalf("timed out waiting for cls diagnostic containing %q", substring)
	return logger.Entry{}
}

func TestBuildBatchPayloadReportsTruncatedFieldCount(t *testing.T) {
	entries := make(chan logger.Entry, 2)
	stop := logger.ReplaceObserver(t.Name(), func(entry logger.Entry) {
		entries <- entry
	})
	t.Cleanup(stop)

	p := &Plugin{}
	p.applyDefaults()
	p.sourceIP = "192.0.2.10"

	big := strings.Repeat("v", maxSingleValueSize+10)
	payload, err := p.buildBatchPayload([]map[string]any{{"big": big}})
	if err != nil {
		t.Fatalf("buildBatchPayload() error = %v", err)
	}
	if len(payload) == 0 {
		t.Fatal("buildBatchPayload() = nil, want a payload despite truncation")
	}

	entry := waitCLSEntry(t, entries, "truncated")
	if !strings.Contains(entry.Message, "1") {
		t.Fatalf("truncation diagnostic = %q, want the truncated field count", entry.Message)
	}
}

func TestBuildBatchPayloadReportsOverLimitEntryDrops(t *testing.T) {
	entries := make(chan logger.Entry, 2)
	stop := logger.ReplaceObserver(t.Name(), func(entry logger.Entry) {
		entries <- entry
	})
	t.Cleanup(stop)

	p := &Plugin{}
	p.applyDefaults()
	p.sourceIP = "192.0.2.10"

	// Six 1MB values in one entry exceed the 5MB group limit and must be
	// reported as a dropped entry rather than sent.
	huge := map[string]any{}
	for i := range 6 {
		huge["f"+string(rune('a'+i))] = strings.Repeat("v", maxSingleValueSize)
	}
	payload, err := p.buildBatchPayload([]map[string]any{huge})
	if err != nil {
		t.Fatalf("buildBatchPayload() error = %v", err)
	}
	if len(payload) != 0 {
		t.Fatalf("buildBatchPayload() = %d bytes, want empty payload for an over-limit entry", len(payload))
	}

	entry := waitCLSEntry(t, entries, "dropped")
	if !strings.Contains(entry.Message, "1") {
		t.Fatalf("drop diagnostic = %q, want the dropped entry count", entry.Message)
	}
}

func TestBuildBatchPayloadReportsDroppedBatchRemainder(t *testing.T) {
	entries := make(chan logger.Entry, 2)
	stop := logger.ReplaceObserver(t.Name(), func(entry logger.Entry) {
		entries <- entry
	})
	t.Cleanup(stop)

	p := &Plugin{}
	p.applyDefaults()
	p.sourceIP = "192.0.2.10"

	// Six 1MB entries exceed the 5MB group limit; the last two are dropped
	// and the remaining batch is still sent.
	big := strings.Repeat("v", maxSingleValueSize)
	logs := make([]map[string]any, 0, 6)
	for range 6 {
		logs = append(logs, map[string]any{"v": big})
	}
	payload, err := p.buildBatchPayload(logs)
	if err != nil {
		t.Fatalf("buildBatchPayload() error = %v", err)
	}
	if len(payload) == 0 {
		t.Fatal("buildBatchPayload() = nil, want the accepted entries' payload")
	}

	entry := waitCLSEntry(t, entries, "dropped")
	if !strings.Contains(entry.Message, "2") {
		t.Fatalf("drop diagnostic = %q, want the dropped remainder count", entry.Message)
	}
}

func TestAuthorizationSignTimeUsesSingleTimestamp(t *testing.T) {
	p := &Plugin{config: Config{SecretID: "secret-id", SecretKey: "secret-key"}}
	calls := 0
	p.now = func() time.Time {
		calls++
		if calls > 1 {
			return time.Unix(1710000001, 0)
		}
		return time.Unix(1710000000, 0)
	}

	auth := authorization(&p.config, p.now())

	start, end, ok := signTimeWindow(auth)
	if !ok {
		t.Fatalf("authorization = %q, want q-sign-time=start;end", auth)
	}
	if end-start != authExpireSeconds {
		t.Fatalf("sign time window = %d seconds, want exactly %d", end-start, authExpireSeconds)
	}
	if calls != 1 {
		t.Fatalf("now() called %d times, want exactly once per signature", calls)
	}
}

func signTimeWindow(auth string) (int64, int64, bool) {
	for part := range strings.SplitSeq(auth, "&") {
		value, ok := strings.CutPrefix(part, "q-sign-time=")
		if !ok {
			continue
		}
		start, end, ok := strings.Cut(value, ";")
		if !ok {
			return 0, 0, false
		}
		startTime, err := strconv.ParseInt(start, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		endTime, err := strconv.ParseInt(end, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		return startTime, endTime, true
	}
	return 0, 0, false
}
