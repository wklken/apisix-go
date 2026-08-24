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

func (*kafkaScopedSecretBroker) AuthorizeCandidate(
	context.Context, secret.AttemptID, generation.ApplyTicket, generation.PublicationSet,
) error {
	return nil
}

func (*kafkaScopedSecretBroker) AuthorizeRecovery(
	context.Context, secret.AttemptID, generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return errors.New("recovery is not used by this Kafka logger fixture")
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

func (*kafkaScopedSecretBroker) RevokeAttempt(context.Context, secret.AttemptID) error {
	return nil
}

func (broker *kafkaScopedSecretBroker) callsSnapshot() []kafkaScopedSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]kafkaScopedSecretCall(nil), broker.calls...)
}

func newKafkaScopedSecretHarness(
	t *testing.T, config Config, values map[string]string,
) (secret.GenerationCapability, secret.Scope, *kafkaScopedSecretBroker, func()) {
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
	ticket := generation.ApplyTicket{
		DesiredRevision: 90, DesiredDigest: snapshot.Digest(),
		RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}
	publication := generation.PublicationSet{
		DesiredRevision: 90,
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
	broker := &kafkaScopedSecretBroker{values: values, fail: make(map[string]error)}
	registration, err := secret.NewScopedMaterializer(broker, catalog).RegisterCandidate(
		context.Background(), ticket, publication,
	)
	if err != nil {
		t.Fatal(err)
	}
	capabilityValue, err := secret.NewGenerationCapability(registration, 90)
	if err != nil {
		_ = registration.Close(context.Background())
		t.Fatal(err)
	}
	scope := secret.Scope{
		Generation: 90, Attempt: registration.AttemptID(), Domain: generation.DomainHTTP,
		Plugin: name, Resource: key, Source: capability.SecretPluginConfig,
	}
	return capabilityValue, scope, broker, func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Errorf("close Kafka scoped attempt: %v", err)
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
	capabilityValue, scope, broker, closeAttempt := newKafkaScopedSecretHarness(
		t, config, map[string]string{firstRaw: "first-private", secondRaw: "second-private"},
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
	capabilityValue, scope, _, closeAttempt := newKafkaScopedSecretHarness(
		t, config, map[string]string{firstRaw: "first-private", secondRaw: "second-private"},
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
	capabilityValue, scope, _, closeAttempt := newKafkaScopedSecretHarness(
		t, config, map[string]string{firstRaw: plaintext, secondRaw: plaintext},
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
	if p.config.Brokers[0].SASLConfig.Password != p.config.Brokers[1].SASLConfig.Password {
		t.Fatalf("equal resolved passwords produced different descriptors: %#v", p.config.Brokers)
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
	capabilityValue, scope, broker, closeAttempt := newKafkaScopedSecretHarness(
		t, config, map[string]string{firstRaw: "first-private", secondRaw: "second-private"},
	)
	defer closeAttempt()
	broker.fail[secondRaw] = errors.New("resolver leaked " + secondRaw)
	p := &Plugin{config: config}
	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
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
		context.Background(), scope, capabilityValue, p,
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
	capabilityValue, scope, broker, closeAttempt := newKafkaScopedSecretHarness(
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
			errs <- base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
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
		capabilityValue, scope, _, closeAttempt := newKafkaScopedSecretHarness(
			t, config, map[string]string{raw: password},
		)
		p := &Plugin{config: config}
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
	capabilityValue, scope, _, closeAttempt := newKafkaScopedSecretHarness(
		t, config, map[string]string{raw: "clone-private"},
	)
	defer closeAttempt()
	p := &Plugin{config: config}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
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
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	p.sender = sender
	if err := p.MaterializeSecrets(); err != nil {
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
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		t.Fatal("Stop returned before the active Kafka send drained")
	case <-time.After(25 * time.Millisecond):
	}
	close(sender.release)
	if err := <-sendDone; err != nil {
		t.Fatalf("active SendBatch() error = %v", err)
	}
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("Stop did not finish after the active Kafka send drained")
	}
	if sender.closeCount() != 1 {
		t.Fatalf("Kafka sender close count = %d, want 1", sender.closeCount())
	}
	if p.sender != nil || p.BatchProcessor != nil || p.secretsPrepared ||
		len(p.saslPasswords) != 0 || len(p.legacySASLPasswords) != 0 || len(p.saslBrokerIndexes) != 0 {
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
	); !errors.Is(
		err,
		secret.ErrCredentialUnavailable,
	) {
		t.Fatalf("post-Stop SendBatch() error = %v", err)
	}
	if err := p.MaterializeSecrets(); !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("post-Stop MaterializeSecrets() error = %v", err)
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
	p.BatchProcessor = logger_batch.NewWithContext(logger_batch.Config{
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
	t.Helper()

	p := &Plugin{config: cfg, sender: sender}
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	p.sender = sender
	if err := p.MaterializeSecrets(); err != nil {
		t.Fatalf("MaterializeSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	t.Cleanup(p.Stop)

	return p
}

func TestPostInitRejectsMissingDataEncryptionResolver(t *testing.T) {
	p := &Plugin{config: Config{Brokers: []Broker{{
		Host: "127.0.0.1", Port: 9092,
		SASLConfig: &SASLConfig{User: "logger", Password: "private"},
	}}}}
	if err := p.MaterializeSecrets(); err == nil || err.Error() != "data-encryption resolver is required" {
		t.Fatalf("MaterializeSecrets() error = %v, want missing resolver error", err)
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

	p.Stop()
	p.Stop()

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

func TestPostInitRejectsInvalidEncryptedSASLPassword(t *testing.T) {
	p := &Plugin{
		config: Config{
			Brokers: []Broker{{
				Host:       "127.0.0.1",
				Port:       9092,
				SASLConfig: &SASLConfig{User: "logger", Password: "not-a-ciphertext"},
			}},
			KafkaTopic: "apisix-logs",
		},
		sender: &captureSender{},
	}
	p.SetDependencies(base.Dependencies{
		DataEncryption: testutil.DataEncryptionService(true, []string{"qeddd145sfvddff3"}).Resolver(),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeSecrets(); err == nil {
		t.Fatal("MaterializeSecrets() error = nil, want strict encrypted SASL password rejection")
	}
}

func TestPostInitResolvesRotatedEncryptedSASLPassword(t *testing.T) {
	oldKey := "old-keyring-item"
	newKey := "qeddd145sfvddff3"
	password := encryptKafkaLoggerTestValue(t, oldKey, "kafka-secret")
	p := &Plugin{
		config: Config{
			Brokers: []Broker{{
				Host:       "127.0.0.1",
				Port:       9092,
				SASLConfig: &SASLConfig{User: "logger", Password: password},
			}},
			KafkaTopic: "apisix-logs",
		},
		sender: &captureSender{},
	}
	p.SetDependencies(base.Dependencies{
		DataEncryption: testutil.DataEncryptionService(true, []string{newKey, oldKey}).Resolver(),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeSecrets(); err != nil {
		t.Fatalf("MaterializeSecrets() error = %v", err)
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
	p := &Plugin{
		config: Config{
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
		},
		sender: &captureSender{},
	}
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.MaterializeSecrets(); err != nil {
		t.Fatalf("MaterializeSecrets() error = %v", err)
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
	if !strings.HasSuffix(payload, "\r\n\r\n"+`{"order":1}`) {
		t.Fatalf("origin payload = %q, want request body after header block", payload)
	}
}

func TestHandlerIncludesRequestAndResponseBody(t *testing.T) {
	sender := &captureSender{}
	p := newTestPlugin(t, Config{
		Brokers:          []Broker{{Host: "127.0.0.1", Port: 9092}},
		KafkaTopic:       "apisix-logs",
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
	if err := json.Unmarshal(message.Value, &payload); err != nil {
		t.Fatalf("unmarshal kafka payload: %v", err)
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
		Brokers:             []Broker{{Host: "127.0.0.1", Port: 9092}},
		KafkaTopic:          "apisix-logs",
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
	if err := json.Unmarshal(message.Value, &payload); err != nil {
		t.Fatalf("unmarshal kafka payload: %v", err)
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
		Brokers:             []Broker{{Host: "127.0.0.1", Port: 9092}},
		KafkaTopic:          "apisix-logs",
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
	if err := json.Unmarshal(message.Value, &payload); err != nil {
		t.Fatalf("unmarshal kafka payload: %v", err)
	}
	request, ok := payload["request"].(map[string]any)
	if !ok {
		t.Fatalf("payload request = %#v, want default request fields", payload["request"])
	}
	if _, ok := request["body"]; ok {
		t.Fatalf("payload request = %#v, want no request body", request)
	}
	response, ok := payload["response"].(map[string]any)
	if !ok {
		t.Fatalf("payload response = %#v, want default response fields", payload["response"])
	}
	if _, ok := response["body"]; ok {
		t.Fatalf("payload response = %#v, want no response body", response)
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
