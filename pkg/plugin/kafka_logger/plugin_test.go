package kafka_logger

import (
	"bytes"
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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	plain "github.com/segmentio/kafka-go/sasl/plain"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
)

type kafkaScopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type kafkaScopedSecretBroker struct {
	mu     sync.Mutex
	values map[string]string
	fail   map[string]error
	calls  []kafkaScopedSecretCall
}

func (broker *kafkaScopedSecretBroker) ResolveScoped(
	ctx context.Context, scope secret.Scope, raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, kafkaScopedSecretCall{Scope: scope, Raw: raw})
	if err := broker.fail[raw]; err != nil {
		return "", err
	}
	value, ok := broker.values[raw]
	if !ok {
		return "", errors.New("missing private Kafka password test value")
	}
	return value, nil
}

func (broker *kafkaScopedSecretBroker) callsSnapshot() []kafkaScopedSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]kafkaScopedSecretCall(nil), broker.calls...)
}

func newKafkaScopedSecretHarness(
	t *testing.T, config Config, values map[string]string, keyring ...string,
) (secret.GenerationSecrets, secret.Scope, *kafkaScopedSecretBroker, func()) {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: "kafka-scoped"}
	document, err := json.Marshal(map[string]any{
		"id": "kafka-scoped", "plugins": map[string]any{name: config},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := generation.NewSnapshot(90, []generation.Resource{{
		Key: key, Value: document,
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	candidate := generation.PublicationCandidate{
		Artifact: generation.GenerationArtifact{
			Domain: generation.DomainHTTP, Revision: 90,
			Digest: snapshot.Digest(), Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot,
		Closure:  []generation.ResourceKey{key},
		Decisions: []generation.ResourceDecision{{
			Key: key, Disposition: generation.DispositionPublished, Code: "kafka-scoped-test",
		}},
	}
	publication := generation.PublicationSet{
		DesiredRevision: 90,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: candidate,
		},
	}
	catalog, err := capability.NewSecretDeclarationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	broker := &kafkaScopedSecretBroker{values: values, fail: make(map[string]error)}
	materialization, err := testutil.NewSecretMaterializerWithKeyring(broker, catalog, keyring).
		PrepareGeneration(context.Background(), publication)
	if err != nil {
		t.Fatal(err)
	}
	secrets := materialization.Secrets()
	scope := secret.Scope{
		Generation: 90, Domain: generation.DomainHTTP,
		Plugin: name, Resource: key, Source: capability.SecretPluginConfig,
	}
	return secrets, scope, broker, func() {
		if err := materialization.Close(context.Background()); err != nil {
			t.Errorf("close Kafka scoped generation: %v", err)
		}
	}
}

func kafkaPasswordDescriptor(plaintext string) string {
	digest := sha256.Sum256([]byte(plaintext))
	return fmt.Sprintf("plugin_config#sha256:%s", hex.EncodeToString(digest[:]))
}

func TestMaterializeScopedSecretsOwnsEveryKafkaBrokerPassword(t *testing.T) {
	const (
		firstRaw  = "$ENV://KAFKA_FIRST_PASSWORD"
		secondRaw = "$secret://vault/kafka/second-password"
	)
	config := Config{
		Brokers: []Broker{
			{Host: "kafka-a", Port: 9092, SASLConfig: &SASLConfig{
				Mechanism: "PLAIN", User: "first-user", Password: firstRaw,
			}},
			{Host: "kafka-plain", Port: 9092},
			{Host: "kafka-b", Port: 9092, SASLConfig: &SASLConfig{
				Mechanism: "PLAIN", User: "second-user", Password: secondRaw,
			}},
		},
		KafkaTopic: "apisix-logs",
	}
	secrets, scope, broker, closeAttempt := newKafkaScopedSecretHarness(
		t, config, map[string]string{firstRaw: "first-private", secondRaw: "second-private"},
	)
	defer closeAttempt()
	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatal(err)
	}
	calls := broker.callsSnapshot()
	wantRaw := []string{firstRaw, secondRaw}
	if len(calls) != 2 {
		t.Fatalf("password materialization calls = %#v, want exactly two", calls)
	}
	for index, call := range calls {
		wantScope := scope
		wantScope.Field = "brokers.*.sasl_config.password"
		if call.Scope != wantScope || call.Raw != wantRaw[index] {
			t.Fatalf("call %d = %#v, want scope %#v raw %q", index, call, wantScope, wantRaw[index])
		}
	}
	if got := p.config.Brokers[0].SASLConfig.Password; got != kafkaPasswordDescriptor("first-private") {
		t.Fatalf("first public password = %q", got)
	}
	if got := p.config.Brokers[2].SASLConfig.Password; got != kafkaPasswordDescriptor("second-private") {
		t.Fatalf("second public password = %q", got)
	}
	if p.config.Brokers[1].SASLConfig != nil {
		t.Fatal("broker without SASL gained a private configuration")
	}
	if p.TaskOwner() == nil {
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	}
	if err := p.PostInit(); err == nil || !strings.Contains(err.Error(), "share one SASL identity") {
		t.Fatalf("PostInit() error = %v, want mixed non-secret SASL identity rejection", err)
	}
	if p.sender != nil || p.BatchProcessor != nil {
		t.Fatal("mixed SASL identities caused writer or batch side effects")
	}
}

func TestKafkaRejectsSharedSASLIdentityWithDifferentResolvedPasswords(t *testing.T) {
	const (
		firstRaw  = "$ENV://KAFKA_SHARED_FIRST"
		secondRaw = "$secret://vault/kafka/shared-second"
	)
	config := Config{
		Brokers: []Broker{
			{Host: "kafka-a", Port: 9092, SASLConfig: &SASLConfig{
				Mechanism: "PLAIN", User: "logger", Password: firstRaw,
			}},
			{Host: "kafka-b", Port: 9092, SASLConfig: &SASLConfig{
				Mechanism: "PLAIN", User: "logger", Password: secondRaw,
			}},
		},
		KafkaTopic: "apisix-logs",
	}
	secrets, scope, _, closeAttempt := newKafkaScopedSecretHarness(
		t, config, map[string]string{firstRaw: "first-private", secondRaw: "second-private"},
	)
	defer closeAttempt()
	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatal(err)
	}
	if p.TaskOwner() == nil {
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	}
	if err := p.PostInit(); err == nil || !strings.Contains(err.Error(), "share one SASL identity") {
		t.Fatalf("PostInit() error = %v, want resolved password identity rejection", err)
	}
	if p.sender != nil || p.BatchProcessor != nil {
		t.Fatal("different resolved SASL passwords caused writer or batch side effects")
	}
	p.Stop()
}

func TestKafkaAcceptsDifferentReferencesWithSameResolvedSASLPassword(t *testing.T) {
	const (
		firstRaw  = "$ENV://KAFKA_SHARED_EQUAL_FIRST"
		secondRaw = "$secret://vault/kafka/shared-equal-second"
		plaintext = "same-private-password"
	)
	config := Config{
		Brokers: []Broker{
			{Host: "kafka-a", Port: 9092, SASLConfig: &SASLConfig{
				Mechanism: "PLAIN", User: "logger", Password: firstRaw,
			}},
			{Host: "kafka-b", Port: 9092, SASLConfig: &SASLConfig{
				Mechanism: "PLAIN", User: "logger", Password: secondRaw,
			}},
		},
		KafkaTopic: "apisix-logs",
	}
	secrets, scope, _, closeAttempt := newKafkaScopedSecretHarness(
		t, config, map[string]string{firstRaw: plaintext, secondRaw: plaintext},
	)
	defer closeAttempt()
	p := &Plugin{config: config}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatal(err)
	}
	if p.config.Brokers[0].SASLConfig.Password != p.config.Brokers[1].SASLConfig.Password {
		t.Fatalf("equal resolved passwords produced different descriptors: %#v", p.config.Brokers)
	}
	if p.TaskOwner() == nil {
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v, want equal resolved password identity", err)
	}
	p.Stop()
}

func TestKafkaScopedPasswordFailureIsAtomicAndRetryable(t *testing.T) {
	const (
		firstRaw  = "$ENV://KAFKA_ATOMIC_FIRST"
		secondRaw = "$secret://vault/kafka/atomic-second"
	)
	config := Config{
		Brokers: []Broker{
			{Host: "kafka-a", Port: 9092, SASLConfig: &SASLConfig{
				Mechanism: "PLAIN", User: "logger", Password: firstRaw,
			}},
			{Host: "kafka-b", Port: 9092, SASLConfig: &SASLConfig{
				Mechanism: "PLAIN", User: "logger", Password: secondRaw,
			}},
		},
		KafkaTopic: "apisix-logs",
	}
	secrets, scope, broker, closeAttempt := newKafkaScopedSecretHarness(
		t, config, map[string]string{firstRaw: "first-private", secondRaw: "second-private"},
	)
	defer closeAttempt()
	broker.fail[secondRaw] = errors.New("resolver leaked " + secondRaw)
	p := &Plugin{config: config}
	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p)
	if !errors.Is(err, secret.ErrCredentialUnavailable) || strings.Contains(err.Error(), secondRaw) {
		t.Fatalf("first materialization error = %v, want redacted unavailable", err)
	}
	if p.config.Brokers[0].SASLConfig.Password != firstRaw ||
		p.config.Brokers[1].SASLConfig.Password != secondRaw ||
		p.secretsPrepared || len(p.saslPasswords) != 0 || len(p.saslBrokerIndexes) != 0 {
		t.Fatal("failed materialization installed partial public or private state")
	}
	broker.mu.Lock()
	delete(broker.fail, secondRaw)
	broker.calls = nil
	broker.mu.Unlock()
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatalf("same-instance retry error = %v", err)
	}
	if calls := broker.callsSnapshot(); len(calls) != 2 {
		t.Fatalf("retry calls = %#v, want both passwords", calls)
	}
	if p.config.Brokers[0].SASLConfig.Password != kafkaPasswordDescriptor("first-private") ||
		p.config.Brokers[1].SASLConfig.Password != kafkaPasswordDescriptor("second-private") {
		t.Fatalf("retry public brokers = %#v", p.config.Brokers)
	}
	p.Stop()
}

func TestKafkaConcurrentScopedPasswordMaterializationIsSingleFlight(t *testing.T) {
	const raw = "$ENV://KAFKA_SINGLEFLIGHT_PASSWORD"
	config := Config{
		Brokers: []Broker{{Host: "kafka-a", Port: 9092, SASLConfig: &SASLConfig{
			Mechanism: "PLAIN", User: "logger", Password: raw,
		}}},
		KafkaTopic: "apisix-logs",
	}
	secrets, scope, broker, closeAttempt := newKafkaScopedSecretHarness(
		t, config, map[string]string{raw: "singleflight-private"},
	)
	defer closeAttempt()
	p := &Plugin{config: config}
	start := make(chan struct{})
	errs := make(chan error, 16)
	var group sync.WaitGroup
	for range 16 {
		group.Go(func() {
			<-start
			errs <- base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p)
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
		t.Fatalf("singleflight calls = %#v, want one", calls)
	}
	p.Stop()
}

func TestKafkaWritersAndPasswordsAreAttemptOwned(t *testing.T) {
	const raw = "$ENV://KAFKA_GENERATION_PASSWORD"
	newConfig := func() Config {
		return Config{
			Brokers: []Broker{{Host: "127.0.0.1", Port: 9092, SASLConfig: &SASLConfig{
				Mechanism: "PLAIN", User: "logger", Password: raw,
			}}},
			KafkaTopic: "apisix-logs",
		}
	}
	newScoped := func(password string) (*Plugin, func()) {
		config := newConfig()
		secrets, scope, _, closeAttempt := newKafkaScopedSecretHarness(
			t, config, map[string]string{raw: password},
		)
		p := &Plugin{config: config}
		if err := p.Init(); err != nil {
			closeAttempt()
			t.Fatal(err)
		}
		if err := base.MaterializeScopedPluginSecrets(
			context.Background(), scope, secrets, p,
		); err != nil {
			closeAttempt()
			t.Fatal(err)
		}
		if p.TaskOwner() == nil {
			p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
		}
		if err := p.PostInit(); err != nil {
			closeAttempt()
			t.Fatal(err)
		}
		return p, func() {
			p.Stop()
			closeAttempt()
		}
	}
	first, closeFirst := newScoped("generation-first")
	second, closeSecond := newScoped("generation-second")
	defer closeSecond()
	firstSender := first.sender.(*kafkaGoSender)
	secondSender := second.sender.(*kafkaGoSender)
	if firstSender.writer == secondSender.writer {
		t.Fatal("two attempts shared a credential-bearing Kafka writer")
	}
	firstMechanism := firstSender.writer.Transport.(*kafka.Transport).SASL.(plain.Mechanism)
	secondMechanism := secondSender.writer.Transport.(*kafka.Transport).SASL.(plain.Mechanism)
	if firstMechanism.Password != "generation-first" || secondMechanism.Password != "generation-second" {
		t.Fatalf("attempt writer passwords = %q/%q", firstMechanism.Password, secondMechanism.Password)
	}
	if first.config.Brokers[0].SASLConfig.Password == "generation-first" ||
		second.config.Brokers[0].SASLConfig.Password == "generation-second" {
		t.Fatal("private password escaped into public broker config")
	}
	closeFirst()
	if second.sender != secondSender || second.stopped.Load() {
		t.Fatal("stopping first attempt changed the second writer")
	}
}

func TestKafkaPrivateBrokerCloneIsClearedAfterWriterConstruction(t *testing.T) {
	const raw = "$secret://vault/kafka/clone-password"
	config := Config{
		Brokers: []Broker{{Host: "127.0.0.1", Port: 9092, SASLConfig: &SASLConfig{
			Mechanism: "PLAIN", User: "logger", Password: raw,
		}}},
		KafkaTopic: "apisix-logs",
	}
	secrets, scope, _, closeAttempt := newKafkaScopedSecretHarness(
		t, config, map[string]string{raw: "clone-private"},
	)
	defer closeAttempt()
	p := &Plugin{config: config}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatal(err)
	}
	var retained []Broker
	if err := p.withPrivateBrokersLocked(func(brokers []Broker) error {
		if got := brokers[0].SASLConfig.Password; got != "clone-private" {
			t.Fatalf("private clone password = %q", got)
		}
		retained = brokers
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if retained[0] != (Broker{}) {
		t.Fatalf("retained private broker clone = %#v, want cleared", retained[0])
	}
	p.Stop()
}

type blockingKafkaSender struct {
	entered   chan struct{}
	release   chan struct{}
	enterOnce sync.Once
	mu        sync.Mutex
	closes    int
}

func (s *blockingKafkaSender) Send(context.Context, kafkaMessage) error {
	s.enterOnce.Do(func() { close(s.entered) })
	<-s.release
	return nil
}

func (s *blockingKafkaSender) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closes++
	return nil
}

func (s *blockingKafkaSender) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closes
}

func TestKafkaStopDrainsActiveSendAndPreventsResurrection(t *testing.T) {
	sender := &blockingKafkaSender{entered: make(chan struct{}), release: make(chan struct{})}
	p := &Plugin{
		config: Config{
			BrokerList: map[string]int{"127.0.0.1": 9092},
			KafkaTopic: "apisix-logs",
		},
		sender: sender,
	}
	p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	p.sender = sender
	secrets, scope, cleanup := testutil.ScopedSecretHarness(
		t,
		name,
		nil,
		generation.ApplyTicket{DesiredRevision: 1, RequiredDomains: []generation.Domain{generation.DomainHTTP}},
	)
	defer cleanup()
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}

	sendDone := make(chan error, 1)
	go func() {
		_, err := p.SendBatch(context.Background(), []map[string]any{{"route_id": "r1"}}, 1)
		sendDone <- err
	}()
	select {
	case <-sender.entered:
	case <-time.After(time.Second):
		t.Fatal("active Kafka send did not start")
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
		t.Fatal("Stop waited for active Kafka send instead of sealing scheduler admission")
	}
	if sender.closeCount() != 0 {
		t.Fatal("Kafka sender closed before the active send completed")
	}
	close(sender.release)
	if err := <-sendDone; err != nil {
		t.Fatalf("active SendBatch() error = %v", err)
	}
	if err := processor.Shutdown(context.Background()); err != nil {
		t.Fatalf("batch Shutdown() error = %v", err)
	}
	if sender.closeCount() != 1 {
		t.Fatalf("Kafka sender close count = %d, want 1", sender.closeCount())
	}
	if p.sender != nil || p.BatchProcessor != nil || p.secretsPrepared ||
		len(p.saslPasswords) != 0 || len(p.saslBrokerIndexes) != 0 {
		t.Fatal("Stop retained Kafka writer, processor, or private password owners")
	}
	queued := len(p.FireChan)
	if err := p.RunLogPhase(base.LogSnapshot{}); !errors.Is(err, base.ErrLogQueueUnavailable) {
		t.Fatalf("post-Stop RunLogPhase() error = %v", err)
	}
	if len(p.FireChan) != queued {
		t.Fatal("post-Stop RunLogPhase enqueued work")
	}
	if _, err := p.SendBatch(
		context.Background(),
		[]map[string]any{{"route_id": "late"}},
		1,
	); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("post-Stop SendBatch() error = %v", err)
	}
	if err := p.PostInit(); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("post-Stop PostInit() error = %v", err)
	}
	p.Stop()
	if sender.closeCount() != 1 {
		t.Fatalf("idempotent Stop close count = %d, want 1", sender.closeCount())
	}
}

func TestRunLogPhaseOriginPreservesHTTPFraming(t *testing.T) {
	delivered := make(chan map[string]any, 1)
	p := &Plugin{config: Config{MetaFormat: "origin", IncludeReqBody: true, MaxReqBodyBytes: 64}}
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
			Host: "gateway.test", Header: http.Header{"X-Test": {"one"}}, Body: []byte("payload"),
		},
		Outcome: apisixctx.ResponseOutcome{Status: http.StatusOK},
	}
	if err := p.RunLogPhase(snapshot); err != nil {
		t.Fatalf("RunLogPhase() error = %v", err)
	}
	select {
	case entry := <-delivered:
		origin, ok := entry[originLogKey].(string)
		if !ok ||
			!strings.Contains(origin, "POST /orders?x=1 HTTP/1.1\r\nHost: gateway.test\r\nX-Test: one\r\n\r\npayload") {
			t.Fatalf("origin payload = %q", origin)
		}
	case <-time.After(time.Second):
		t.Fatal("detached Kafka origin entry was not delivered")
	}
}

type captureSender struct {
	mu       sync.Mutex
	messages []kafkaMessage
}

type contextCaptureSender struct {
	ctx context.Context
}

func (s *contextCaptureSender) Send(ctx context.Context, _ kafkaMessage) error {
	s.ctx = ctx
	return ctx.Err()
}

func TestSendBatchPassesParentContextToKafka(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()

	sender := &contextCaptureSender{}
	p := &Plugin{config: Config{Timeout: 30}, sender: sender}
	_, err := p.SendBatch(parent, []map[string]any{{"route_id": "r1"}}, 1)
	if err == nil {
		t.Fatal("SendBatch() error = nil, want canceled parent error")
	}
	if sender.ctx == nil || sender.ctx.Err() == nil {
		t.Fatal("Kafka sender did not receive the canceled parent context")
	}
}

func TestSendBatchPreservesKafkaMarshalErrorContext(t *testing.T) {
	p := &Plugin{config: Config{Timeout: 1}, sender: &captureSender{}}
	_, err := p.SendBatch(context.Background(), []map[string]any{{"bad": make(chan int)}}, 1)
	if err == nil || !strings.Contains(err.Error(), "failed to marshal kafka log message") {
		t.Fatalf("SendBatch() error = %v, want kafka marshal context", err)
	}
}

type closeTrackingSender struct {
	closeCount int
}

func (s *closeTrackingSender) Send(context.Context, kafkaMessage) error {
	return nil
}

func (s *closeTrackingSender) Close() error {
	s.closeCount++
	return nil
}

func (s *captureSender) Send(ctx context.Context, message kafkaMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.messages = append(s.messages, message)
	return nil
}

func (s *captureSender) waitForMessage(t *testing.T) kafkaMessage {
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

	t.Fatal("timed out waiting for kafka message")
	return kafkaMessage{}
}

func (s *captureSender) waitForMessages(t *testing.T, count int) []kafkaMessage {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		if len(s.messages) >= count {
			messages := append([]kafkaMessage(nil), s.messages[:count]...)
			s.mu.Unlock()
			return messages
		}
		s.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %d kafka messages", count)
	return nil
}

func newTestPlugin(t *testing.T, cfg Config, sender kafkaSender) *Plugin {
	return newTestPluginWithMetadata(t, cfg, sender, runtime.MetadataView{})
}

func newTestPluginWithMetadata(
	t *testing.T, cfg Config, sender kafkaSender, metadata runtime.MetadataView,
) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg, sender: sender}
	p.SetDependencies(base.Dependencies{
		Tasks:    newLoggerTestTaskOwner(t),
		Metadata: metadata,
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.sender = sender
	values := make(map[string]string)
	for _, broker := range cfg.Brokers {
		if broker.SASLConfig != nil {
			values[broker.SASLConfig.Password] = broker.SASLConfig.Password
		}
	}
	secrets, scope, _, cleanup := newKafkaScopedSecretHarness(t, cfg, values)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
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

func TestPreparedGenerationsRetainMetadataFormat(t *testing.T) {
	nSource := []byte(`{"log_format":{"generation":"n"},"max_pending_entries":41}`)
	nView := mustMetadataView(t, map[string][]byte{name: nSource})
	clear(nSource)
	n := newTestPluginWithMetadata(t, Config{
		BrokerList: map[string]int{"127.0.0.1": 9092}, KafkaTopic: "n",
	}, &captureSender{}, nView)

	n1Source := []byte(`{"log_format":{"generation":"n1"},"max_pending_entries":42}`)
	n1View := mustMetadataView(t, map[string][]byte{name: n1Source})
	clear(n1Source)
	n1 := newTestPluginWithMetadata(t, Config{
		BrokerList: map[string]int{"127.0.0.1": 9092}, KafkaTopic: "n1",
	}, &captureSender{}, n1View)

	if got := n.LogFormat["generation"]; got != "n" || n.config.MaxPendingEntries != 41 {
		t.Fatalf("N metadata = format %q pending %d, want n/41", got, n.config.MaxPendingEntries)
	}
	if got := n1.LogFormat["generation"]; got != "n1" || n1.config.MaxPendingEntries != 42 {
		t.Fatalf("N+1 metadata = format %q pending %d, want n1/42", got, n1.config.MaxPendingEntries)
	}

	routePlugin := newTestPluginWithMetadata(t, Config{
		BrokerList: map[string]int{"127.0.0.1": 9092},
		KafkaTopic: "route",
		LogFormat:  map[string]string{"route": "$route_id"},
	}, &captureSender{}, n1View)
	if got := routePlugin.LogFormat["route"]; got != "$route_id" || len(routePlugin.LogFormat) != 1 {
		t.Fatalf("route format = %#v, want route precedence", routePlugin.LogFormat)
	}
}

func mustMetadataView(t *testing.T, documents map[string][]byte) runtime.MetadataView {
	t.Helper()
	view, err := runtime.NewMetadataView(documents)
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	return view
}

func TestPostInitRejectsInvalidMetadataBeforeSideEffects(t *testing.T) {
	p := &Plugin{config: Config{
		BrokerList: map[string]int{"127.0.0.1": 9092},
		KafkaTopic: "invalid-metadata",
	}}
	p.SetDependencies(base.Dependencies{
		Tasks: newLoggerTestTaskOwner(t),
		Metadata: mustMetadataView(t, map[string][]byte{
			name: []byte(`{"log_format":"sensitive-invalid-metadata"}`),
		}),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	secrets, scope, cleanup := testutil.ScopedSecretHarness(
		t,
		name,
		nil,
		generation.ApplyTicket{DesiredRevision: 1, RequiredDomains: []generation.Domain{generation.DomainHTTP}},
	)
	defer cleanup()
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	t.Cleanup(p.Stop)

	err := p.PostInit()
	if err == nil || !strings.Contains(err.Error(), "kafka-logger metadata decode failed") {
		t.Fatalf("PostInit() error = %v, want redacted metadata decode failure", err)
	}
	if strings.Contains(err.Error(), "sensitive-invalid-metadata") {
		t.Fatalf("PostInit() leaked metadata: %v", err)
	}
	if p.sender != nil || p.BatchProcessor != nil || p.config.ProducerType != "" {
		t.Fatalf(
			"PostInit() published side effects after invalid metadata: sender=%v batch=%v producer_type=%q",
			p.sender,
			p.BatchProcessor,
			p.config.ProducerType,
		)
	}
}

// APISIX 3.17 t/plugin/kafka-logger2.t TESTS 10-11 perform custom
// expression validation after JSON Schema validation.
func TestPostInitRejectsAPISIX317InvalidBodyExpressionOperators(t *testing.T) {
	tests := []struct {
		name       string
		field      string
		expression [][]any
	}{
		{name: "request body", field: "include_req_body_expr", expression: [][]any{{"bar", "<>", "foo"}}},
		{name: "response body", field: "include_resp_body_expr", expression: [][]any{{"bar", "<!>", "foo"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := Config{
				BrokerList: map[string]int{"127.0.0.1": 9092},
				KafkaTopic: "integration",
			}
			if test.field == "include_req_body_expr" {
				config.IncludeReqBody = true
				config.IncludeReqBodyExpr = test.expression
			} else {
				config.IncludeRespBody = true
				config.IncludeRespBodyExpr = test.expression
			}
			p := &Plugin{config: config, sender: &captureSender{}}
			p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
			if err := p.Init(); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			secrets, scope, cleanup := testutil.ScopedSecretHarness(
				t,
				name,
				nil,
				generation.ApplyTicket{DesiredRevision: 1, RequiredDomains: []generation.Domain{generation.DomainHTTP}},
			)
			defer cleanup()
			if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
				t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
			}
			t.Cleanup(p.Stop)

			err := p.PostInit()
			if err == nil || !strings.Contains(err.Error(), test.field) ||
				!strings.Contains(err.Error(), "invalid operator") {
				t.Fatalf("PostInit() error = %v, want %s invalid operator rejection", err, test.field)
			}
		})
	}
}

func TestMetadataSchemaAcceptsObjectLogFormatAndPendingLimit(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Validate(map[string]any{
		"log_format":          map[string]any{"route": "$route_id"},
		"max_pending_entries": 1,
	}, p.GetMetadataSchema()); err != nil {
		t.Fatalf("valid metadata rejected: %v", err)
	}
	for _, metadata := range []map[string]any{
		{"log_format": "wrong-type"},
		{"max_pending_entries": 0},
	} {
		if err := util.Validate(metadata, p.GetMetadataSchema()); err == nil {
			t.Fatalf("invalid metadata accepted: %#v", metadata)
		}
	}
}

func TestSendEncodesLogAndPublishesToConfiguredTopic(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		Brokers:    []Broker{{Host: "127.0.0.1", Port: 9092}},
		KafkaTopic: "apisix-logs",
		Key:        "route-a",
	}, sender)

	p.Send(map[string]any{
		"route_id": "r1",
		"status":   200,
	})

	message := sender.waitForMessage(t)
	if message.Topic != "apisix-logs" {
		t.Fatalf("topic = %q, want apisix-logs", message.Topic)
	}
	if string(message.Key) != "route-a" {
		t.Fatalf("key = %q, want route-a", string(message.Key))
	}

	var payload map[string]any
	if err := json.Unmarshal(message.Value, &payload); err != nil {
		t.Fatalf("unmarshal kafka payload: %v", err)
	}
	if payload["route_id"] != "r1" {
		t.Fatalf("route_id = %v, want r1", payload["route_id"])
	}
	if payload["status"].(float64) != 200 {
		t.Fatalf("status = %v, want 200", payload["status"])
	}
}

func TestNewWriterLeavesTopicForPerMessageRouting(t *testing.T) {
	p := &Plugin{config: Config{
		BrokerList: map[string]int{"127.0.0.1": 9092},
		KafkaTopic: "apisix-logs",
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	writer, err := p.newWriter(p.config.Brokers)
	if err != nil {
		t.Fatalf("newWriter() error = %v", err)
	}
	defer func() { _ = writer.Close() }()
	if writer.Topic != "" {
		t.Fatalf("writer topic = %q, want empty for per-message topic", writer.Topic)
	}
}

func TestStopClosesKafkaSenderAfterFlushingBatchProcessor(t *testing.T) {
	sender := &closeTrackingSender{}
	p := newTestPlugin(t, Config{
		BrokerList: map[string]int{"127.0.0.1": 9092},
		KafkaTopic: "apisix-logs",
	}, sender)

	processor := p.BatchProcessor
	p.Stop()
	p.Stop()
	if err := processor.Shutdown(context.Background()); err != nil {
		t.Fatalf("batch Shutdown() error = %v", err)
	}

	if sender.closeCount != 1 {
		t.Fatalf("Kafka sender close count = %d, want 1", sender.closeCount)
	}
}

func TestPostInitAcceptsDeprecatedBrokerListAndAppliesDefaults(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		BrokerList: map[string]int{"127.0.0.1": 9092},
		KafkaTopic: "apisix-logs",
	}, sender)

	got := p.brokerAddresses(p.config.Brokers)
	if len(got) != 1 || got[0] != "127.0.0.1:9092" {
		t.Fatalf("broker addresses = %v, want [127.0.0.1:9092]", got)
	}
	if p.config.ProducerType != "async" {
		t.Fatalf("producer_type = %q, want async", p.config.ProducerType)
	}
	if p.config.RequiredAcks != 1 {
		t.Fatalf("required_acks = %d, want 1", p.config.RequiredAcks)
	}
	if p.config.Timeout != 3 {
		t.Fatalf("timeout = %d, want 3", p.config.Timeout)
	}
	if p.config.BatchMaxSize != 1000 {
		t.Fatalf("batch_max_size = %d, want 1000", p.config.BatchMaxSize)
	}
}

func TestPostInitResolvesRotatedEncryptedSASLPassword(t *testing.T) {
	oldKey := "old-keyring-item"
	password := encryptKafkaLoggerTestValue(t, oldKey, "kafka-secret")
	config := Config{
		Brokers: []Broker{{
			Host:       "127.0.0.1",
			Port:       9092,
			SASLConfig: &SASLConfig{User: "logger", Password: password},
		}},
		KafkaTopic: "apisix-logs",
	}
	p := &Plugin{
		config: config,
		sender: &captureSender{},
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	secrets, scope, _, cleanup := newKafkaScopedSecretHarness(
		t, config, map[string]string{password: "kafka-secret"},
		oldKey,
	)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if p.TaskOwner() == nil {
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)
	if got := p.config.Brokers[0].SASLConfig.Password; got != kafkaPasswordDescriptor("kafka-secret") {
		t.Fatalf("SASL password = %q, want resolved descriptor", got)
	}
}

func TestPostInitRejectsMixedBrokerSASLIdentities(t *testing.T) {
	config := Config{
		Brokers: []Broker{
			{
				Host:       "127.0.0.1",
				Port:       9092,
				SASLConfig: &SASLConfig{Mechanism: "PLAIN", User: "logger", Password: "one"},
			},
			{
				Host:       "10.0.0.2",
				Port:       9092,
				SASLConfig: &SASLConfig{Mechanism: "PLAIN", User: "other", Password: "two"},
			},
		},
		KafkaTopic: "apisix-logs",
	}
	p := &Plugin{
		config: config,
		sender: &captureSender{},
	}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	secrets, scope, _, cleanup := newKafkaScopedSecretHarness(
		t, config, map[string]string{"one": "one", "two": "two"},
	)
	t.Cleanup(cleanup)
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if p.TaskOwner() == nil {
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	}
	if err := p.PostInit(); err == nil {
		t.Fatal("PostInit() error = nil, want mixed SASL identity rejection")
	}
}

func TestSendBatchEncodesEntriesAsSingleKafkaMessage(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		Brokers:      []Broker{{Host: "127.0.0.1", Port: 9092}},
		KafkaTopic:   "apisix-logs",
		Key:          "route-a",
		BatchMaxSize: 2,
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
	if string(message.Key) != "route-a" {
		t.Fatalf("key = %q, want route-a", string(message.Key))
	}

	var payload []map[string]any
	if err := json.Unmarshal(message.Value, &payload); err != nil {
		t.Fatalf("unmarshal kafka batch payload: %v", err)
	}
	if len(payload) != 2 {
		t.Fatalf("batch payload length = %d, want 2", len(payload))
	}
}

func TestHandlerSendsFormattedRequestLog(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		Brokers:    []Broker{{Host: "127.0.0.1", Port: 9092}},
		KafkaTopic: "apisix-logs",
		LogFormat: map[string]string{
			"method": "$request_method",
			"path":   "$request_uri",
			"plugin": "kafka-logger",
		},
		BatchMaxSize: 1,
	}, sender)

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
	if err := json.Unmarshal(message.Value, &payload); err != nil {
		t.Fatalf("unmarshal kafka payload: %v", err)
	}
	if payload["method"] != http.MethodPatch {
		t.Fatalf("method = %v, want PATCH", payload["method"])
	}
	if payload["path"] != "/orders/1?debug=true" {
		t.Fatalf("path = %v, want request URI", payload["path"])
	}
	if payload["plugin"] != "kafka-logger" {
		t.Fatalf("plugin = %v, want kafka-logger", payload["plugin"])
	}
}

func TestHandlerSendsDefaultAccessLogWhenNoFormatIsConfigured(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		Brokers:      []Broker{{Host: "127.0.0.1", Port: 9092}},
		KafkaTopic:   "apisix-logs",
		BatchMaxSize: 1,
	}, sender)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders?debug=true", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("accepted"))
	})).ServeHTTP(rr, req)

	message := sender.waitForMessage(t)
	var payload map[string]any
	if err := json.Unmarshal(message.Value, &payload); err != nil {
		t.Fatalf("unmarshal kafka payload: %v", err)
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
		Brokers:         []Broker{{Host: "127.0.0.1", Port: 9092}},
		KafkaTopic:      "apisix-logs",
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
	payload := string(message.Value)
	if !strings.HasPrefix(payload, "POST /orders?debug=true HTTP/1.1\r\n") {
		t.Fatalf("origin payload = %q, want request line prefix", payload)
	}
	if !strings.Contains(payload, "X-Tenant: gold\r\n") {
		t.Fatalf("origin payload = %q, want request header", payload)
	}
	if !strings.Contains(payload, "Host: example.com\r\n") {
		t.Fatalf("origin payload = %q, want request host", payload)
	}
	if !strings.HasSuffix(payload, "\r\n\r\n"+`{"order":1}`) {
		t.Fatalf("origin payload = %q, want request body after header block", payload)
	}
}

func TestHandlerBodyCaptureMatrix(t *testing.T) {
	tests := []struct {
		name, requestBody, responseBody, header string
		requestExpr, responseExpr               [][]any
		wantBodies                              bool
	}{
		{name: "unconditional", requestBody: `{"order":1}`, responseBody: `{"ok":true}`, wantBodies: true},
		{
			name:         "expressions match",
			requestBody:  `{"order":2}`,
			responseBody: `{"created":true}`,
			header:       "yes",
			requestExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
			responseExpr: [][]any{{"status", "==", "201"}},
			wantBodies:   true,
		},
		{
			name:         "expressions miss",
			requestBody:  `{"order":3}`,
			responseBody: `{"created":false}`,
			header:       "no",
			requestExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
			responseExpr: [][]any{{"status", "==", "500"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sender := &captureSender{}
			p := newTestPlugin(t, Config{
				Brokers: []Broker{{Host: "127.0.0.1", Port: 9092}}, KafkaTopic: "apisix-logs", BatchMaxSize: 1,
				IncludeReqBody: true, IncludeReqBodyExpr: test.requestExpr,
				IncludeRespBody: true, IncludeRespBodyExpr: test.responseExpr,
				MaxReqBodyBytes: 32, MaxRespBodyBytes: 32,
			}, sender)
			req := httptest.NewRequest(
				http.MethodPost,
				"http://example.com/orders",
				bytes.NewBufferString(test.requestBody),
			)
			if test.header != "" {
				req.Header.Set("X-Log-Body", test.header)
			}
			rr := httptest.NewRecorder()
			p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read upstream request body: %v", err)
				}
				if string(body) != test.requestBody {
					t.Fatalf("upstream body = %q, want %q", body, test.requestBody)
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(test.responseBody))
			})).ServeHTTP(rr, req)
			if rr.Code != http.StatusCreated || rr.Body.String() != test.responseBody {
				t.Fatalf(
					"response = (%d, %q), want (%d, %q)",
					rr.Code,
					rr.Body.String(),
					http.StatusCreated,
					test.responseBody,
				)
			}

			message := sender.waitForMessage(t)
			var payload map[string]any
			if err := json.Unmarshal(message.Value, &payload); err != nil {
				t.Fatalf("unmarshal kafka payload: %v", err)
			}
			request := payload["request"].(map[string]any)
			response := payload["response"].(map[string]any)
			if test.wantBodies {
				if request["body"] != test.requestBody || response["body"] != test.responseBody {
					t.Fatalf(
						"logged bodies = (%#v, %#v), want (%q, %q)",
						request["body"],
						response["body"],
						test.requestBody,
						test.responseBody,
					)
				}
			} else if _, requestOK := request["body"]; requestOK {
				t.Fatalf("request.body = %#v, want omitted", request["body"])
			} else if _, responseOK := response["body"]; responseOK {
				t.Fatalf("response.body = %#v, want omitted", response["body"])
			}
		})
	}
}

func TestSASLMechanismDefaultsToPlain(t *testing.T) {
	p := &Plugin{config: Config{
		Brokers: []Broker{{
			Host: "127.0.0.1",
			Port: 9092,
			SASLConfig: &SASLConfig{
				User:     "user",
				Password: "pass",
			},
		}},
		KafkaTopic: "apisix-logs",
	}}

	mechanism, err := p.saslMechanism(p.config.Brokers)
	if err != nil {
		t.Fatalf("saslMechanism() error = %v", err)
	}
	if mechanism == nil {
		t.Fatal("saslMechanism() returned nil")
	}
	if got := mechanism.Name(); got != "PLAIN" {
		t.Fatalf("SASL mechanism = %q, want PLAIN", got)
	}
}

func TestNewWriterUsesBrokerSASLConfig(t *testing.T) {
	p := &Plugin{config: Config{
		Brokers: []Broker{{
			Host: "127.0.0.1",
			Port: 9092,
			SASLConfig: &SASLConfig{
				Mechanism: "SCRAM-SHA-512",
				User:      "user",
				Password:  "pass",
			},
		}},
		KafkaTopic: "apisix-logs",
	}}
	p.applyDefaults()

	writer, err := p.newWriter(p.config.Brokers)
	if err != nil {
		t.Fatalf("newWriter() error = %v", err)
	}
	transport, ok := writer.Transport.(*kafka.Transport)
	if !ok || transport.SASL == nil {
		t.Fatal("writer does not have a SASL transport")
	}
	if got := transport.SASL.Name(); got != "SCRAM-SHA-512" {
		t.Fatalf("writer SASL mechanism = %q, want SCRAM-SHA-512", got)
	}
}

func TestSchemaEnforcesPositiveIntegerTimeout(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	config := map[string]any{
		"broker_list": map[string]any{"127.0.0.1": 9092},
		"kafka_topic": "integration",
		"timeout":     1,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("validate timeout 1: %v", err)
	}
	for _, invalid := range []any{0, -1, "1"} {
		config["timeout"] = invalid
		if err := util.Validate(config, p.GetSchema()); err == nil {
			t.Fatalf("validate timeout %#v = nil, want positive integer rejection", invalid)
		}
	}
}

func TestSchemaAcceptsAPIVersionTwoAndRejectsThree(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	base := map[string]any{
		"broker_list": map[string]any{"127.0.0.1": 9092},
		"kafka_topic": "integration",
	}
	base["api_version"] = 2
	if err := util.Validate(base, p.GetSchema()); err != nil {
		t.Fatalf("validate api_version 2: %v", err)
	}
	base["api_version"] = 3
	if err := util.Validate(base, p.GetSchema()); err == nil {
		t.Fatal("validate api_version 3 = nil, want enum rejection")
	}
}

// These are the direct schema checks from APISIX 3.17
// kafka-logger-large-body.t TEST 1, kafka-logger.t TESTS 2-3, and
// kafka-logger2.t TEST 1.
func TestSchemaRejectsAPISIX317InvalidRootConfigurations(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	tests := []struct {
		name   string
		config map[string]any
	}{
		{
			name: "string max request body bytes",
			config: map[string]any{
				"broker_list": map[string]any{"127.0.0.1": 9092},
				"kafka_topic": "integration", "max_req_body_bytes": "10",
			},
		},
		{
			name:   "missing broker source",
			config: map[string]any{"kafka_topic": "integration"},
		},
		{
			name: "string timeout",
			config: map[string]any{
				"broker_list": map[string]any{"127.0.0.1": 9092},
				"kafka_topic": "integration", "timeout": "10",
			},
		},
		{
			name: "unsupported required acknowledgements",
			config: map[string]any{
				"broker_list": map[string]any{"127.0.0.1": 9092},
				"kafka_topic": "integration", "required_acks": 2,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := util.Validate(test.config, p.GetSchema()); err == nil {
				t.Fatalf("Validate(%#v) error = nil, want APISIX 3.17 schema rejection", test.config)
			}
		})
	}
}

// APISIX 3.17 t/plugin/kafka-logger2.t TEST 5 checks these configurations
// through plugin.check_schema. Keep the field-level contract in this package;
// the data-plane integration only needs to prove that invalid configuration is
// never published.
func TestSchemaRejectsAPISIX317InvalidBrokerConfigurations(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	tests := []struct {
		name   string
		config map[string]any
	}{
		{name: "empty broker list", config: map[string]any{"broker_list": map[string]any{}}},
		{name: "string broker-list port", config: map[string]any{"broker_list": map[string]any{"127.0.0.1": "9092"}}},
		{name: "zero broker-list port", config: map[string]any{"broker_list": map[string]any{"127.0.0.1": 0}}},
		{name: "oversized broker-list port", config: map[string]any{"broker_list": map[string]any{"127.0.0.1": 65536}}},
		{name: "empty brokers", config: map[string]any{"brokers": []any{}}},
		{name: "broker without port", config: map[string]any{"brokers": []any{map[string]any{"host": "127.0.0.1"}}}},
		{name: "broker without host", config: map[string]any{"brokers": []any{map[string]any{"port": 9092}}}},
		{
			name:   "string broker port",
			config: map[string]any{"brokers": []any{map[string]any{"host": "127.0.0.1", "port": "9093"}}},
		},
		{
			name:   "zero broker port",
			config: map[string]any{"brokers": []any{map[string]any{"host": "127.0.0.1", "port": 0}}},
		},
		{
			name:   "oversized broker port",
			config: map[string]any{"brokers": []any{map[string]any{"host": "127.0.0.1", "port": 65536}}},
		},
		{
			name: "invalid SASL mechanism",
			config: map[string]any{
				"brokers": []any{
					map[string]any{
						"host": "127.0.0.1",
						"port": 9093,
						"sasl_config": map[string]any{
							"mechanism": "INVALID",
							"user":      "admin",
							"password":  "admin-secret",
						},
					},
				},
			},
		},
		{
			name: "SASL without password",
			config: map[string]any{
				"brokers": []any{
					map[string]any{"host": "127.0.0.1", "port": 9093, "sasl_config": map[string]any{"user": "admin"}},
				},
			},
		},
		{
			name: "SASL without user",
			config: map[string]any{
				"brokers": []any{
					map[string]any{
						"host":        "127.0.0.1",
						"port":        9093,
						"sasl_config": map[string]any{"password": "admin-secret"},
					},
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.config["kafka_topic"] = "integration"
			if err := util.Validate(test.config, p.GetSchema()); err == nil {
				t.Fatalf("Validate(%#v) error = nil, want APISIX 3.17 schema rejection", test.config)
			}
		})
	}
}

func encryptKafkaLoggerTestValue(t *testing.T, key string, value string) string {
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
