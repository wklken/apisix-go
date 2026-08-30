package rocketmq_logger

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	brotli "github.com/andybalholm/brotli"
	rocketmq "github.com/apache/rocketmq-client-go/v2"
	"github.com/apache/rocketmq-client-go/v2/primitive"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	apisixruntime "github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
)

type rocketMQScopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

func TestSchemaMatchesAPISIX317Matrix(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	tests := []struct {
		name   string
		config map[string]any
		valid  bool
	}{
		{
			name: "sanity",
			config: map[string]any{
				"nameserver_list": []any{"127.0.0.1:3"},
				"topic":           "test",
				"key":             "key1",
			},
			valid: true,
		},
		{
			name: "TLS enabled",
			config: map[string]any{
				"nameserver_list": []any{"127.0.0.1:3"},
				"topic":           "test",
				"key":             "key1",
				"use_tls":         true,
			},
			valid: true,
		},
		{
			name: "TLS verification is not an APISIX option",
			config: map[string]any{
				"nameserver_list": []any{"127.0.0.1:3"},
				"topic":           "test",
				"use_tls":         true,
				"tls_verify":      true,
			},
		},
		{
			name:   "missing nameserver list",
			config: map[string]any{"topic": "test", "key": "key1"},
		},
		{
			name: "string timeout",
			config: map[string]any{
				"nameserver_list": []any{"127.0.0.1:3000"},
				"timeout":         "10",
				"topic":           "test",
				"key":             "key1",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := util.Validate(test.config, p.GetSchema())
			if test.valid && err != nil {
				t.Fatalf("valid APISIX 3.17 configuration rejected: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("invalid APISIX 3.17 configuration accepted")
			}
		})
	}
}

func TestAPISIX317WarnsWhenRocketMQTLSIsDisabled(t *testing.T) {
	var warnings []string
	stop := logger.ReplaceObserver("rocketmq-logger-security-warning", func(entry logger.Entry) {
		if entry.Level == "WARN" && strings.Contains(entry.Message, "use_tls disabled in rocketmq-logger") {
			warnings = append(warnings, entry.Message)
		}
	})
	defer stop()

	newTestPlugin(t, Config{
		NameServerList: []string{"127.0.0.1:9876"},
		Topic:          "apisix-logs",
	}, &captureSender{})

	want := "Keeping use_tls disabled in rocketmq-logger configuration is a security risk"
	if len(warnings) != 1 || warnings[0] != want {
		t.Fatalf("warnings = %#v, want [%q]", warnings, want)
	}
}

func TestAPISIX317DoesNotWarnWhenRocketMQTLSIsEnabled(t *testing.T) {
	warnings := 0
	stop := logger.ReplaceObserver("rocketmq-logger-tls-warning", func(entry logger.Entry) {
		if entry.Level == "WARN" && strings.Contains(entry.Message, "use_tls disabled in rocketmq-logger") {
			warnings++
		}
	})
	defer stop()

	newTestPlugin(t, Config{
		NameServerList: []string{"127.0.0.1:9876"},
		Topic:          "apisix-logs",
		UseTLS:         true,
	}, &captureSender{})

	if warnings != 0 {
		t.Fatalf("TLS-enabled warning count = %d, want zero", warnings)
	}
}

type rocketMQScopedSecretBroker struct {
	mu     sync.Mutex
	values map[string]string
	fail   map[string]error
	calls  []rocketMQScopedSecretCall
}

func (*rocketMQScopedSecretBroker) AuthorizeCandidate(
	context.Context,
	secret.AttemptID,
	generation.ApplyTicket,
	generation.PublicationSet,
) error {
	return nil
}

func (broker *rocketMQScopedSecretBroker) ResolveScoped(
	ctx context.Context,
	scope secret.Scope,
	raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, rocketMQScopedSecretCall{Scope: scope, Raw: raw})
	if err := broker.fail[raw]; err != nil {
		return "", err
	}
	value, ok := broker.values[raw]
	if !ok {
		return "", errors.New("missing private RocketMQ test secret")
	}
	return value, nil
}

func (*rocketMQScopedSecretBroker) RevokeAttempt(context.Context, secret.AttemptID) error {
	return nil
}

func (broker *rocketMQScopedSecretBroker) setFailure(raw string, err error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if err == nil {
		delete(broker.fail, raw)
		return
	}
	broker.fail[raw] = err
}

func (broker *rocketMQScopedSecretBroker) callsSnapshot() []rocketMQScopedSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]rocketMQScopedSecretCall(nil), broker.calls...)
}

func newRocketMQScopedSecretHarness(
	t *testing.T,
	revision uint64,
	resourceID string,
	rawConfig map[string]any,
	values map[string]string,
	keyring ...string,
) (secret.GenerationCapability, secret.Scope, *rocketMQScopedSecretBroker) {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: resourceID}
	document, err := json.Marshal(map[string]any{
		"id": resourceID, "plugins": map[string]any{name: rawConfig},
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
			Key: key, Disposition: generation.DispositionPublished, Code: "rocketmq-scoped-test",
		}},
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: revision, DesiredDigest: snapshot.Digest(),
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
	broker := &rocketMQScopedSecretBroker{
		values: values,
		fail:   make(map[string]error),
	}
	registration, err := testutil.NewSecretMaterializerWithKeyring(broker, catalog, keyring).RegisterCandidate(
		context.Background(), ticket, publication,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Errorf("close RocketMQ scoped attempt: %v", err)
		}
	})
	capabilityValue, err := secret.NewGenerationCapability(registration, revision)
	if err != nil {
		t.Fatal(err)
	}
	scope := secret.Scope{
		Generation: revision, Attempt: registration.AttemptID(), Domain: generation.DomainHTTP,
		Plugin: name, Resource: key, Source: capability.SecretPluginConfig,
	}
	return capabilityValue, scope, broker
}

func rocketMQRawConfig(secretKey string, useTLS bool) map[string]any {
	return map[string]any{
		"nameserver_list": []any{"127.0.0.1:9876"},
		"topic":           "apisix-logs",
		"access_key":      "rocketmq-access",
		"secret_key":      secretKey,
		"use_tls":         useTLS,
	}
}

func newRawRocketMQPlugin(t *testing.T, rawConfig map[string]any) *Plugin {
	t.Helper()
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := util.Parse(rawConfig, p.Config()); err != nil {
		t.Fatal(err)
	}
	return p
}

func rocketMQSecretDescriptor(plaintext string) string {
	digest := sha256.Sum256([]byte(plaintext))
	return fmt.Sprintf("plugin_config#sha256:%s", hex.EncodeToString(digest[:]))
}

func TestMaterializeScopedSecretsOwnsRocketMQSecretKey(t *testing.T) {
	contextual := encryptRocketMQLoggerTestValue(t, "0123456789abcdef", "contextual-private")
	tests := []struct {
		name     string
		raw      string
		resolved string
	}{
		{name: "contextual ciphertext", raw: contextual, resolved: "contextual-private"},
		{name: "environment", raw: "$ENV://ROCKETMQ_SECRET_KEY", resolved: "environment-private"},
		{name: "managed", raw: "$secret://vault/rocketmq/secret-key", resolved: "managed-private"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rawConfig := rocketMQRawConfig(test.raw, false)
			capabilityValue, scope, broker := newRocketMQScopedSecretHarness(
				t, uint64(110+index), "rocketmq-raw", rawConfig,
				map[string]string{test.raw: test.resolved},
				"0123456789abcdef",
			)
			p := newRawRocketMQPlugin(t, rawConfig)
			if err := base.MaterializeScopedPluginSecrets(
				context.Background(), scope, capabilityValue, p,
			); err != nil {
				t.Fatal(err)
			}
			calls := broker.callsSnapshot()
			wantScope := scope
			wantScope.Field = "secret_key"
			isReference := strings.HasPrefix(test.raw, "$secret://") ||
				strings.HasPrefix(strings.ToUpper(test.raw), "$ENV://")
			if !isReference && len(calls) != 0 {
				t.Fatalf("resolver calls = %#v, want none for ciphertext", calls)
			}
			if isReference && (len(calls) != 1 || calls[0].Scope != wantScope || calls[0].Raw != test.raw) {
				t.Fatalf("resolver calls = %#v, want exact secret_key scope %#v", calls, wantScope)
			}
			if p.config.SecretKey != rocketMQSecretDescriptor(test.resolved) ||
				p.config.SecretKey == test.raw || p.config.SecretKey == test.resolved {
				t.Fatalf("public secret_key = %q, want resolved descriptor", p.config.SecretKey)
			}
			if p.sender != nil || p.BatchProcessor != nil {
				t.Fatal("materialization created sender or processor before PostInit")
			}
			p.Stop()
		})
	}

	const retryRaw = "$secret://vault/rocketmq/retry"
	retryConfig := rocketMQRawConfig(retryRaw, false)
	retryCapability, retryScope, retryBroker := newRocketMQScopedSecretHarness(
		t, 120, "rocketmq-retry", retryConfig,
		map[string]string{retryRaw: "retry-private"},
	)
	retryBroker.setFailure(retryRaw, errors.New("resolver leaked "+retryRaw+" retry-private"))
	retry := newRawRocketMQPlugin(t, retryConfig)
	err := base.MaterializeScopedPluginSecrets(
		context.Background(), retryScope, retryCapability, retry,
	)
	if !errors.Is(err, secret.ErrCredentialUnavailable) ||
		strings.Contains(fmt.Sprint(err), retryRaw) || strings.Contains(fmt.Sprint(err), "retry-private") {
		t.Fatalf("failed materialization error = %v, want redacted unavailable", err)
	}
	if retry.config.SecretKey != retryRaw || retry.sender != nil || retry.BatchProcessor != nil {
		t.Fatal("failed materialization installed public or runtime state")
	}
	retryBroker.setFailure(retryRaw, nil)
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), retryScope, retryCapability, retry,
	); err != nil {
		t.Fatalf("same-instance retry error = %v", err)
	}
	if retry.config.SecretKey != rocketMQSecretDescriptor("retry-private") {
		t.Fatalf("retry descriptor = %q", retry.config.SecretKey)
	}
	retry.Stop()

	const concurrentRaw = "$ENV://ROCKETMQ_SINGLEFLIGHT"
	concurrentConfig := rocketMQRawConfig(concurrentRaw, false)
	concurrentCapability, concurrentScope, concurrentBroker := newRocketMQScopedSecretHarness(
		t, 121, "rocketmq-singleflight", concurrentConfig,
		map[string]string{concurrentRaw: "singleflight-private"},
	)
	concurrent := newRawRocketMQPlugin(t, concurrentConfig)
	start := make(chan struct{})
	errs := make(chan error, 16)
	var group sync.WaitGroup
	for range 16 {
		group.Go(func() {
			<-start
			errs <- base.MaterializeScopedPluginSecrets(
				context.Background(), concurrentScope, concurrentCapability, concurrent,
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
	if calls := concurrentBroker.callsSnapshot(); len(calls) != 1 {
		t.Fatalf("singleflight calls = %#v, want one", calls)
	}
	concurrent.Stop()
}

func TestMaterializeScopedSecretsAllowsTLSAndOwnsSecret(t *testing.T) {
	const tlsRaw = "$secret://vault/rocketmq/tls-ordering"
	tlsConfig := rocketMQRawConfig(tlsRaw, true)
	tlsCapability, tlsScope, tlsBroker := newRocketMQScopedSecretHarness(
		t, 122, "rocketmq-tls", tlsConfig, map[string]string{tlsRaw: "tls-private"},
	)
	tlsPlugin := newRawRocketMQPlugin(t, tlsConfig)
	t.Cleanup(tlsPlugin.Stop)

	err := base.MaterializeScopedPluginSecrets(
		context.Background(), tlsScope, tlsCapability, tlsPlugin,
	)
	if err != nil {
		t.Fatalf("TLS secret materialization error = %v", err)
	}
	calls := tlsBroker.callsSnapshot()
	wantScope := tlsScope
	wantScope.Field = "secret_key"
	if len(calls) != 1 || calls[0].Scope != wantScope || calls[0].Raw != tlsRaw {
		t.Fatalf("TLS resolver calls = %#v, want exact secret_key scope %#v", calls, wantScope)
	}
	if !tlsPlugin.secretsPrepared || !tlsPlugin.secretKeySet || !tlsPlugin.config.UseTLS ||
		tlsPlugin.config.SecretKey != rocketMQSecretDescriptor("tls-private") ||
		tlsPlugin.sender != nil || tlsPlugin.BatchProcessor != nil {
		t.Fatal("TLS materialization did not install only the attempt-owned secret state")
	}
}

type attemptOwnedRocketMQSender struct {
	captureSender
	mu            sync.Mutex
	secretKey     string
	shutdownCalls int
}

func (sender *attemptOwnedRocketMQSender) Shutdown() error {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	sender.shutdownCalls++
	return nil
}

func (sender *attemptOwnedRocketMQSender) shutdownCount() int {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	return sender.shutdownCalls
}

func TestRocketMQSendersAndPrivateConfigClonesAreAttemptOwned(t *testing.T) {
	const raw = "$secret://vault/rocketmq/generation-secret"
	newGeneration := func(
		revision uint64,
		private string,
	) (*Plugin, *attemptOwnedRocketMQSender, *Config, *apisixruntime.TaskRegistry) {
		rawConfig := rocketMQRawConfig(raw, false)
		capabilityValue, scope, _ := newRocketMQScopedSecretHarness(
			t, revision, fmt.Sprintf("rocketmq-generation-%d", revision), rawConfig,
			map[string]string{raw: private},
		)
		p := newRawRocketMQPlugin(t, rawConfig)
		tasks, owner := newRocketMQTaskRegistryForTest(
			t, fmt.Sprintf("plugin/test/rocketmq_logger/generation-%d", revision),
		)
		p.SetDependencies(base.Dependencies{Tasks: owner})
		var retained *Config
		var sender *attemptOwnedRocketMQSender
		p.senderFactory = func(config *Config) (rocketmqSender, error) {
			if config.SecretKey != private {
				t.Fatalf("private sender config secret_key = %q, want %q", config.SecretKey, private)
			}
			retained = config
			sender = &attemptOwnedRocketMQSender{secretKey: config.SecretKey}
			return sender, nil
		}
		if err := base.MaterializeScopedPluginSecrets(
			context.Background(), scope, capabilityValue, p,
		); err != nil {
			t.Fatal(err)
		}
		if err := p.PostInit(); err != nil {
			t.Fatal(err)
		}
		return p, sender, retained, tasks
	}

	first, firstSender, firstClone, firstTasks := newGeneration(130, "generation-first")
	second, secondSender, secondClone, secondTasks := newGeneration(131, "generation-second")
	if firstSender == secondSender {
		t.Fatal("two attempts shared a credential-bearing RocketMQ sender")
	}
	if firstSender.secretKey != "generation-first" || secondSender.secretKey != "generation-second" {
		t.Fatalf("attempt sender secrets = %q/%q", firstSender.secretKey, secondSender.secretKey)
	}
	if first.config.SecretKey != rocketMQSecretDescriptor("generation-first") ||
		second.config.SecretKey != rocketMQSecretDescriptor("generation-second") {
		t.Fatalf("public descriptors = %q/%q", first.config.SecretKey, second.config.SecretKey)
	}
	if firstClone == nil || secondClone == nil || firstClone.SecretKey != "" || secondClone.SecretKey != "" {
		t.Fatalf("retained private clones were not cleared: %#v/%#v", firstClone, secondClone)
	}
	if first.senderFactory != nil || second.senderFactory != nil {
		t.Fatal("sender factory callback survived sender construction")
	}
	firstProcessor := first.BatchProcessor
	first.QuiesceGenerationTasks()
	stopRocketMQTaskRegistryForTest(t, firstTasks)
	first.Stop()
	if err := firstProcessor.Shutdown(context.Background()); err != nil {
		t.Fatalf("first batch Shutdown() error = %v", err)
	}
	if firstSender.shutdownCount() != 1 || secondSender.shutdownCount() != 0 {
		t.Fatalf(
			"sender shutdown counts after first Stop = %d/%d, want 1/0",
			firstSender.shutdownCount(), secondSender.shutdownCount(),
		)
	}
	if second.sender != secondSender || second.stopped.Load() {
		t.Fatal("stopping first generation changed the second sender")
	}
	secondProcessor := second.BatchProcessor
	second.QuiesceGenerationTasks()
	stopRocketMQTaskRegistryForTest(t, secondTasks)
	second.Stop()
	if err := secondProcessor.Shutdown(context.Background()); err != nil {
		t.Fatalf("second batch Shutdown() error = %v", err)
	}
}

type blockingRocketMQSender struct {
	entered       chan struct{}
	release       chan struct{}
	enterOnce     sync.Once
	mu            sync.Mutex
	shutdownCalls int
}

type ownedShutdownRocketMQSender struct {
	sendEntered     chan struct{}
	sendRelease     chan struct{}
	shutdownEntered chan struct{}
	shutdownRelease chan struct{}

	sendOnce     sync.Once
	shutdownOnce sync.Once
	mu           sync.Mutex
	shutdowns    int
}

func (sender *ownedShutdownRocketMQSender) Send(ctx context.Context, _ rocketmqMessage) error {
	if sender.sendEntered != nil {
		sender.sendOnce.Do(func() { close(sender.sendEntered) })
	}
	if sender.sendRelease == nil {
		return nil
	}
	select {
	case <-sender.sendRelease:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (sender *ownedShutdownRocketMQSender) Shutdown() error {
	sender.mu.Lock()
	sender.shutdowns++
	sender.mu.Unlock()
	if sender.shutdownEntered != nil {
		sender.shutdownOnce.Do(func() { close(sender.shutdownEntered) })
	}
	if sender.shutdownRelease != nil {
		<-sender.shutdownRelease
	}
	return nil
}

func (sender *ownedShutdownRocketMQSender) shutdownCount() int {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	return sender.shutdowns
}

func newOwnedShutdownRocketMQPluginForTest(
	t *testing.T,
	owner *apisixruntime.TaskOwner,
	sender rocketmqSender,
) *Plugin {
	t.Helper()
	p := newRawRocketMQPlugin(t, rocketMQRawConfig("owned-shutdown-private", false))
	p.SetDependencies(base.Dependencies{
		Tasks:          owner,
		DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
	})
	p.sender = sender
	if err := materializeRocketMQForTest(t, p, 1, "owned-shutdown", p.config.SecretKey); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRocketMQSenderShutdownIsOwnedAndResidualVisible(t *testing.T) {
	const ownerPrefix = "plugin/test/rocketmq_logger/owned-shutdown"
	tasks := apisixruntime.NewTaskRegistry(context.Background(), nil)
	owner, err := apisixruntime.NewTaskOwner(tasks, ownerPrefix, apisixruntime.TaskPlugin)
	if err != nil {
		t.Fatal(err)
	}
	shutdownEntered := make(chan struct{})
	shutdownRelease := make(chan struct{})
	var releaseOnce sync.Once
	var p *Plugin
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(shutdownRelease) })
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if residuals, stopErr := tasks.Stop(ctx); stopErr != nil {
			t.Errorf("TaskRegistry.Stop() residuals=%v error=%v", residuals, stopErr)
		}
		if p != nil {
			p.Stop()
		}
	})
	sender := &ownedShutdownRocketMQSender{
		shutdownEntered: shutdownEntered,
		shutdownRelease: shutdownRelease,
	}
	p = newOwnedShutdownRocketMQPluginForTest(t, owner, sender)

	type stopResult struct {
		residuals []apisixruntime.TaskResidual
		err       error
	}
	stopDone := make(chan stopResult, 1)
	go func() {
		residuals, stopErr := tasks.Stop(context.Background())
		stopDone <- stopResult{residuals: residuals, err: stopErr}
	}()
	select {
	case <-shutdownEntered:
	case <-time.After(time.Second):
		t.Fatal("generation cancellation did not enter owned RocketMQ sender Shutdown")
	}
	lateSend := make(chan error, 1)
	go func() {
		_, sendErr := p.SendBatch(context.Background(), []map[string]any{{"late": true}}, 1)
		lateSend <- sendErr
	}()
	select {
	case sendErr := <-lateSend:
		if !errors.Is(sendErr, secret.ErrCredentialUnavailable) {
			t.Fatalf("SendBatch() after shutdown began error = %v, want credential unavailable", sendErr)
		}
	case <-time.After(20 * time.Millisecond):
		t.Fatal("SendBatch() after shutdown began blocked behind sender Shutdown")
	}
	wantOwner := ownerPrefix + "/rocketmq-sender-shutdown"
	deadline := time.Now().Add(time.Second)
	for active := tasks.Active(); len(active) != 1 || active[0] != wantOwner; active = tasks.Active() {
		if time.Now().After(deadline) {
			t.Fatalf("active task owners = %v, want [%s]", active, wantOwner)
		}
		time.Sleep(time.Millisecond)
	}
	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	residuals, stopErr := tasks.Stop(short)
	cancel()
	var residualErr *apisixruntime.TaskResidualError
	if !errors.As(stopErr, &residualErr) || !errors.Is(stopErr, context.DeadlineExceeded) {
		t.Fatalf("TaskRegistry.Stop() residuals=%v error=%v, want deadline residual", residuals, stopErr)
	}
	wantResiduals := []apisixruntime.TaskResidual{{Owner: wantOwner}}
	if len(residuals) != 1 || residuals[0] != wantResiduals[0] ||
		len(residualErr.Residuals()) != 1 || residualErr.Residuals()[0] != wantResiduals[0] {
		t.Fatalf(
			"TaskRegistry.Stop() residuals=%v stored=%v, want %v",
			residuals,
			residualErr.Residuals(),
			wantResiduals,
		)
	}
	if sender.shutdownCount() != 1 {
		t.Fatalf("sender shutdown count while blocked = %d, want 1", sender.shutdownCount())
	}

	releaseOnce.Do(func() { close(shutdownRelease) })
	select {
	case result := <-stopDone:
		if result.err != nil || len(result.residuals) != 0 {
			t.Fatalf("TaskRegistry.Stop() after release residuals=%v error=%v", result.residuals, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("owned RocketMQ sender shutdown task did not join after release")
	}
	if sender.shutdownCount() != 1 {
		t.Fatalf("sender shutdown count after join = %d, want 1", sender.shutdownCount())
	}
}

func TestRocketMQGenerationCancellationFlushesBeforeSenderShutdown(t *testing.T) {
	const ownerPrefix = "plugin/test/rocketmq_logger/cancel-flush"
	tasks := apisixruntime.NewTaskRegistry(context.Background(), nil)
	owner, err := apisixruntime.NewTaskOwner(tasks, ownerPrefix, apisixruntime.TaskPlugin)
	if err != nil {
		t.Fatal(err)
	}
	sendEntered := make(chan struct{})
	sendRelease := make(chan struct{})
	shutdownEntered := make(chan struct{})
	shutdownRelease := make(chan struct{})
	var sendReleaseOnce sync.Once
	var shutdownReleaseOnce sync.Once
	var p *Plugin
	t.Cleanup(func() {
		sendReleaseOnce.Do(func() { close(sendRelease) })
		shutdownReleaseOnce.Do(func() { close(shutdownRelease) })
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if residuals, stopErr := tasks.Stop(ctx); stopErr != nil {
			t.Errorf("TaskRegistry.Stop() residuals=%v error=%v", residuals, stopErr)
		}
		if p != nil {
			p.Stop()
		}
	})
	sender := &ownedShutdownRocketMQSender{
		sendEntered:     sendEntered,
		sendRelease:     sendRelease,
		shutdownEntered: shutdownEntered,
		shutdownRelease: shutdownRelease,
	}
	p = newOwnedShutdownRocketMQPluginForTest(t, owner, sender)
	if err := p.RunLogPhase(base.LogSnapshot{}); err != nil {
		t.Fatal(err)
	}

	stopDone := make(chan error, 1)
	go func() {
		_, stopErr := tasks.Stop(context.Background())
		stopDone <- stopErr
	}()
	select {
	case <-sendEntered:
	case <-time.After(time.Second):
		t.Fatal("generation cancellation did not flush the pending RocketMQ batch")
	}
	select {
	case <-shutdownEntered:
		t.Fatal("RocketMQ sender Shutdown started before pending delivery completed")
	default:
	}
	sendReleaseOnce.Do(func() { close(sendRelease) })
	select {
	case <-shutdownEntered:
	case <-time.After(time.Second):
		t.Fatal("RocketMQ sender Shutdown did not start after pending delivery completed")
	}
	select {
	case err := <-stopDone:
		t.Fatalf("TaskRegistry.Stop() returned before sender Shutdown completed: %v", err)
	default:
	}
	shutdownReleaseOnce.Do(func() { close(shutdownRelease) })
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("TaskRegistry.Stop() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("generation cancellation did not join RocketMQ sender Shutdown")
	}
	if sender.shutdownCount() != 1 {
		t.Fatalf("sender shutdown count = %d, want 1", sender.shutdownCount())
	}
}

func TestRocketMQRegistryStopWaitsForDirectSendBeforeSenderShutdown(t *testing.T) {
	const ownerPrefix = "plugin/test/rocketmq_logger/direct-send-shutdown"
	tasks := apisixruntime.NewTaskRegistry(context.Background(), nil)
	owner, err := apisixruntime.NewTaskOwner(tasks, ownerPrefix, apisixruntime.TaskPlugin)
	if err != nil {
		t.Fatal(err)
	}
	sendEntered := make(chan struct{})
	sendRelease := make(chan struct{})
	shutdownEntered := make(chan struct{})
	shutdownRelease := make(chan struct{})
	var sendReleaseOnce sync.Once
	var shutdownReleaseOnce sync.Once
	var p *Plugin
	t.Cleanup(func() {
		sendReleaseOnce.Do(func() { close(sendRelease) })
		shutdownReleaseOnce.Do(func() { close(shutdownRelease) })
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if residuals, stopErr := tasks.Stop(ctx); stopErr != nil {
			t.Errorf("TaskRegistry.Stop() residuals=%v error=%v", residuals, stopErr)
		}
		if p != nil {
			p.Stop()
		}
	})
	sender := &ownedShutdownRocketMQSender{
		sendEntered:     sendEntered,
		sendRelease:     sendRelease,
		shutdownEntered: shutdownEntered,
		shutdownRelease: shutdownRelease,
	}
	p = newOwnedShutdownRocketMQPluginForTest(t, owner, sender)

	sendDone := make(chan error, 1)
	go func() {
		_, sendErr := p.SendBatch(context.Background(), []map[string]any{{"active": true}}, 1)
		sendDone <- sendErr
	}()
	select {
	case <-sendEntered:
	case <-time.After(time.Second):
		t.Fatal("direct SendBatch did not enter sender")
	}

	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	residuals, stopErr := tasks.Stop(short)
	cancel()
	wantOwner := ownerPrefix + "/rocketmq-sender-shutdown"
	var residualErr *apisixruntime.TaskResidualError
	if !errors.As(stopErr, &residualErr) || !errors.Is(stopErr, context.DeadlineExceeded) {
		t.Fatalf("TaskRegistry.Stop() residuals=%v error=%v, want deadline residual", residuals, stopErr)
	}
	wantResidual := apisixruntime.TaskResidual{Owner: wantOwner}
	if len(residuals) != 1 || residuals[0] != wantResidual ||
		len(residualErr.Residuals()) != 1 || residualErr.Residuals()[0] != wantResidual {
		t.Fatalf(
			"TaskRegistry.Stop() residuals=%v stored=%v, want [%v]",
			residuals,
			residualErr.Residuals(),
			wantResidual,
		)
	}
	select {
	case <-shutdownEntered:
		t.Fatal("sender Shutdown entered while direct SendBatch remained blocked")
	default:
	}
	if sender.shutdownCount() != 0 {
		t.Fatalf("sender shutdown count while direct send blocked = %d, want 0", sender.shutdownCount())
	}

	sendReleaseOnce.Do(func() { close(sendRelease) })
	select {
	case sendErr := <-sendDone:
		if sendErr != nil {
			t.Fatalf("direct SendBatch() error = %v", sendErr)
		}
	case <-time.After(time.Second):
		t.Fatal("direct SendBatch did not finish after release")
	}
	select {
	case <-shutdownEntered:
	case <-time.After(time.Second):
		t.Fatal("sender Shutdown did not start after direct SendBatch completed")
	}
	if sender.shutdownCount() != 1 {
		t.Fatalf("sender shutdown count after direct send = %d, want 1", sender.shutdownCount())
	}

	joinDone := make(chan error, 1)
	go func() {
		_, joinErr := tasks.Stop(context.Background())
		joinDone <- joinErr
	}()
	select {
	case joinErr := <-joinDone:
		t.Fatalf("TaskRegistry.Stop() returned before sender Shutdown completed: %v", joinErr)
	default:
	}
	shutdownReleaseOnce.Do(func() { close(shutdownRelease) })
	select {
	case joinErr := <-joinDone:
		if joinErr != nil {
			t.Fatalf("TaskRegistry.Stop() join error = %v", joinErr)
		}
	case <-time.After(time.Second):
		t.Fatal("TaskRegistry.Stop() did not join sender Shutdown after release")
	}
	if sender.shutdownCount() != 1 {
		t.Fatalf("sender shutdown count after registry join = %d, want 1", sender.shutdownCount())
	}
}

func TestRocketMQRegistryCancellationBeforePublicationRollsBackRuntime(t *testing.T) {
	const ownerPrefix = "plugin/test/rocketmq_logger/pre-publication-cancel"
	tasks := apisixruntime.NewTaskRegistry(context.Background(), nil)
	owner, err := apisixruntime.NewTaskOwner(tasks, ownerPrefix, apisixruntime.TaskPlugin)
	if err != nil {
		t.Fatal(err)
	}
	checkpointEntered := make(chan struct{})
	checkpointRelease := make(chan struct{})
	shutdownEntered := make(chan struct{})
	shutdownRelease := make(chan struct{})
	var checkpointReleaseOnce sync.Once
	var shutdownReleaseOnce sync.Once
	t.Cleanup(func() {
		checkpointReleaseOnce.Do(func() { close(checkpointRelease) })
		shutdownReleaseOnce.Do(func() { close(shutdownRelease) })
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if residuals, stopErr := tasks.Stop(ctx); stopErr != nil {
			t.Errorf("TaskRegistry.Stop() residuals=%v error=%v", residuals, stopErr)
		}
	})
	sender := &ownedShutdownRocketMQSender{
		shutdownEntered: shutdownEntered,
		shutdownRelease: shutdownRelease,
	}
	p := newRawRocketMQPlugin(t, rocketMQRawConfig("pre-publication-private", false))
	p.SetDependencies(base.Dependencies{
		Tasks:          owner,
		DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
	})
	if err := materializeRocketMQForTest(t, p, 1, "pre-publication-cancel", p.config.SecretKey); err != nil {
		t.Fatal(err)
	}
	p.senderFactory = func(config *Config) (rocketmqSender, error) {
		if config.SecretKey != "pre-publication-private" {
			t.Fatalf("private sender config secret_key = %q", config.SecretKey)
		}
		return sender, nil
	}
	p.beforeRuntimePublication = func() {
		close(checkpointEntered)
		<-checkpointRelease
	}
	postInitDone := make(chan error, 1)
	go func() { postInitDone <- p.PostInit() }()
	select {
	case <-checkpointEntered:
	case <-time.After(time.Second):
		t.Fatal("PostInit did not reach the pre-publication checkpoint")
	}
	if p.sender != nil || p.BatchProcessor != nil || p.ready {
		t.Fatalf("runtime fields published before checkpoint release: %#v", p)
	}

	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	residuals, stopErr := tasks.Stop(short)
	cancel()
	var residualErr *apisixruntime.TaskResidualError
	if !errors.As(stopErr, &residualErr) || !errors.Is(stopErr, context.DeadlineExceeded) {
		t.Fatalf("TaskRegistry.Stop() residuals=%v error=%v, want deadline residual", residuals, stopErr)
	}
	checkpointReleaseOnce.Do(func() { close(checkpointRelease) })
	select {
	case <-shutdownEntered:
	case <-time.After(time.Second):
		t.Fatal("staged sender Shutdown did not start after publication rollback")
	}
	if p.sender != nil || p.BatchProcessor != nil || p.ready {
		t.Fatalf("canceled PostInit published runtime fields: %#v", p)
	}
	if sender.shutdownCount() != 1 {
		t.Fatalf("staged sender shutdown count = %d, want 1", sender.shutdownCount())
	}
	select {
	case postInitErr := <-postInitDone:
		t.Fatalf("PostInit returned before sender Shutdown completed: %v", postInitErr)
	default:
	}

	shutdownReleaseOnce.Do(func() { close(shutdownRelease) })
	select {
	case postInitErr := <-postInitDone:
		if !errors.Is(postInitErr, secret.ErrCredentialUnavailable) {
			t.Fatalf("PostInit() error = %v, want credential unavailable", postInitErr)
		}
	case <-time.After(time.Second):
		t.Fatal("PostInit did not finish after staged sender shutdown")
	}
	ctx, joinCancel := context.WithTimeout(context.Background(), time.Second)
	joinedResiduals, joinErr := tasks.Stop(ctx)
	joinCancel()
	if joinErr != nil || len(joinedResiduals) != 0 {
		t.Fatalf("TaskRegistry.Stop() after release residuals=%v error=%v", joinedResiduals, joinErr)
	}
	if p.sender != nil || p.BatchProcessor != nil || p.ready {
		t.Fatalf("failed PostInit retained runtime fields: %#v", p)
	}
	if sender.shutdownCount() != 1 {
		t.Fatalf("staged sender shutdown count after join = %d, want 1", sender.shutdownCount())
	}
}

func (sender *blockingRocketMQSender) Send(context.Context, rocketmqMessage) error {
	sender.enterOnce.Do(func() { close(sender.entered) })
	<-sender.release
	return nil
}

func (sender *blockingRocketMQSender) Shutdown() error {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	sender.shutdownCalls++
	return nil
}

func (sender *blockingRocketMQSender) shutdownCount() int {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	return sender.shutdownCalls
}

func newRocketMQTaskRegistryForTest(
	t *testing.T,
	ownerPrefix string,
) (*apisixruntime.TaskRegistry, *apisixruntime.TaskOwner) {
	t.Helper()
	tasks := apisixruntime.NewTaskRegistry(context.Background(), nil)
	owner, err := apisixruntime.NewTaskOwner(tasks, ownerPrefix, apisixruntime.TaskPlugin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopRocketMQTaskRegistryForTest(t, tasks) })
	return tasks, owner
}

func stopRocketMQTaskRegistryForTest(t *testing.T, tasks *apisixruntime.TaskRegistry) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if residuals, err := tasks.Stop(ctx); err != nil {
		t.Fatalf("TaskRegistry.Stop() residuals=%v error=%v", residuals, err)
	}
}

func newBlockingRocketMQPlugin(
	t *testing.T,
	sender rocketmqSender,
) (*Plugin, *apisixruntime.TaskRegistry) {
	t.Helper()
	tasks, owner := newRocketMQTaskRegistryForTest(t, "plugin/test/rocketmq_logger/blocking")
	p := newRawRocketMQPlugin(t, rocketMQRawConfig("active-private", false))
	p.SetDependencies(base.Dependencies{
		Tasks:          owner,
		DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
	})
	p.sender = sender
	if err := materializeRocketMQForTest(t, p, 1, "blocking", p.config.SecretKey); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	return p, tasks
}

func TestRocketMQQuiesceFlushesPendingBatchBeforeSenderShutdown(t *testing.T) {
	sender := &blockingRocketMQSender{entered: make(chan struct{}), release: make(chan struct{})}
	p, tasks := newBlockingRocketMQPlugin(t, sender)
	if err := p.RunLogPhase(base.LogSnapshot{}); err != nil {
		t.Fatal(err)
	}
	processor := p.BatchProcessor
	quiesceDone := make(chan struct{})
	go func() {
		p.QuiesceGenerationTasks()
		close(quiesceDone)
	}()
	select {
	case <-sender.entered:
	case <-time.After(time.Second):
		t.Fatal("pending RocketMQ batch was not flushed during quiesce")
	}
	if sender.shutdownCount() != 0 {
		t.Fatal("sender shut down before pending batch completed")
	}
	select {
	case <-quiesceDone:
	case <-time.After(time.Second):
		t.Fatal("quiesce blocked on the pending RocketMQ batch")
	}
	close(sender.release)
	if err := processor.Shutdown(context.Background()); err != nil {
		t.Fatalf("batch Shutdown() error = %v", err)
	}
	stopRocketMQTaskRegistryForTest(t, tasks)
	p.Stop()
	if sender.shutdownCount() != 1 {
		t.Fatalf("sender shutdown count = %d, want 1 after pending flush", sender.shutdownCount())
	}
}

func TestRocketMQRejectsLogEnqueueBeforePostInit(t *testing.T) {
	p := newRawRocketMQPlugin(t, rocketMQRawConfig("pre-init-private", false))
	p.SetDependencies(base.Dependencies{
		Tasks:          newLoggerTestTaskOwner(t),
		DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
	})
	queued := len(p.FireChan)
	if err := p.RunLogPhase(base.LogSnapshot{}); !errors.Is(err, base.ErrLogQueueUnavailable) {
		t.Fatalf("pre-materialization RunLogPhase() error = %v", err)
	}
	if len(p.FireChan) != queued {
		t.Fatal("pre-materialization RunLogPhase enqueued work")
	}
	if err := materializeRocketMQForTest(t, p, 1, "pre-post-init", p.config.SecretKey); err != nil {
		t.Fatal(err)
	}
	if err := p.RunLogPhase(base.LogSnapshot{}); !errors.Is(err, base.ErrLogQueueUnavailable) {
		t.Fatalf("pre-PostInit RunLogPhase() error = %v", err)
	}
	if len(p.FireChan) != queued {
		t.Fatal("pre-PostInit RunLogPhase enqueued work")
	}
	p.Stop()
}

func TestRocketMQPostInitIsIdempotent(t *testing.T) {
	sender := &blockingRocketMQSender{entered: make(chan struct{}), release: make(chan struct{})}
	p, tasks := newBlockingRocketMQPlugin(t, sender)
	firstProcessor := p.BatchProcessor
	if err := p.PostInit(); err != nil {
		t.Fatalf("second PostInit() error = %v", err)
	}
	if p.BatchProcessor != firstProcessor {
		t.Fatal("second PostInit replaced the active batch processor")
	}
	p.QuiesceGenerationTasks()
	stopRocketMQTaskRegistryForTest(t, tasks)
	p.Stop()
	if err := firstProcessor.Shutdown(context.Background()); err != nil {
		t.Fatalf("batch Shutdown() error = %v", err)
	}
	if sender.shutdownCount() != 1 {
		t.Fatalf("sender shutdown count = %d, want 1", sender.shutdownCount())
	}
}

func TestRocketMQConcurrentPostInitAndStopCannotPublishSender(t *testing.T) {
	p := newRawRocketMQPlugin(t, rocketMQRawConfig("post-init-private", false))
	p.SetDependencies(base.Dependencies{
		Tasks:          newLoggerTestTaskOwner(t),
		DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
	})
	if err := materializeRocketMQForTest(t, p, 1, "concurrent-post-init", p.config.SecretKey); err != nil {
		t.Fatal(err)
	}
	factoryEntered := make(chan struct{})
	releaseFactory := make(chan struct{})
	sender := &attemptOwnedRocketMQSender{}
	p.senderFactory = func(config *Config) (rocketmqSender, error) {
		if config.SecretKey != "post-init-private" {
			t.Fatalf("private sender config secret_key = %q", config.SecretKey)
		}
		close(factoryEntered)
		<-releaseFactory
		return sender, nil
	}
	postInitDone := make(chan error, 1)
	go func() { postInitDone <- p.PostInit() }()
	select {
	case <-factoryEntered:
	case <-time.After(time.Second):
		t.Fatal("PostInit did not enter sender construction")
	}
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	deadline := time.Now().Add(time.Second)
	for !p.stopped.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !p.stopped.Load() {
		close(releaseFactory)
		t.Fatal("concurrent Stop did not retire plugin")
	}
	close(releaseFactory)
	if err := <-postInitDone; !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("concurrent PostInit() error = %v", err)
	}
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("concurrent Stop did not finish")
	}
	if p.sender != nil || p.BatchProcessor != nil || p.secretKeySet ||
		p.secretsPrepared {
		t.Fatalf("concurrent PostInit/Stop published state: %#v", p)
	}
	if sender.shutdownCount() != 1 {
		t.Fatalf("staged sender shutdown count = %d, want 1", sender.shutdownCount())
	}
	p.Stop()
}

func TestRunLogPhaseOriginPreservesHTTPFraming(t *testing.T) {
	delivered := make(chan map[string]any, 1)
	p := &Plugin{config: Config{MetaFormat: "origin", IncludeReqBody: true, MaxReqBodyBytes: 64}, ready: true}
	p.BatchProcessor = newOwnedBatchProcessorForTest(t, logger_batch.Config{
		BatchMaxSize: 1, MaxPendingEntries: 1, InactiveTimeout: time.Hour,
		BufferDuration: time.Hour, ShutdownTimeout: time.Second,
	}, func(_ context.Context, entries []map[string]any, _ int) (int, error) {
		delivered <- entries[0]
		return 0, nil
	})
	t.Cleanup(p.Stop)
	snapshot := base.LogSnapshot{
		Request: apisixlog.RequestLogSnapshot{
			Method: http.MethodPost, URI: "/orders?x=1", Proto: "HTTP/1.1",
			Header: http.Header{"X-Test": {"one"}}, Body: []byte("payload"),
		},
		Outcome: apisixctx.ResponseOutcome{Status: http.StatusOK},
	}
	if err := p.RunLogPhase(snapshot); err != nil {
		t.Fatalf("RunLogPhase() error = %v", err)
	}
	select {
	case entry := <-delivered:
		origin, ok := entry[originLogKey].(string)
		if !ok || !strings.Contains(origin, "POST /orders?x=1 HTTP/1.1\r\nX-Test: one\r\n\r\npayload") {
			t.Fatalf("origin payload = %q", origin)
		}
	case <-time.After(time.Second):
		t.Fatal("detached RocketMQ origin entry was not delivered")
	}
}

func TestRunLogPhaseAddsMatchedRouteToCustomFormat(t *testing.T) {
	delivered := make(chan map[string]any, 1)
	p := &Plugin{ready: true}
	p.LogFormat = map[string]string{"x_ip": "$remote_addr"}
	p.RouteID = "fallback-route"
	p.BatchProcessor = newOwnedBatchProcessorForTest(t, logger_batch.Config{
		BatchMaxSize: 1, MaxPendingEntries: 1, InactiveTimeout: time.Hour,
		BufferDuration: time.Hour, ShutdownTimeout: time.Second,
	}, func(_ context.Context, entries []map[string]any, _ int) (int, error) {
		delivered <- entries[0]
		return 0, nil
	})
	t.Cleanup(p.Stop)
	snapshot := base.LogSnapshot{Request: apisixlog.RequestLogSnapshot{
		RemoteAddr: "127.0.0.1:32000",
		APISIXVars: map[string]any{"$route_id": "matched-route"},
	}}
	if err := p.RunLogPhase(snapshot); err != nil {
		t.Fatalf("RunLogPhase() error = %v", err)
	}
	select {
	case entry := <-delivered:
		if entry["x_ip"] != "127.0.0.1" || entry["route_id"] != "matched-route" {
			t.Fatalf("custom RocketMQ entry = %#v", entry)
		}
	case <-time.After(time.Second):
		t.Fatal("detached RocketMQ custom entry was not delivered")
	}
}

type captureSender struct {
	mu       sync.Mutex
	messages []rocketmqMessage
}

type contextCaptureSender struct {
	ctx context.Context
}

func (s *contextCaptureSender) Send(ctx context.Context, _ rocketmqMessage) error {
	s.ctx = ctx
	return ctx.Err()
}

func TestSendBatchPassesParentContextToRocketMQ(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()

	sender := &contextCaptureSender{}
	p := &Plugin{config: Config{Timeout: 30}, sender: sender}
	_, err := p.SendBatch(parent, []map[string]any{{"route_id": "r1"}}, 1)
	if err == nil {
		t.Fatal("SendBatch() error = nil, want canceled parent error")
	}
	if sender.ctx == nil || sender.ctx.Err() == nil {
		t.Fatal("RocketMQ sender did not receive the canceled parent context")
	}
}

func TestSendBatchPreservesRocketMQMarshalErrorContext(t *testing.T) {
	p := &Plugin{}
	_, err := p.SendBatch(context.Background(), []map[string]any{{"bad": make(chan int)}}, 1)
	if err == nil || !strings.Contains(err.Error(), "failed to marshal rocketmq log message") {
		t.Fatalf("SendBatch() error = %v, want rocketmq marshal context", err)
	}
}

type shutdownSender struct {
	captureSender
	shutdown bool
}

func (s *shutdownSender) Shutdown() error {
	s.shutdown = true
	return nil
}

func (s *captureSender) Send(ctx context.Context, message rocketmqMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.messages = append(s.messages, message)
	return nil
}

func (s *captureSender) waitForMessage(t *testing.T) rocketmqMessage {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		if len(s.messages) > 0 {
			message := s.messages[0]
			s.mu.Unlock()
			return message
		}
		s.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("timed out waiting for rocketmq message")
	return rocketmqMessage{}
}

func (s *captureSender) waitForMessages(t *testing.T, count int) []rocketmqMessage {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		if len(s.messages) >= count {
			messages := append([]rocketmqMessage(nil), s.messages[:count]...)
			s.mu.Unlock()
			return messages
		}
		s.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %d rocketmq messages", count)
	return nil
}

func newTestPlugin(t *testing.T, cfg Config, sender rocketmqSender) *Plugin {
	t.Helper()
	return newTestPluginWithMetadata(t, cfg, sender, nil)
}

func newTestPluginWithMetadata(
	t *testing.T,
	cfg Config,
	sender rocketmqSender,
	metadata map[string]any,
) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg, sender: sender}
	p.SetDependencies(base.Dependencies{
		Tasks:          newLoggerTestTaskOwner(t),
		DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
		Metadata:       mustMetadataView(t, metadata),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.sender = sender
	rawConfig := make(map[string]any)
	document, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if err := json.Unmarshal(document, &rawConfig); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	values := map[string]string{cfg.SecretKey: cfg.SecretKey}
	capabilityValue, scope, _ := newRocketMQScopedSecretHarness(t, 1, "test-route", rawConfig, values)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	return p
}

func materializeRocketMQForTest(
	t *testing.T,
	p *Plugin,
	revision uint64,
	resourceID string,
	resolvedSecretKey string,
	keyring ...string,
) error {
	t.Helper()
	rawConfig := make(map[string]any)
	document, err := json.Marshal(p.config)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(document, &rawConfig); err != nil {
		return err
	}
	values := map[string]string{}
	if p.config.SecretKey != "" {
		values[p.config.SecretKey] = resolvedSecretKey
	}
	capabilityValue, scope, _ := newRocketMQScopedSecretHarness(
		t, revision, resourceID, rawConfig, values, keyring...,
	)
	return base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
}

func mustMetadataView(t *testing.T, metadata map[string]any) apisixruntime.MetadataView {
	t.Helper()
	if len(metadata) == 0 {
		return apisixruntime.MetadataView{}
	}
	document, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	view, err := apisixruntime.NewMetadataView(map[string][]byte{name: document})
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	return view
}

func TestSendEncodesLogAndPublishesToConfiguredTopic(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		NameServerList: []string{"127.0.0.1:9876"},
		Topic:          "apisix-logs",
		Key:            "route-a",
		Tag:            "access",
	}, sender)

	p.Send(map[string]any{
		"route_id": "r1",
		"status":   200,
	})

	message := sender.waitForMessage(t)
	if message.Topic != "apisix-logs" {
		t.Fatalf("topic = %q, want apisix-logs", message.Topic)
	}
	if message.Key != "route-a" {
		t.Fatalf("key = %q, want route-a", message.Key)
	}
	if message.Tag != "access" {
		t.Fatalf("tag = %q, want access", message.Tag)
	}

	var payload map[string]any
	if err := json.Unmarshal(message.Body, &payload); err != nil {
		t.Fatalf("unmarshal rocketmq payload: %v", err)
	}
	if payload["route_id"] != "r1" {
		t.Fatalf("route_id = %v, want r1", payload["route_id"])
	}
	if payload["status"].(float64) != 200 {
		t.Fatalf("status = %v, want 200", payload["status"])
	}
}

func TestGenerationCancellationShutsDownRocketMQSender(t *testing.T) {
	sender := &shutdownSender{}
	tasks, owner := newRocketMQTaskRegistryForTest(t, "plugin/test/rocketmq_logger/shutdown")
	p := newOwnedShutdownRocketMQPluginForTest(t, owner, sender)
	processor := p.BatchProcessor
	p.QuiesceGenerationTasks()
	stopRocketMQTaskRegistryForTest(t, tasks)
	p.Stop()
	if err := processor.Shutdown(context.Background()); err != nil {
		t.Fatalf("batch Shutdown() error = %v", err)
	}
	if !sender.shutdown {
		t.Fatal("generation cancellation did not shut down the sender")
	}
}

func TestPostInitAppliesDefaults(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		NameServerList: []string{"127.0.0.1:9876"},
		Topic:          "apisix-logs",
	}, sender)

	if p.config.MetaFormat != "default" {
		t.Fatalf("meta_format = %q, want default", p.config.MetaFormat)
	}
	if p.config.Timeout != 3 {
		t.Fatalf("timeout = %d, want 3", p.config.Timeout)
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
}

func TestPostInitPublishesTLSEnabledRuntime(t *testing.T) {
	factoryCalls := 0
	p := &Plugin{
		config: Config{
			NameServerList: []string{"127.0.0.1:9876"},
			Topic:          "apisix-logs",
			UseTLS:         true,
		},
		senderFactory: func(config *Config) (rocketmqSender, error) {
			factoryCalls++
			if !config.UseTLS {
				t.Fatal("sender factory received use_tls=false, want true")
			}
			return &captureSender{}, nil
		},
		secretsPrepared: true,
	}
	p.SetDependencies(
		base.Dependencies{
			Tasks:          newLoggerTestTaskOwner(t),
			DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
		},
	)
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	t.Cleanup(p.Stop)

	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	if factoryCalls != 1 || p.sender == nil || p.BatchProcessor == nil || !p.ready {
		t.Fatalf(
			"TLS runtime state: factory_calls=%d sender=%T processor=%v ready=%v",
			factoryCalls,
			p.sender,
			p.BatchProcessor != nil,
			p.ready,
		)
	}
}

func TestPostInitRejectsInvalidBodyExpressions(t *testing.T) {
	tests := []struct {
		name   string
		config Config
		field  string
	}{
		{
			name: "request",
			config: Config{
				IncludeReqBody:     true,
				IncludeReqBodyExpr: [][]any{{"bar", "<>", "foo"}},
			},
			field: "include_req_body_expr",
		},
		{
			name: "response",
			config: Config{
				IncludeRespBody:     true,
				IncludeRespBodyExpr: [][]any{{"bar", "<!>", "foo"}},
			},
			field: "include_resp_body_expr",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.config.NameServerList = []string{"127.0.0.1:9876"}
			test.config.Topic = "apisix-logs"
			p := &Plugin{config: test.config, sender: &captureSender{}}
			p.SetDependencies(
				base.Dependencies{
					Tasks:          newLoggerTestTaskOwner(t),
					DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
				},
			)
			if err := p.Init(); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			if err := materializeRocketMQForTest(t, p, 1, "body-expr-"+test.name, p.config.SecretKey); err != nil {
				t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
			}
			err := p.PostInit()
			if err == nil {
				t.Fatalf("PostInit() error = nil, want invalid %s rejection", test.field)
			}
			if !strings.Contains(err.Error(), test.field) || !strings.Contains(err.Error(), "invalid operator") {
				t.Fatalf("PostInit() error = %q, want %s invalid operator error", err, test.field)
			}
		})
	}
}

func TestPostInitAcceptsOfficialNestedBodyExpressions(t *testing.T) {
	p := &Plugin{
		config: Config{
			NameServerList: []string{"127.0.0.1:9876"},
			Topic:          "apisix-logs",
			IncludeReqBody: true,
			IncludeReqBodyExpr: [][]any{
				{"request_length", "<", 1024},
				{"http_content_type", "in", []any{"application/json", "text/plain"}},
			},
			IncludeRespBody: true,
			IncludeRespBodyExpr: [][]any{
				{"http_content_length", "<", 1024},
				{"http_content_type", "in", []any{"application/json", "text/plain"}},
			},
		},
		sender: &captureSender{},
	}
	p.SetDependencies(
		base.Dependencies{
			Tasks:          newLoggerTestTaskOwner(t),
			DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
		},
	)
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := materializeRocketMQForTest(t, p, 1, "official-body-expr", p.config.SecretKey); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
}

func TestPostInitResolvesRotatedEncryptedSecretKey(t *testing.T) {
	oldKey := "old-keyring-item"
	newKey := "qeddd145sfvddff3"
	p := &Plugin{
		config: Config{
			NameServerList: []string{"127.0.0.1:9876"},
			Topic:          "apisix-logs",
			AccessKey:      "access",
			SecretKey:      encryptRocketMQLoggerTestValue(t, oldKey, "rocketmq-secret"),
		},
		sender: &captureSender{},
	}
	p.SetDependencies(base.Dependencies{
		Tasks:          newLoggerTestTaskOwner(t),
		DataEncryption: testutil.DataEncryptionService(true, []string{newKey, oldKey}).Resolver(),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := materializeRocketMQForTest(t, p, 1, "rotated-secret", "rocketmq-secret", newKey, oldKey); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	if p.config.SecretKey != rocketMQSecretDescriptor("rocketmq-secret") {
		t.Fatalf("secret_key = %q, want resolved descriptor", p.config.SecretKey)
	}
}

func TestSendBatchEncodesEntriesAsSingleRocketMQMessage(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		NameServerList: []string{"127.0.0.1:9876"},
		Topic:          "apisix-logs",
		Key:            "route-a",
		Tag:            "access",
		BatchMaxSize:   2,
	}, sender)

	firstFail, err := p.SendBatch(context.Background(), []map[string]any{{"route_id": "r1"}, {"route_id": "r2"}}, 2)
	if err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}
	if firstFail != 0 {
		t.Fatalf("firstFail = %d, want 0", firstFail)
	}

	messages := sender.waitForMessages(t, 1)
	message := messages[0]
	if message.Key != "route-a" {
		t.Fatalf("key = %q, want route-a", message.Key)
	}
	if message.Tag != "access" {
		t.Fatalf("tag = %q, want access", message.Tag)
	}

	var payload []map[string]any
	if err := json.Unmarshal(message.Body, &payload); err != nil {
		t.Fatalf("unmarshal rocketmq batch payload: %v", err)
	}
	if len(payload) != 2 {
		t.Fatalf("batch payload length = %d, want 2", len(payload))
	}
}

func TestHandlerSendsFormattedRequestLog(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		NameServerList: []string{"127.0.0.1:9876"},
		Topic:          "apisix-logs",
		LogFormat: map[string]string{
			"method": "$request_method",
			"path":   "$request_uri",
			"plugin": "rocketmq-logger",
		},
		BatchMaxSize: 1,
	}, sender)
	p.RouteID = "matched-route"

	req := httptest.NewRequest(http.MethodPatch, "http://example.com/orders/1?debug=true", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rr.Code)
	}

	message := sender.waitForMessage(t)
	var payload map[string]any
	if err := json.Unmarshal(message.Body, &payload); err != nil {
		t.Fatalf("unmarshal rocketmq payload: %v", err)
	}
	if payload["method"] != http.MethodPatch {
		t.Fatalf("method = %v, want PATCH", payload["method"])
	}
	if payload["path"] != "/orders/1?debug=true" {
		t.Fatalf("path = %v, want request URI", payload["path"])
	}
	if payload["plugin"] != "rocketmq-logger" {
		t.Fatalf("plugin = %v, want rocketmq-logger", payload["plugin"])
	}
	if payload["route_id"] != "matched-route" {
		t.Fatalf("route_id = %v, want matched-route", payload["route_id"])
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
	first := newTestPluginWithMetadata(t, Config{}, &captureSender{}, map[string]any{
		"log_format":          map[string]any{"generation": "n"},
		"max_pending_entries": 11,
	})
	second := newTestPluginWithMetadata(t, Config{}, &captureSender{}, map[string]any{
		"log_format":          map[string]any{"generation": "n-plus-one"},
		"max_pending_entries": 12,
	})
	route := newTestPluginWithMetadata(t, Config{
		LogFormat: map[string]string{"generation": "route"},
	}, &captureSender{}, map[string]any{
		"log_format":          map[string]any{"generation": "metadata"},
		"max_pending_entries": 13,
	})

	if first.LogFormat["generation"] != "n" || first.config.MaxPendingEntries != 11 {
		t.Fatalf("generation N metadata = %#v/%d", first.LogFormat, first.config.MaxPendingEntries)
	}
	if second.LogFormat["generation"] != "n-plus-one" || second.config.MaxPendingEntries != 12 {
		t.Fatalf("generation N+1 metadata = %#v/%d", second.LogFormat, second.config.MaxPendingEntries)
	}
	if route.LogFormat["generation"] != "route" || route.config.MaxPendingEntries != 13 {
		t.Fatalf("route precedence = %#v/%d", route.LogFormat, route.config.MaxPendingEntries)
	}
}

func TestMetadataDecodeFailsBeforeRocketMQSenderAndProcessorAcquisition(t *testing.T) {
	var factoryCalls int
	p := &Plugin{config: Config{
		NameServerList: []string{"127.0.0.1:9876"},
		Topic:          "apisix-logs",
	}}
	p.senderFactory = func(*Config) (rocketmqSender, error) {
		factoryCalls++
		return &captureSender{}, nil
	}
	p.SetDependencies(base.Dependencies{
		Tasks:          newLoggerTestTaskOwner(t),
		DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
		Metadata: mustMetadataView(t, map[string]any{
			"max_pending_entries": "invalid",
		}),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := materializeRocketMQForTest(t, p, 1, "invalid-metadata", p.config.SecretKey); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	err := p.PostInit()
	defer p.Stop()
	if err == nil {
		t.Fatal("PostInit() error = nil for invalid metadata")
	}
	if factoryCalls != 0 || p.sender != nil || p.BatchProcessor != nil {
		t.Fatalf(
			"decode failure acquired resources: factory_calls=%d sender=%v processor=%v",
			factoryCalls,
			p.sender,
			p.BatchProcessor,
		)
	}
}

func TestHandlerSendsDefaultAccessLogWhenNoFormatIsConfigured(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		NameServerList: []string{"127.0.0.1:9876"},
		Topic:          "apisix-logs",
		BatchMaxSize:   1,
	}, sender)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders?debug=true", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("accepted"))
	})).ServeHTTP(rr, req)

	message := sender.waitForMessage(t)
	var payload map[string]any
	if err := json.Unmarshal(message.Body, &payload); err != nil {
		t.Fatalf("unmarshal rocketmq payload: %v", err)
	}
	response, ok := payload["response"].(map[string]any)
	if !ok || response["status"] != float64(http.StatusAccepted) {
		t.Fatalf("payload response = %#v, want status 202", payload["response"])
	}
	request, ok := payload["request"].(map[string]any)
	if !ok || request["uri"] != "/orders?debug=true" {
		t.Fatalf("payload request = %#v, want request URI", payload["request"])
	}
}

func TestHandlerSendsOriginRequestLog(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		MetaFormat:      "origin",
		NameServerList:  []string{"127.0.0.1:9876"},
		Topic:           "apisix-logs",
		IncludeReqBody:  true,
		MaxReqBodyBytes: 32,
		BatchMaxSize:    1,
	}, sender)

	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/orders?debug=true",
		bytes.NewBufferString(`{"order":1}`),
	)
	req.Header.Set("X-Tenant", "gold")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream request body: %v", err)
		}
		if string(body) != `{"order":1}` {
			t.Fatalf("upstream body = %q, want original request body", body)
		}
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	message := sender.waitForMessage(t)
	payload := string(message.Body)
	if !strings.HasPrefix(payload, "POST /orders?debug=true HTTP/1.1\r\n") {
		t.Fatalf("origin payload = %q, want request line prefix", payload)
	}
	if !strings.Contains(payload, "X-Tenant: gold\r\n") {
		t.Fatalf("origin payload = %q, want request header", payload)
	}
	if !strings.HasSuffix(payload, "\r\n\r\n"+`{"order":1}`) {
		t.Fatalf("origin payload = %q, want request body after header block", payload)
	}
}

func TestHandlerIncludesRequestAndResponseBody(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		NameServerList:   []string{"127.0.0.1:9876"},
		Topic:            "apisix-logs",
		IncludeReqBody:   true,
		IncludeRespBody:  true,
		MaxReqBodyBytes:  32,
		MaxRespBodyBytes: 32,
		BatchMaxSize:     1,
	}, sender)

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

	message := sender.waitForMessage(t)
	var payload map[string]any
	if err := json.Unmarshal(message.Body, &payload); err != nil {
		t.Fatalf("unmarshal rocketmq payload: %v", err)
	}

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
}

func TestHandlerIncludesBodiesWhenExpressionsMatch(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		NameServerList:      []string{"127.0.0.1:9876"},
		Topic:               "apisix-logs",
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: [][]any{{"status", "==", "201"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
		BatchMaxSize:        1,
	}, sender)

	req := httptest.NewRequest(http.MethodPost, "http://example.com/orders", bytes.NewBufferString(`{"order":2}`))
	req.Header.Set("X-Log-Body", "yes")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	})).ServeHTTP(rr, req)

	message := sender.waitForMessage(t)
	var payload map[string]any
	if err := json.Unmarshal(message.Body, &payload); err != nil {
		t.Fatalf("unmarshal rocketmq payload: %v", err)
	}

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
}

func TestHandlerSkipsBodiesWhenExpressionsDoNotMatch(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		NameServerList:      []string{"127.0.0.1:9876"},
		Topic:               "apisix-logs",
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: [][]any{{"status", "==", "500"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
		BatchMaxSize:        1,
	}, sender)

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

	message := sender.waitForMessage(t)
	var payload map[string]any
	if err := json.Unmarshal(message.Body, &payload); err != nil {
		t.Fatalf("unmarshal rocketmq payload: %v", err)
	}
	request, ok := payload["request"].(map[string]any)
	if !ok {
		t.Fatalf("payload request = %#v, want default request object", payload["request"])
	}
	if _, ok := request["body"]; ok {
		t.Fatalf("payload request body = %#v, want omitted", request["body"])
	}
	response, ok := payload["response"].(map[string]any)
	if !ok {
		t.Fatalf("payload response = %#v, want default response object", payload["response"])
	}
	if _, ok := response["body"]; ok {
		t.Fatalf("payload response body = %#v, want omitted", response["body"])
	}
}

func TestHandlerLogsDecodedCompressedResponseBody(t *testing.T) {
	tests := []struct {
		name     string
		encoding string
		compress func(*testing.T, string) []byte
	}{
		{
			name:     "gzip",
			encoding: "gzip",
			compress: func(t *testing.T, body string) []byte {
				t.Helper()
				var compressed bytes.Buffer
				writer := gzip.NewWriter(&compressed)
				if _, err := writer.Write([]byte(body)); err != nil {
					t.Fatalf("gzip write: %v", err)
				}
				if err := writer.Close(); err != nil {
					t.Fatalf("gzip close: %v", err)
				}
				return compressed.Bytes()
			},
		},
		{
			name:     "brotli",
			encoding: "br",
			compress: func(t *testing.T, body string) []byte {
				t.Helper()
				var compressed bytes.Buffer
				writer := brotli.NewWriter(&compressed)
				if _, err := writer.Write([]byte(body)); err != nil {
					t.Fatalf("brotli write: %v", err)
				}
				if err := writer.Close(); err != nil {
					t.Fatalf("brotli close: %v", err)
				}
				return compressed.Bytes()
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sender := &captureSender{}
			p := newTestPlugin(t, Config{
				NameServerList:   []string{"127.0.0.1:9876"},
				Topic:            "apisix-logs",
				IncludeRespBody:  true,
				MaxRespBodyBytes: 1024,
				BatchMaxSize:     1,
			}, sender)
			t.Cleanup(func() { p.BatchProcessor.Stop() })

			const body = "compressed hello world\n"
			req := httptest.NewRequest(http.MethodGet, "http://example.com/compressed", nil)
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Encoding", test.encoding)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(test.compress(t, body))
			})).ServeHTTP(rr, req)

			message := sender.waitForMessage(t)
			var payload map[string]any
			if err := json.Unmarshal(message.Body, &payload); err != nil {
				t.Fatalf("unmarshal rocketmq payload: %v", err)
			}
			response, ok := payload["response"].(map[string]any)
			if !ok {
				t.Fatalf("payload response = %#v, want object", payload["response"])
			}
			if response["body"] != body {
				t.Fatalf("payload response body = %#v, want decoded %q", response["body"], body)
			}
		})
	}
}

func encryptRocketMQLoggerTestValue(t *testing.T, key string, value string) string {
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

// delayedUnblockProducer simulates a RocketMQ broker that honors context
// cancellation but unwinds slowly, so a wrapper goroutine that outlives the
// send is observable. Only SendSync is used by the sender under test.
type delayedUnblockProducer struct {
	rocketmq.Producer
}

func (*delayedUnblockProducer) SendSync(
	ctx context.Context,
	msgs ...*primitive.Message,
) (*primitive.SendResult, error) {
	<-ctx.Done()
	time.Sleep(300 * time.Millisecond)
	return nil, ctx.Err()
}

func TestSenderCancellationUnblocksSendWithoutLeakedGoroutine(t *testing.T) {
	sender := &rocketmqClientSender{producer: &delayedUnblockProducer{}}
	p := &Plugin{config: Config{Topic: "logs", Timeout: 1}, sender: sender}

	before := runtime.NumGoroutine()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := sender.Send(ctx, rocketmqMessage{Topic: "logs"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Send() error = nil, want context deadline exceeded")
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("Send took %v, want cancellation to unblock it", elapsed)
	}

	if _, batchErr := p.SendBatch(context.Background(), []map[string]any{{"route_id": "r1"}}, 1); batchErr == nil {
		t.Fatal("SendBatch() error = nil, want delivery failure after cancellation")
	}
	if got := runtime.NumGoroutine(); got > before {
		t.Fatalf("goroutines after SendBatch = %d, want <= %d", got, before)
	}
}
