package error_log_logger

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl/plain"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
)

type cancellationWatchConn struct {
	net.Conn
	closeCalls   atomic.Int32
	closeStarted chan struct{}
	releaseClose <-chan struct{}
}

func (c *cancellationWatchConn) Close() error {
	c.closeCalls.Add(1)
	select {
	case c.closeStarted <- struct{}{}:
	default:
	}
	if c.releaseClose != nil {
		<-c.releaseClose
	}
	return nil
}

func TestWatchConnectionCancellation(t *testing.T) {
	for _, scenario := range []string{
		"cancellation closes connection",
		"normal completion preserves reused connection",
		"cleanup joins close already running",
		"nil context is no-op",
	} {
		t.Run(scenario, func(t *testing.T) {
			var ctx context.Context
			var cancel context.CancelFunc
			if scenario != "nil context is no-op" {
				ctx, cancel = context.WithCancel(context.Background())
				defer cancel()
			}
			var releaseClose chan struct{}
			if scenario == "cleanup joins close already running" {
				releaseClose = make(chan struct{})
			}
			conn := &cancellationWatchConn{
				closeStarted: make(chan struct{}, 1),
				releaseClose: releaseClose,
			}
			cleanup := watchConnectionCancellation(ctx, conn)

			switch scenario {
			case "cancellation closes connection":
				cancel()
				cleanup()
			case "normal completion preserves reused connection":
				cleanup()
				cancel()
			case "cleanup joins close already running":
				cancel()
				select {
				case <-conn.closeStarted:
				case <-time.After(time.Second):
					t.Fatal("cancellation callback did not enter Close")
				}
				cleanupDone := make(chan struct{})
				go func() {
					cleanup()
					close(cleanupDone)
				}()
				select {
				case <-cleanupDone:
					t.Fatal("cleanup returned while Close was blocked")
				case <-time.After(20 * time.Millisecond):
				}
				close(releaseClose)
				select {
				case <-cleanupDone:
				case <-time.After(time.Second):
					t.Fatal("cleanup did not return after Close completed")
				}
			case "nil context is no-op":
				cleanup()
			}

			wantCloseCalls := int32(0)
			if scenario == "cancellation closes connection" ||
				scenario == "cleanup joins close already running" {
				wantCloseCalls = 1
			}
			if got := conn.closeCalls.Load(); got != wantCloseCalls {
				t.Fatalf("Close calls = %d, want %d", got, wantCloseCalls)
			}
		})
	}
}

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	p.SetDependencies(
		base.Dependencies{
			Tasks: newLoggerTestTaskOwner(t),
		},
	)
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	secrets, scope, closeAttempt := testutil.ScopedSecretHarness(
		t,
		name,
		nil,
		generation.ApplyTicket{DesiredRevision: 1, RequiredDomains: []generation.Domain{generation.DomainHTTP}},
	)
	t.Cleanup(closeAttempt)
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

func TestPostInitWarnsOnlyForInsecureTransports(t *testing.T) {
	tests := []struct {
		name         string
		tls          bool
		scheme       string
		wantWarnings []string
	}{
		{
			name:   "insecure",
			scheme: "http",
			wantWarnings: []string{
				"Using error-log-logger skywalking.endpoint_addr with no TLS is a security risk",
				"Using error-log-logger clickhouse.endpoint_addr with no TLS is a security risk",
				"Keeping tcp.tls disabled in error-log-logger configuration is a security risk",
			},
		},
		{name: "secure", tls: true, scheme: "https"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var warnings []string
			stop := logger.ReplaceObserver("error-log-logger-security-warning-"+test.name, func(entry logger.Entry) {
				if entry.Level == "WARN" && strings.Contains(entry.Message, "error-log-logger") {
					warnings = append(warnings, entry.Message)
				}
			})
			defer stop()

			newTestPlugin(t, Config{
				TCP: &TCPConfig{Host: "host.com", Port: 99, TLS: test.tls},
				Skywalking: &SkywalkingConfig{
					EndpointAddr: test.scheme + "://a.example",
				},
				Clickhouse: &ClickHouseConfig{
					EndpointAddr: test.scheme + "://clickhouse.example",
					User:         "user",
					Password:     "secret",
					Database:     "default",
					LogTable:     "logs",
				},
			})

			if !reflect.DeepEqual(warnings, test.wantWarnings) {
				t.Fatalf("warnings = %#v, want %#v", warnings, test.wantWarnings)
			}
		})
	}
}

func TestPostInitDoesNotWarnForPluginMetadataTransport(t *testing.T) {
	var warnings []string
	stop := logger.ReplaceObserver("error-log-logger-metadata-security-warning", func(entry logger.Entry) {
		if entry.Level == "WARN" && strings.Contains(entry.Message, "error-log-logger") {
			warnings = append(warnings, entry.Message)
		}
	})
	defer stop()

	newPreparedMetadataPlugin(t, Config{}, map[string]any{
		"tcp": map[string]any{"host": "host.com", "port": 99, "tls": false},
	})

	if len(warnings) != 0 {
		t.Fatalf("plugin metadata warnings = %#v, want none", warnings)
	}
}

func TestMetadataSchemaIsExplicitForErrorLogLogger(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if p.GetMetadataSchema() == "" {
		t.Fatal("error-log-logger metadata schema is empty")
	}
	if p.GetMetadataSchema() == p.GetSchema() {
		t.Fatal("metadata schema unexpectedly reused the route-level empty-object schema")
	}
	if err := util.Validate(map[string]any{"ignored": true}, p.GetSchema()); err != nil {
		t.Fatalf("route-level schema rejected an object APISIX ignores: %v", err)
	}
	if err := util.Validate(map[string]any{
		"tcp": map[string]any{"host": "127.0.0.1", "port": 19001},
	}, p.GetMetadataSchema()); err != nil {
		t.Fatalf("metadata schema rejected valid TCP document: %v", err)
	}
}

func TestPostInitWithoutMetadataReportsConfigurationAndStartsNoSender(t *testing.T) {
	var messages []string
	stop := logger.ReplaceObserver("error-log-logger-missing-metadata", func(entry logger.Entry) {
		if strings.Contains(entry.Message, "plugin_metadata for error-log-logger") {
			messages = append(messages, entry.Message)
		}
	})
	defer stop()

	p := newTestPlugin(t, Config{})
	if !reflect.DeepEqual(messages, []string{"please set the correct plugin_metadata for error-log-logger"}) {
		t.Fatalf("missing metadata messages = %#v", messages)
	}
	if p.BatchProcessor != nil || p.client != nil || p.kafkaSender != nil {
		t.Fatalf("missing metadata started sender resources: %#v/%#v/%#v", p.BatchProcessor, p.client, p.kafkaSender)
	}
}

func TestPostInitUsesPreparedErrorLogMetadata(t *testing.T) {
	p := newPreparedMetadataPlugin(t, Config{}, map[string]any{
		"tcp":   map[string]any{"host": "127.0.0.1", "port": 19001},
		"level": "INFO",
	})

	if p.config.TCP == nil || p.config.TCP.Host != "127.0.0.1" || p.config.TCP.Port != 19001 {
		t.Fatalf("prepared metadata TCP config = %#v, want metadata-selected sink", p.config.TCP)
	}
	if p.config.Level != "INFO" {
		t.Fatalf("prepared metadata level = %q, want INFO", p.config.Level)
	}
}

func TestMetadataSecurityWarningIsDeliveredOnceAfterObserverAdmission(t *testing.T) {
	received := make(chan string, 2)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(sink.Close)

	p := newPreparedMetadataPlugin(t, Config{}, map[string]any{
		"clickhouse": map[string]any{
			"endpoint_addr": sink.URL,
			"user":          "default",
			"password":      "secret",
			"database":      "default",
			"logtable":      "logs",
		},
		"level": "WARN", "batch_max_size": 1, "max_retry_count": 0,
	})
	select {
	case payload := <-received:
		t.Fatalf("security warning delivered before observer admission: %q", payload)
	case <-time.After(100 * time.Millisecond):
	}

	registry := runtime.NewTaskRegistry(context.Background(), nil)
	owner := newObserverTaskOwner(t, registry)
	if err := p.StartObservingWithTasks(owner); err != nil {
		t.Fatalf("StartObservingWithTasks() error = %v", err)
	}
	select {
	case payload := <-received:
		const warning = "Using error-log-logger clickhouse.endpoint_addr with no TLS is a security risk"
		if !strings.Contains(payload, `"data":"`) || !strings.Contains(payload, "[warn] "+warning) {
			t.Fatalf("startup security warning payload = %q, want exact warning", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for startup security warning delivery")
	}

	p.StartObserving()
	select {
	case payload := <-received:
		t.Fatalf("observer re-entry repeated startup security warning: %q", payload)
	case <-time.After(150 * time.Millisecond):
	}
	stopTaskRegistry(t, registry)
}

func TestRejectedObserverAdmissionDoesNotDeliverMetadataSecurityWarning(t *testing.T) {
	received := make(chan string, 1)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- string(body)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(sink.Close)

	p := newPreparedMetadataPlugin(t, Config{}, map[string]any{
		"clickhouse": map[string]any{
			"endpoint_addr": sink.URL,
			"user":          "default",
			"password":      "secret",
			"database":      "default",
			"logtable":      "logs",
		},
		"level": "WARN", "batch_max_size": 1, "max_retry_count": 0,
	})
	registry := runtime.NewTaskRegistry(context.Background(), nil)
	owner := newObserverTaskOwner(t, registry)
	stopTaskRegistry(t, registry)
	if err := p.StartObservingWithTasks(owner); !errors.Is(err, errObserverTaskRegistration) {
		t.Fatalf("StartObservingWithTasks(stopped) error = %v, want %v", err, errObserverTaskRegistration)
	}
	select {
	case payload := <-received:
		t.Fatalf("rejected observer admission delivered startup security warning: %q", payload)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestRouteErrorLogConfigOverridesPreparedMetadata(t *testing.T) {
	p := newPreparedMetadataPlugin(t, Config{
		TCP:   &TCPConfig{Host: "127.0.0.1", Port: 19002},
		Level: "ERROR",
	}, map[string]any{
		"tcp":   map[string]any{"host": "127.0.0.1", "port": 19001},
		"level": "INFO",
	})

	if p.config.TCP == nil || p.config.TCP.Host != "127.0.0.1" || p.config.TCP.Port != 19002 {
		t.Fatalf("route config TCP = %#v, want route-selected sink", p.config.TCP)
	}
	if p.config.Level != "ERROR" {
		t.Fatalf("route config level = %q, want ERROR", p.config.Level)
	}
}

func TestPreparedGenerationsRetainErrorLogMetadata(t *testing.T) {
	first := newPreparedMetadataPlugin(t, Config{}, map[string]any{
		"tcp":   map[string]any{"host": "127.0.0.1", "port": 19011},
		"level": "WARN",
	})
	second := newPreparedMetadataPlugin(t, Config{}, map[string]any{
		"tcp":   map[string]any{"host": "127.0.0.1", "port": 19012},
		"level": "DEBUG",
	})

	if got := first.config.TCP; got == nil || got.Port != 19011 || first.config.Level != "WARN" {
		t.Fatalf("generation N config = %#v/%q, want 19011/WARN", got, first.config.Level)
	}
	if got := second.config.TCP; got == nil || got.Port != 19012 || second.config.Level != "DEBUG" {
		t.Fatalf("generation N+1 config = %#v/%q, want 19012/DEBUG", got, second.config.Level)
	}
}

func TestPreparedErrorLogMetadataSecretsArePrivateAndRedacted(t *testing.T) {
	clickhouseEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-ClickHouse-Key"); got != "metadata-clickhouse-secret" {
			t.Errorf("ClickHouse key = %q, want resolved metadata secret", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(clickhouseEndpoint.Close)

	p := newPreparedMetadataPlugin(t, Config{}, map[string]any{
		"clickhouse": map[string]any{
			"endpoint_addr": clickhouseEndpoint.URL,
			"user":          "default",
			"password":      "metadata-clickhouse-secret",
			"database":      "logs",
			"logtable":      "error_logs",
		},
		"kafka": map[string]any{
			"brokers": []any{
				map[string]any{
					"host": "broker-a",
					"port": 9092,
					"sasl_config": map[string]any{
						"user":     "user-a",
						"password": "metadata-kafka-secret-a",
					},
				},
				map[string]any{
					"host": "broker-b",
					"port": 9093,
					"sasl_config": map[string]any{
						"user":     "user-b",
						"password": "metadata-kafka-secret-b",
					},
				},
			},
			"kafka_topic": "apisix-error-logs",
		},
		"level": "INFO",
	})

	if p.config.Clickhouse.Password == "metadata-clickhouse-secret" ||
		!strings.Contains(p.config.Clickhouse.Password, "plugin_metadata#sha256:") {
		t.Fatalf("public ClickHouse password = %q, want metadata descriptor", p.config.Clickhouse.Password)
	}
	if len(p.metadataKafkaPasswords) != 2 {
		t.Fatalf("metadata Kafka private values = %d, want two broker values", len(p.metadataKafkaPasswords))
	}
	if p.config.Kafka.Brokers[0].SASLConfig.Password == "metadata-kafka-secret-a" ||
		p.config.Kafka.Brokers[1].SASLConfig.Password == "metadata-kafka-secret-b" {
		t.Fatalf("public Kafka config retained metadata plaintext: %#v", p.config.Kafka.Brokers)
	}

	if err := p.SendLogs(context.Background(), []string{`2026/08/24 [error] metadata secret`}); err != nil {
		t.Fatalf("SendLogs() error = %v", err)
	}

	writerSender, ok := p.kafkaSender.(*kafkaGoSender)
	if !ok {
		t.Fatalf("kafka sender type = %T, want kafkaGoSender", p.kafkaSender)
	}
	transport, ok := writerSender.writer.Transport.(*kafka.Transport)
	if !ok || transport.SASL == nil {
		t.Fatal("metadata Kafka writer has no SASL transport")
	}
	mechanism, ok := transport.SASL.(plain.Mechanism)
	if !ok || mechanism.Password != "metadata-kafka-secret-a" {
		t.Fatalf(
			"metadata Kafka mechanism = %#v/%T, want first resolved metadata password",
			transport.SASL,
			transport.SASL,
		)
	}

	// The metadata path must not need the S2 plugin-config resolver.
	if p.metadataSelected && p.secretsMaterialized {
		t.Fatal("metadata-selected instance invoked plugin-config secret materialization")
	}
	processor := p.BatchProcessor
	p.Stop()
	if err := processor.Shutdown(context.Background()); err != nil {
		t.Fatalf("batch Shutdown() error = %v", err)
	}
	if p.metadataClickhousePassword != nil || len(p.metadataKafkaPasswords) != 0 {
		t.Fatal("Stop() retained metadata private secret references")
	}
}

func TestStartObservingWithTasksUsesExactOwnerPrefix(t *testing.T) {
	p := newObserverTestPlugin(t, &fakeKafkaSender{})
	registry := runtime.NewTaskRegistry(context.Background(), nil)
	owner := newObserverTaskOwner(t, registry)
	if err := p.StartObservingWithTasks(owner); err != nil {
		t.Fatal(err)
	}
	want := []string{"plugin/error-log-logger/" + strings.Repeat("a", 64) + "/observer"}
	if got := registry.Active(); !reflect.DeepEqual(got, want) {
		t.Fatalf("active observers = %v, want %v", got, want)
	}
	stopTaskRegistry(t, registry)
}

func TestErrorLogObserverDelegatesBlockingBatchShutdownWithoutRemainingResidual(t *testing.T) {
	registry := runtime.NewTaskRegistry(context.Background(), nil)
	ownerPrefix := "plugin/error-log-logger/" + strings.Repeat("a", 64)
	owner, err := runtime.NewTaskOwner(registry, ownerPrefix, runtime.TaskPlugin)
	if err != nil {
		t.Fatal(err)
	}
	deliveryStarted := make(chan struct{})
	releaseDelivery := make(chan struct{})
	processor, err := logger_batch.NewWithContext(logger_batch.Config{
		Tasks:                   owner,
		BatchMaxSize:            1,
		MaxConcurrentDeliveries: 1,
	}, func(context.Context, []map[string]any, int) (int, error) {
		close(deliveryStarted)
		<-releaseDelivery
		return 0, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sender := &fakeKafkaSender{}
	p := &Plugin{
		config:         Config{Name: "error log logger", Level: "WARN"},
		BatchProcessor: processor,
		kafkaSender:    sender,
	}
	if err := p.StartObservingWithTasks(owner); err != nil {
		t.Fatal(err)
	}
	logger.Warn("owned error-log blocking batch marker")
	select {
	case <-deliveryStarted:
	case <-time.After(time.Second):
		t.Fatal("delivery did not start")
	}

	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("Plugin.Stop() waited for blocked batch delivery")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	residuals, err := registry.Stop(ctx)
	var residualErr *runtime.TaskResidualError
	if !errors.As(err, &residualErr) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("TaskRegistry.Stop() residuals=%v error=%v, want deadline residual", residuals, err)
	}
	want := []runtime.TaskResidual{
		{Owner: ownerPrefix + "/batch-shutdown"},
		{Owner: ownerPrefix + "/batch-worker"},
	}
	if !reflect.DeepEqual(residuals, want) || !reflect.DeepEqual(residualErr.Residuals(), want) {
		t.Fatalf("residuals returned=%v resident=%v, want %v", residuals, residualErr.Residuals(), want)
	}
	if got := sender.closeCallsCount(); got != 0 {
		t.Fatalf("cleanup count while delivery blocked = %d, want 0", got)
	}
	close(releaseDelivery)
	stopTaskRegistry(t, registry)
	if got := sender.closeCallsCount(); got != 1 {
		t.Fatalf("cleanup count = %d, want 1", got)
	}
}

func TestStartObservingWithTasksRejectsMissingOrStoppedOwner(t *testing.T) {
	p := newObserverTestPlugin(t, &fakeKafkaSender{})
	if err := p.StartObservingWithTasks(nil); !errors.Is(err, errObserverTaskOwnerRequired) {
		t.Fatalf("StartObservingWithTasks(nil) error = %v, want %v", err, errObserverTaskOwnerRequired)
	}
	if p.observerStop != nil {
		t.Fatal("nil owner installed an observer")
	}

	registry := runtime.NewTaskRegistry(context.Background(), nil)
	owner := newObserverTaskOwner(t, registry)
	if _, err := registry.Stop(context.Background()); err != nil {
		t.Fatalf("registry.Stop() error = %v", err)
	}
	if err := p.StartObservingWithTasks(owner); !errors.Is(err, errObserverTaskRegistration) {
		t.Fatalf("StartObservingWithTasks(stopped) error = %v, want %v", err, errObserverTaskRegistration)
	}
	if p.observerStop != nil {
		t.Fatal("stopped registry installed an observer")
	}
}

func TestRejectedObserverAdmissionPreservesCurrentGeneration(t *testing.T) {
	currentSender := &fakeKafkaSender{}
	current := newObserverTestPlugin(t, currentSender)
	currentTasks := runtime.NewTaskRegistry(context.Background(), nil)
	currentOwner := newObserverTaskOwner(t, currentTasks)
	if err := current.StartObservingWithTasks(currentOwner); err != nil {
		t.Fatalf("start current observer: %v", err)
	}

	stoppedTasks := runtime.NewTaskRegistry(context.Background(), nil)
	stoppedOwner := newObserverTaskOwner(t, stoppedTasks)
	stopTaskRegistry(t, stoppedTasks)
	rejected := newObserverTestPlugin(t, &fakeKafkaSender{})
	if err := rejected.StartObservingWithTasks(stoppedOwner); err == nil {
		t.Fatal("rejected observer admission error = nil")
	}

	logger.Warn("current generation survives rejected observer admission")
	waitFor(t, func() bool { return currentSender.messagesCount() == 1 }, "current generation delivery")
	stopTaskRegistry(t, currentTasks)
}

func TestObserverAdmissionStopRaceHasNoLeakOrCurrentLoss(t *testing.T) {
	for iteration := range 32 {
		currentSender := &fakeKafkaSender{delivered: make(chan struct{}, 1)}
		current := newObserverTestPlugin(t, currentSender)
		currentTasks := runtime.NewTaskRegistry(context.Background(), nil)
		currentOwner := newObserverTaskOwner(t, currentTasks)
		if err := current.StartObservingWithTasks(currentOwner); err != nil {
			t.Fatalf("iteration %d: start current observer: %v", iteration, err)
		}

		candidate := newObserverTestPlugin(t, &fakeKafkaSender{})
		candidateTasks := runtime.NewTaskRegistry(context.Background(), nil)
		candidateOwner := newObserverTaskOwner(t, candidateTasks)
		startResult := make(chan error, 1)
		go func() {
			startResult <- candidate.StartObservingWithTasks(candidateOwner)
		}()
		stopResult := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err := candidateTasks.Stop(ctx)
			stopResult <- err
		}()

		startErr := <-startResult
		if err := <-stopResult; err != nil {
			t.Fatalf("iteration %d: concurrent registry Stop() error = %v", iteration, err)
		}
		if candidate.observerStop != nil {
			t.Fatalf("iteration %d: candidate retained observer stop after registry retirement", iteration)
		}
		candidate.Stop()

		if startErr != nil {
			logger.Warn(fmt.Sprintf("current generation survives admission race %d", iteration))
			select {
			case <-currentSender.delivered:
			case <-time.After(2 * time.Second):
				t.Fatalf("iteration %d: current observer lost after rejected admission: %v", iteration, startErr)
			}
		}
		stopTaskRegistry(t, currentTasks)
	}
}

func TestStartObservingWithTasksRejectsStoppedPlugin(t *testing.T) {
	p := newObserverTestPlugin(t, &fakeKafkaSender{})
	p.Stop()

	registry := runtime.NewTaskRegistry(context.Background(), nil)
	owner := newObserverTaskOwner(t, registry)
	if err := p.StartObservingWithTasks(owner); !errors.Is(err, errObserverLifecycleStopped) {
		t.Fatalf("StartObservingWithTasks() error = %v, want stopped lifecycle", err)
	}
	p.observerLifecycleMu.Lock()
	observerStop := p.observerStop
	stopped := p.observerStopped
	p.observerLifecycleMu.Unlock()
	if observerStop != nil || !stopped {
		t.Fatalf("stopped observer state = stop:%v stopped:%v", observerStop != nil, stopped)
	}
	stopTaskRegistry(t, registry)
}

func TestExternalStopRacesObserverAdmissionWithoutLeak(t *testing.T) {
	for iteration := range 64 {
		p := newObserverTestPlugin(t, &fakeKafkaSender{})
		registry := runtime.NewTaskRegistry(context.Background(), nil)
		owner := newObserverTaskOwner(t, registry)
		startResult := make(chan error, 1)
		stopDone := make(chan struct{})
		go func() { startResult <- p.StartObservingWithTasks(owner) }()
		go func() {
			p.Stop()
			close(stopDone)
		}()

		startErr := <-startResult
		<-stopDone
		if startErr != nil && !errors.Is(startErr, errObserverLifecycleStopped) {
			t.Fatalf("iteration %d: start error = %v", iteration, startErr)
		}
		p.observerLifecycleMu.Lock()
		observerStop := p.observerStop
		stopped := p.observerStopped
		p.observerLifecycleMu.Unlock()
		if observerStop != nil || !stopped {
			t.Fatalf(
				"iteration %d: observer state = stop:%v stopped:%v",
				iteration,
				observerStop != nil,
				stopped,
			)
		}
		stopTaskRegistry(t, registry)
	}
}

func TestPreparedGenerationsReplaceErrorLogObserverSafely(t *testing.T) {
	firstSender := &fakeKafkaSender{}
	first := newObserverTestPlugin(t, firstSender)
	firstTasks := runtime.NewTaskRegistry(context.Background(), nil)
	firstOwner := newObserverTaskOwner(t, firstTasks)
	if err := first.StartObservingWithTasks(firstOwner); err != nil {
		t.Fatalf("start first observer: %v", err)
	}

	secondSender := &fakeKafkaSender{}
	second := newObserverTestPlugin(t, secondSender)
	secondTasks := runtime.NewTaskRegistry(context.Background(), nil)
	secondOwner := newObserverTaskOwner(t, secondTasks)
	if err := second.StartObservingWithTasks(secondOwner); err != nil {
		t.Fatalf("start second observer: %v", err)
	}

	first.Stop()
	logger.Warn("prepared error-log generation marker")
	waitFor(t, func() bool { return secondSender.messagesCount() == 1 }, "replacement observer delivery")
	if got := firstSender.messagesCount(); got != 0 {
		t.Fatalf("retired generation deliveries = %d, want zero", got)
	}

	stopTaskRegistry(t, firstTasks)
	stopTaskRegistry(t, secondTasks)
}

func TestTaskStopUnregistersObserverAndClosesResourcesOnce(t *testing.T) {
	sender := &fakeKafkaSender{}
	p := newObserverTestPlugin(t, sender)
	registry := runtime.NewTaskRegistry(context.Background(), nil)
	owner := newObserverTaskOwner(t, registry)
	if err := p.StartObservingWithTasks(owner); err != nil {
		t.Fatalf("StartObservingWithTasks() error = %v", err)
	}

	logger.Warn("error-log task stop marker")
	waitFor(t, func() bool { return sender.messagesCount() == 1 }, "task observer delivery")
	processor := p.BatchProcessor
	stopTaskRegistry(t, registry)
	if err := processor.Shutdown(context.Background()); err != nil {
		t.Fatalf("batch Shutdown() error = %v", err)
	}
	if got := sender.closeCallsCount(); got != 1 {
		t.Fatalf("Kafka close calls = %d, want one after task stop", got)
	}

	logger.Warn("after error-log task stop")
	time.Sleep(100 * time.Millisecond)
	if got := sender.messagesCount(); got != 1 {
		t.Fatalf("post-stop observer deliveries = %d, want one", got)
	}
	p.Stop()
	if got := sender.closeCallsCount(); got != 1 {
		t.Fatalf("Kafka close calls after idempotent Stop = %d, want one", got)
	}
}

func TestPreparationFailureClosesErrorLogResourcesInReverseOrder(t *testing.T) {
	sender := &fakeKafkaSender{blockSend: make(chan struct{})}
	p := newObserverTestPlugin(t, sender)
	registry := runtime.NewTaskRegistry(context.Background(), nil)
	owner := newObserverTaskOwner(t, registry)
	if err := p.StartObservingWithTasks(owner); err != nil {
		t.Fatalf("StartObservingWithTasks() error = %v", err)
	}

	logger.Warn("error-log in-flight marker")
	waitFor(t, func() bool { return sender.messagesCount() == 1 }, "in-flight send")
	processor := p.BatchProcessor

	stopped := make(chan struct{})
	go func() {
		p.Stop()
		close(stopped)
	}()
	time.Sleep(100 * time.Millisecond)
	if got := sender.closeCallsCount(); got != 0 {
		t.Fatalf("Kafka closed before in-flight send completed: %d", got)
	}

	close(sender.blockSend)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reverse cleanup")
	}
	if err := processor.Shutdown(context.Background()); err != nil {
		t.Fatalf("batch Shutdown() error = %v", err)
	}
	if got := sender.closeCallsCount(); got != 1 {
		t.Fatalf("Kafka close calls = %d, want one after in-flight completion", got)
	}
	stopTaskRegistry(t, registry)
}

func newPreparedMetadataPlugin(t *testing.T, config Config, metadata map[string]any) *Plugin {
	t.Helper()
	view, err := runtime.NewMetadataView(map[string][]byte{
		name: mustJSONBytes(t, metadata),
	})
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	p := &Plugin{config: config}
	p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t), Metadata: view})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
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

func newObserverTestPlugin(t *testing.T, sender kafkaSender) *Plugin {
	t.Helper()
	p := &Plugin{
		config: Config{
			Kafka: &KafkaConfig{
				Brokers:    []KafkaBroker{{Host: "127.0.0.1", Port: 9092}},
				KafkaTopic: "apisix-error-logs",
			},
			Level:        "INFO",
			BatchMaxSize: 1,
		},
		kafkaSender: sender,
	}
	p.SetDependencies(
		base.Dependencies{
			Tasks: newLoggerTestTaskOwner(t),
		},
	)
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
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

func stopTaskRegistry(t *testing.T, registry *runtime.TaskRegistry) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if residuals, err := registry.Stop(ctx); err != nil {
		t.Fatalf("TaskRegistry.Stop() residuals=%v error=%v", residuals, err)
	}
}

func newObserverTaskOwner(t *testing.T, registry *runtime.TaskRegistry) *runtime.TaskOwner {
	t.Helper()
	owner, err := runtime.NewTaskOwner(
		registry,
		"plugin/error-log-logger/"+strings.Repeat("a", 64),
		runtime.TaskPlugin,
	)
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

func mustJSONBytes(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return encoded
}

func TestMaterializeScopedSecretsOwnsErrorLoggerPluginConfig(t *testing.T) {
	rawConfig := map[string]any{
		"clickhouse": map[string]any{
			"endpoint_addr": "http://clickhouse.invalid",
			"user":          "default",
			"password":      "$ENV://CLICKHOUSE_PASSWORD",
			"database":      "logs",
			"logtable":      "error_logs",
		},
		"kafka": map[string]any{
			"brokers": []any{
				map[string]any{
					"host": "broker-a",
					"port": 9092,
					"sasl_config": map[string]any{
						"user":     "user-a",
						"password": "$secret://KAFKA_PASSWORD_A",
					},
				},
				map[string]any{
					"host": "broker-b",
					"port": 9093,
					"sasl_config": map[string]any{
						"user":     "user-b",
						"password": "$ENV://KAFKA_PASSWORD_B",
					},
				},
			},
			"kafka_topic": "apisix-error-logs",
		},
	}
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Parse(rawConfig, p.Config()); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	fixture := newScopedPluginSecretFixture(t, map[string]string{
		"$ENV://CLICKHOUSE_PASSWORD": "clickhouse-secret",
		"$secret://KAFKA_PASSWORD_A": "kafka-secret-a",
		"$ENV://KAFKA_PASSWORD_B":    "kafka-secret-b",
	})
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), fixture.scope, fixture.capability, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}

	if got := p.config.Clickhouse.Password; got == "$ENV://CLICKHOUSE_PASSWORD" {
		t.Fatalf("clickhouse password = %q, want descriptor", got)
	}
	for index, broker := range p.config.Kafka.Brokers {
		if strings.HasPrefix(broker.SASLConfig.Password, "$") {
			t.Fatalf("broker %d password = %q, want descriptor", index, broker.SASLConfig.Password)
		}
	}
	if len(fixture.broker.scopes) != 3 {
		t.Fatalf("materialization scopes = %#v, want three plugin-config fields", fixture.broker.scopes)
	}
	wantFields := []string{
		"clickhouse.password",
		"kafka.brokers.*.sasl_config.password",
		"kafka.brokers.*.sasl_config.password",
	}
	for index, scope := range fixture.broker.scopes {
		if scope.Source != capability.SecretPluginConfig || scope.Plugin != name ||
			scope.Resource != fixture.scope.Resource || scope.Domain != generation.DomainHTTP ||
			scope.Field != wantFields[index] {
			t.Fatalf("materialization scope = %#v, want plugin-config route scope", scope)
		}
	}
	if p.observerStop != nil {
		t.Fatal("scoped plugin-config materialization started the observer")
	}
	p.Stop()
	if p.clickhousePassword != nil || len(p.kafkaPasswords) != 0 {
		t.Fatal("Stop() retained scoped private plugin-config values")
	}
}

func TestScopedClickHouseDeliveryUsesPrivateValue(t *testing.T) {
	var gotKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-ClickHouse-Key")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	p := &Plugin{config: Config{
		Clickhouse: &ClickHouseConfig{
			EndpointAddr: server.URL,
			User:         "default",
			Password:     "$ENV://CLICKHOUSE_PASSWORD",
			Database:     "logs",
			LogTable:     "error_logs",
		},
		Level: "INFO",
	}, client: &http.Client{}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	fixture := newScopedPluginSecretFixture(t, map[string]string{
		"$ENV://CLICKHOUSE_PASSWORD": "clickhouse-secret",
	})
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), fixture.scope, fixture.capability, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.SendLogs(context.Background(), []string{`2026/07/06 [error] boom`}); err != nil {
		t.Fatalf("SendLogs() error = %v", err)
	}
	if gotKey != "clickhouse-secret" {
		t.Fatalf("ClickHouse key = %q, want private materialized value", gotKey)
	}
	if p.config.Clickhouse.Password == "clickhouse-secret" ||
		!strings.Contains(p.config.Clickhouse.Password, "#sha256:") {
		t.Fatalf("public ClickHouse password = %q, want descriptor only", p.config.Clickhouse.Password)
	}
}

func TestScopedKafkaWriterUsesPrivateValues(t *testing.T) {
	p := &Plugin{config: Config{Kafka: &KafkaConfig{
		Brokers: []KafkaBroker{{
			Host: "broker-a",
			Port: 9092,
			SASLConfig: &SASLConfig{
				User:     "user-a",
				Password: "$secret://KAFKA_PASSWORD_A",
			},
		}},
		KafkaTopic: "apisix-error-logs",
	}}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	fixture := newScopedPluginSecretFixture(t, map[string]string{
		"$secret://KAFKA_PASSWORD_A": "kafka-secret-a",
	})
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), fixture.scope, fixture.capability, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	p.applyDefaults()
	writer, err := p.newKafkaWriter()
	if err != nil {
		t.Fatalf("newKafkaWriter() error = %v", err)
	}
	transport, ok := writer.Transport.(*kafka.Transport)
	if !ok || transport.SASL == nil {
		t.Fatal("writer does not have a SASL transport")
	}
	mechanism, ok := transport.SASL.(plain.Mechanism)
	if !ok {
		t.Fatalf("SASL mechanism type = %T, want plain.Mechanism", transport.SASL)
	}
	if mechanism.Password != "kafka-secret-a" {
		t.Fatalf("writer SASL password = %q, want private materialized value", mechanism.Password)
	}
	if p.config.Kafka.Brokers[0].SASLConfig.Password == "kafka-secret-a" {
		t.Fatal("public Kafka broker config retained plaintext password")
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close() error = %v", err)
	}
}

func TestStopDropsScopedKafkaWriterAndPrivateSecrets(t *testing.T) {
	p := &Plugin{config: Config{
		Clickhouse: &ClickHouseConfig{
			EndpointAddr: "http://clickhouse.invalid",
			Password:     "$ENV://CLICKHOUSE_PASSWORD",
		},
		Kafka: &KafkaConfig{
			Brokers: []KafkaBroker{{
				Host: "broker-a",
				Port: 9092,
				SASLConfig: &SASLConfig{
					User:     "user-a",
					Password: "$secret://KAFKA_PASSWORD_A",
				},
			}},
			KafkaTopic: "apisix-error-logs",
		},
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	fixture := newScopedPluginSecretFixture(t, map[string]string{
		"$ENV://CLICKHOUSE_PASSWORD": "clickhouse-secret",
		"$secret://KAFKA_PASSWORD_A": "kafka-secret-a",
	})
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), fixture.scope, fixture.capability, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if p.TaskOwner() == nil {
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}
	if p.kafkaSender == nil {
		t.Fatal("PostInit() did not create a Kafka sender")
	}

	processor := p.BatchProcessor
	p.Stop()
	if err := processor.Shutdown(context.Background()); err != nil {
		t.Fatalf("batch Shutdown() error = %v", err)
	}
	if p.kafkaSender != nil {
		t.Fatal("Stop() retained Kafka sender and its credential-bearing writer")
	}
	if p.clickhousePassword != nil || len(p.kafkaPasswords) != 0 {
		t.Fatal("Stop() retained private plugin-config secrets")
	}

	// Stop remains idempotent after the sender reference is dropped.
	p.Stop()
}

func TestMaterializeScopedSecretsFailureIsAtomicAndRedacted(t *testing.T) {
	clickhouseReference := "$ENV://CLICKHOUSE_PASSWORD"
	kafkaReference := "$secret://MISSING_KAFKA_PASSWORD"
	p := &Plugin{config: Config{
		Clickhouse: &ClickHouseConfig{Password: clickhouseReference},
		Kafka: &KafkaConfig{Brokers: []KafkaBroker{{
			SASLConfig: &SASLConfig{Password: kafkaReference},
		}}},
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	fixture := newScopedPluginSecretFixture(t, map[string]string{
		clickhouseReference: "clickhouse-secret",
	})
	err := base.MaterializeScopedPluginSecrets(
		context.Background(), fixture.scope, fixture.capability, p,
	)
	if err == nil {
		t.Fatal("MaterializeScopedPluginSecrets() error = nil, want resolver failure")
	}
	if strings.Contains(err.Error(), clickhouseReference) || strings.Contains(err.Error(), kafkaReference) {
		t.Fatalf("materialization error leaked a credential reference: %v", err)
	}
	if p.config.Clickhouse.Password != clickhouseReference ||
		p.config.Kafka.Brokers[0].SASLConfig.Password != kafkaReference {
		t.Fatalf("failed materialization mutated public config: %#v", p.config)
	}
	if p.clickhousePassword != nil || len(p.kafkaPasswords) != 0 || p.secretsMaterialized {
		t.Fatal("failed materialization retained partial private state")
	}
}

type scopedPluginSecretFixture struct {
	broker     *errorLoggerScopedBroker
	scope      secret.Scope
	capability secret.GenerationSecrets
}

func newScopedPluginSecretFixture(t *testing.T, resolved map[string]string) scopedPluginSecretFixture {
	t.Helper()
	catalog, err := capability.NewSecretDeclarationCatalog()
	if err != nil {
		t.Fatalf("NewSecretDeclarationCatalog() error = %v", err)
	}
	broker := &errorLoggerScopedBroker{resolved: resolved}
	materializer := testutil.NewSecretMaterializer(broker, catalog)
	snapshot, err := generation.NewSnapshot(42, []generation.Resource{{
		Key:   generation.ResourceKey{Kind: "routes", ID: "error-log-test"},
		Value: []byte(`{"id":"error-log-test","plugins":{"error-log-logger":{}}}`),
	}}, nil)
	if err != nil {
		t.Fatalf("generation.NewSnapshot() error = %v", err)
	}
	candidate := generation.PublicationCandidate{
		Artifact: generation.GenerationArtifact{
			Domain:   generation.DomainHTTP,
			Revision: snapshot.Revision(),
			Digest:   snapshot.Digest(),
			Snapshot: snapshot.SnapshotID(),
		},
		Snapshot: snapshot,
		Closure: []generation.ResourceKey{{
			Kind: "routes", ID: "error-log-test",
		}},
		Decisions: []generation.ResourceDecision{{
			Key:         generation.ResourceKey{Kind: "routes", ID: "error-log-test"},
			Disposition: generation.DispositionPublished,
			Code:        "test-published",
		}},
	}
	materialization, err := materializer.PrepareGeneration(context.Background(), generation.PublicationSet{
		DesiredRevision: 42,
		Domains: map[generation.Domain]generation.PublicationCandidate{
			generation.DomainHTTP: candidate,
		},
	})
	if err != nil {
		t.Fatalf("PrepareGeneration() error = %v", err)
	}
	t.Cleanup(func() {
		if err := materialization.Close(context.Background()); err != nil {
			t.Errorf("close scoped registration: %v", err)
		}
	})
	secrets := materialization.Secrets()
	return scopedPluginSecretFixture{
		broker: broker,
		scope: secret.Scope{
			Generation: 42,
			Domain:     generation.DomainHTTP,
			Plugin:     name,
			Resource:   generation.ResourceKey{Kind: "routes", ID: "error-log-test"},
			Source:     capability.SecretPluginConfig,
		},
		capability: secrets,
	}
}

type errorLoggerScopedBroker struct {
	resolved map[string]string
	scopes   []secret.Scope
}

func (broker *errorLoggerScopedBroker) ResolveScoped(
	_ context.Context,
	scope secret.Scope,
	raw string,
) (string, error) {
	broker.scopes = append(broker.scopes, scope)
	resolved, ok := broker.resolved[raw]
	if !ok {
		return "", fmt.Errorf("missing test credential")
	}
	return resolved, nil
}

func TestSendToTCPReturnsWithinWriteDeadline(t *testing.T) {
	conn := &deadlineWriteConn{}
	p := &Plugin{
		config: Config{
			TCP:     &TCPConfig{Host: "127.0.0.1", Port: 1},
			Timeout: 1,
		},
	}
	p.dialTCP = func(context.Context, string, string, time.Duration, *tls.Config, bool) (net.Conn, error) {
		return conn, nil
	}

	start := time.Now()
	err := p.sendToTCP(context.Background(), []string{"blocked write"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("sendToTCP() error = nil, want write deadline error")
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("sendToTCP() took %s, want return within twice the configured 1s timeout", elapsed)
	}
}

func TestSendBatchHonorsParentContextForHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("canceled error-log request reached the backend")
	}))
	t.Cleanup(server.Close)

	p := &Plugin{config: Config{
		Skywalking: &SkywalkingConfig{EndpointAddr: server.URL},
		Timeout:    30,
	}, client: &http.Client{}}
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.SendBatch(parent, []map[string]any{{"message": "line"}}, 1); err == nil {
		t.Fatal("SendBatch() error = nil, want canceled parent error")
	}
}

func TestSendToTCPHonorsParentDeadline(t *testing.T) {
	conn := &deadlineWriteConn{}
	p := &Plugin{
		config: Config{
			TCP:     &TCPConfig{Host: "127.0.0.1", Port: 1},
			Timeout: 30,
		},
	}
	p.dialTCP = func(context.Context, string, string, time.Duration, *tls.Config, bool) (net.Conn, error) {
		return conn, nil
	}

	parent, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := p.sendToTCP(parent, []string{"blocked write"})
	if err == nil {
		t.Fatal("sendToTCP() error = nil, want parent deadline error")
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("sendToTCP() took %s, want parent deadline to win", elapsed)
	}
}

func TestBatchProcessorDefaultsMaxPendingEntries(t *testing.T) {
	p := newTestPlugin(t, Config{
		TCP:             &TCPConfig{Host: "127.0.0.1", Port: 1},
		BatchMaxSize:    50000,
		InactiveTimeout: 3600,
		BufferDuration:  3600,
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

func TestMetadataSchemaAcceptsMaxPendingEntries(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := util.Validate(map[string]any{
		"tcp":                 map[string]any{"host": "127.0.0.1", "port": 1999},
		"max_pending_entries": 100,
	}, p.GetMetadataSchema()); err != nil {
		t.Fatalf("schema rejected max_pending_entries: %v", err)
	}
}

func TestSendLogsFiltersByLevelAndWritesTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	received := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		buf := make([]byte, 512)
		n, _ := conn.Read(buf)
		received <- string(buf[:n])
	}()

	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	p := newTestPlugin(t, Config{
		TCP:   &TCPConfig{Host: host, Port: mustAtoi(t, port)},
		Level: "WARN",
	})

	if err := p.SendLogs(context.Background(), []string{
		`2026/07/06 01:00:00 [info] skip`,
		`2026/07/06 01:00:01 [error] boom`,
		`2026/07/06 01:00:02 [warn] careful`,
	}); err != nil {
		t.Fatalf("SendLogs() error = %v", err)
	}

	got := <-received
	if strings.Contains(got, "skip") {
		t.Fatalf("tcp payload = %q, want info filtered out", got)
	}
	if !strings.Contains(got, "boom\n") || !strings.Contains(got, "careful\n") {
		t.Fatalf("tcp payload = %q, want error and warn lines", got)
	}
}

func TestSendLogsToSkyWalking(t *testing.T) {
	var entries []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/logs" {
			t.Fatalf("path = %q, want /v3/logs", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&entries); err != nil {
			t.Fatalf("decode skywalking entries: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		Skywalking: &SkywalkingConfig{
			EndpointAddr:        server.URL + "/v3/logs",
			ServiceName:         "APISIX",
			ServiceInstanceName: "instance-a",
		},
		Level: "INFO",
	})

	if err := p.SendLogs(context.Background(), []string{`2026/07/06 [warn] hello`}); err != nil {
		t.Fatalf("SendLogs() error = %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("entries len = %d, want 1", len(entries))
	}
	if entries[0]["service"] != "APISIX" || entries[0]["serviceInstance"] != "instance-a" {
		t.Fatalf("skywalking identity = %#v", entries[0])
	}
	body := entries[0]["body"].(map[string]any)
	text := body["text"].(map[string]any)
	if text["text"] != `2026/07/06 [warn] hello` {
		t.Fatalf("skywalking text = %v", text["text"])
	}
}

func TestSendLogsToSkyWalkingResolvesHostnameInstance(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatalf("hostname: %v", err)
	}

	var entries []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&entries); err != nil {
			t.Fatalf("decode skywalking entries: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		Skywalking: &SkywalkingConfig{
			EndpointAddr:        server.URL,
			ServiceInstanceName: "$hostname",
		},
		Level: "INFO",
	})

	if err := p.SendLogs(context.Background(), []string{`2026/07/06 [warn] hello`}); err != nil {
		t.Fatalf("SendLogs() error = %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("entries len = %d, want 1", len(entries))
	}
	if entries[0]["serviceInstance"] != hostname {
		t.Fatalf("serviceInstance = %q, want hostname %q", entries[0]["serviceInstance"], hostname)
	}
}

func TestSendLogsToClickHouse(t *testing.T) {
	var body string
	var user string
	var key string
	var database string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		body = string(buf)
		user = r.Header.Get("X-ClickHouse-User")
		key = r.Header.Get("X-ClickHouse-Key")
		database = r.Header.Get("X-ClickHouse-Database")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		Clickhouse: &ClickHouseConfig{
			EndpointAddr: server.URL,
			User:         "default",
			Password:     "secret",
			Database:     "logs",
			LogTable:     "error_logs",
		},
		Level: "INFO",
	})

	if err := p.SendLogs(context.Background(), []string{`2026/07/06 [error] boom`}); err != nil {
		t.Fatalf("SendLogs() error = %v", err)
	}

	if !strings.HasPrefix(body, "INSERT INTO error_logs FORMAT JSONEachRow ") {
		t.Fatalf("clickhouse body = %q", body)
	}
	if !strings.Contains(body, `{"data":"2026/07/06 [error] boom"}`) {
		t.Fatalf("clickhouse body = %q, want JSONEachRow data", body)
	}
	if user != "default" || key != "secret" || database != "logs" {
		t.Fatalf("clickhouse headers = %q/%q/%q", user, key, database)
	}
}

func TestSendLogsToKafka(t *testing.T) {
	sender := &fakeKafkaSender{}
	p := newTestPlugin(t, Config{
		Kafka: &KafkaConfig{
			Brokers:    []KafkaBroker{{Host: "127.0.0.1", Port: 9092}},
			KafkaTopic: "apisix-error-logs",
			Key:        "error",
		},
		Level: "ERROR",
	})
	p.kafkaSender = sender

	if err := p.SendLogs(context.Background(), []string{
		`2026/07/06 [warn] skip`,
		`2026/07/06 [error] boom`,
	}); err != nil {
		t.Fatalf("SendLogs() error = %v", err)
	}

	if len(sender.messages) != 1 {
		t.Fatalf("kafka messages len = %d, want 1", len(sender.messages))
	}
	if sender.messages[0].Topic != "apisix-error-logs" || string(sender.messages[0].Key) != "error" {
		t.Fatalf("kafka message = %#v", sender.messages[0])
	}
	if string(sender.messages[0].Value) != `"2026/07/06 [error] boom"` {
		t.Fatalf("kafka value = %s", sender.messages[0].Value)
	}
}

func TestKafkaSASLMechanismDefaultsToPlain(t *testing.T) {
	p := &Plugin{config: Config{
		Kafka: &KafkaConfig{
			Brokers: []KafkaBroker{{
				Host: "127.0.0.1",
				Port: 9092,
				SASLConfig: &SASLConfig{
					User:     "user",
					Password: "pass",
				},
			}},
			KafkaTopic: "apisix-error-logs",
		},
	}}

	mechanism, err := saslMechanismFor(p.config.Kafka)
	if err != nil {
		t.Fatalf("saslMechanismFor() error = %v", err)
	}
	if mechanism == nil {
		t.Fatal("saslMechanismFor() returned nil")
	}
	if got := mechanism.Name(); got != "PLAIN" {
		t.Fatalf("SASL mechanism = %q, want PLAIN", got)
	}
}

func TestNewKafkaWriterUsesBrokerSASLConfig(t *testing.T) {
	p := &Plugin{config: Config{
		Kafka: &KafkaConfig{
			Brokers: []KafkaBroker{{
				Host: "127.0.0.1",
				Port: 9092,
				SASLConfig: &SASLConfig{
					Mechanism: "PLAIN",
					User:      "user",
					Password:  "pass",
				},
			}},
			KafkaTopic: "apisix-error-logs",
		},
	}}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	fixture := newScopedPluginSecretFixture(t, map[string]string{"pass": "pass"})
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), fixture.scope, fixture.capability, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	p.applyDefaults()

	writer, err := p.newKafkaWriter()
	if err != nil {
		t.Fatalf("newKafkaWriter() error = %v", err)
	}
	transport, ok := writer.Transport.(*kafka.Transport)
	if !ok || transport.SASL == nil {
		t.Fatal("writer does not have a SASL transport")
	}
	if got := transport.SASL.Name(); got != "PLAIN" {
		t.Fatalf("writer SASL mechanism = %q, want PLAIN", got)
	}
}

func TestNewKafkaWriterUsesMessageTopicOnly(t *testing.T) {
	p := &Plugin{config: Config{
		Kafka: &KafkaConfig{
			Brokers:    []KafkaBroker{{Host: "127.0.0.1", Port: 9092}},
			KafkaTopic: "apisix-error-logs",
		},
	}}
	p.applyDefaults()

	writer, err := p.newKafkaWriter()
	if err != nil {
		t.Fatalf("newKafkaWriter() error = %v", err)
	}
	if writer.Topic != "" {
		t.Fatalf("writer.Topic = %q, want empty so kafkaMessage.Topic selects the destination", writer.Topic)
	}
}

func TestStartObservingUsesBatchProcessor(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	received := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		buf := make([]byte, 1024)
		n, _ := conn.Read(buf)
		received <- string(buf[:n])
	}()

	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	p := newTestPlugin(t, Config{
		TCP:             &TCPConfig{Host: host, Port: mustAtoi(t, port)},
		Level:           "INFO",
		BatchMaxSize:    2,
		BufferDuration:  60,
		InactiveTimeout: 60,
	})

	p.startupWarningsSent = true
	p.StartObserving()
	logger.Info("one")
	select {
	case got := <-received:
		t.Fatalf("received payload before batch was full: %q", got)
	case <-time.After(50 * time.Millisecond):
	}

	logger.Info("two")

	select {
	case got := <-received:
		if !strings.Contains(got, "[info] one") || !strings.Contains(got, "[info] two") {
			t.Fatalf("tcp payload = %q, want both batched log entries", got)
		}
		if lines := strings.Count(strings.TrimSpace(got), "\n") + 1; lines != 2 {
			t.Fatalf("tcp payload = %q, want two newline-delimited entries", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for batched tcp payload")
	}
}

func TestStartObservingForwardsApplicationLogsToCurrentOwner(t *testing.T) {
	firstListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen first sink: %v", err)
	}
	t.Cleanup(func() { _ = firstListener.Close() })
	firstHost, firstPortText, err := net.SplitHostPort(firstListener.Addr().String())
	if err != nil {
		t.Fatalf("split first sink: %v", err)
	}
	first := newTestPlugin(t, Config{
		TCP:          &TCPConfig{Host: firstHost, Port: mustAtoi(t, firstPortText)},
		Level:        "WARN",
		BatchMaxSize: 1,
	})
	first.StartObserving()

	secondListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen second sink: %v", err)
	}
	t.Cleanup(func() { _ = secondListener.Close() })
	secondHost, secondPortText, err := net.SplitHostPort(secondListener.Addr().String())
	if err != nil {
		t.Fatalf("split second sink: %v", err)
	}
	second := newTestPlugin(t, Config{
		TCP:          &TCPConfig{Host: secondHost, Port: mustAtoi(t, secondPortText)},
		Level:        "WARN",
		BatchMaxSize: 1,
	})
	second.StartObserving()

	first.Stop()
	received := make(chan string, 2)
	go func() {
		for range 2 {
			conn, acceptErr := secondListener.Accept()
			if acceptErr != nil {
				return
			}
			body, _ := io.ReadAll(conn)
			_ = conn.Close()
			received <- string(body)
		}
	}()

	select {
	case payload := <-received:
		if !strings.Contains(payload, "Keeping tcp.tls disabled in error-log-logger configuration is a security risk") {
			t.Fatalf("startup payload = %q, want TCP security warning", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for startup security warning")
	}

	logger.Warn("standalone error-log observer marker")
	select {
	case payload := <-received:
		if !strings.Contains(payload, "[warn] standalone error-log observer marker") {
			t.Fatalf("payload = %q, want observed warning", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for current error-log observer")
	}
}

func TestStartObservingFiltersBeforeBatching(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen sink: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split sink: %v", err)
	}
	p := newTestPlugin(t, Config{
		TCP:          &TCPConfig{Host: host, Port: mustAtoi(t, portText)},
		Level:        "WARN",
		BatchMaxSize: 2,
	})
	p.StartObserving()

	received := make(chan string, 2)
	go func() {
		for range 2 {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			body, _ := io.ReadAll(conn)
			_ = conn.Close()
			received <- string(body)
		}
	}()
	logger.Warn("flush startup security warning")
	select {
	case payload := <-received:
		if !strings.Contains(
			payload,
			"Keeping tcp.tls disabled in error-log-logger configuration is a security risk",
		) ||
			!strings.Contains(payload, "flush startup security warning") {
			t.Fatalf("startup payload = %q, want security warning batch", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for startup warning batch")
	}

	logger.Info("filtered observer info")
	logger.Warn("first eligible observer warning")
	select {
	case payload := <-received:
		t.Fatalf("payload = %q before two eligible entries, low-level info counted toward batch size", payload)
	case <-time.After(100 * time.Millisecond):
	}

	logger.Error("second eligible observer error")
	select {
	case payload := <-received:
		if strings.Contains(payload, "filtered observer info") {
			t.Fatalf("payload = %q, want info filtered out", payload)
		}
		if !strings.Contains(payload, "first eligible observer warning") ||
			!strings.Contains(payload, "second eligible observer error") {
			t.Fatalf("payload = %q, want warning and error in one batch", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for two eligible log entries")
	}
}

func TestStartObservingRetriesFailedBatch(t *testing.T) {
	var attempts atomic.Int32
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			http.Error(w, "temporary", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		close(done)
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		Skywalking:      &SkywalkingConfig{EndpointAddr: server.URL},
		Level:           "INFO",
		BatchMaxSize:    1,
		MaxRetryCount:   1,
		RetryDelay:      1,
		BufferDuration:  60,
		InactiveTimeout: 60,
	})

	p.startupWarningsSent = true
	p.StartObserving()
	logger.Info("retry me")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for retry, attempts = %d", attempts.Load())
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want first failure plus one retry", attempts.Load())
	}
}

func TestDefaultsMatchOfficialMetadata(t *testing.T) {
	p := newTestPlugin(t, Config{})

	if p.config.Name != "error-log-logger" {
		t.Fatalf("name = %q, want error-log-logger", p.config.Name)
	}
	if p.config.Level != "WARN" || p.config.Timeout != 3 || p.config.Keepalive != 30 {
		t.Fatalf("defaults = level %q timeout %d keepalive %d", p.config.Level, p.config.Timeout, p.config.Keepalive)
	}
	if p.config.BatchMaxSize != 1000 || p.config.BufferDuration != 60 || p.config.InactiveTimeout != 3 {
		t.Fatalf("batch defaults = %d/%d/%d", p.config.BatchMaxSize, p.config.BufferDuration, p.config.InactiveTimeout)
	}
}

func TestMetadataSchemaRejectsTCPWithoutHost(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	err := util.Validate(map[string]any{
		"tcp": map[string]any{"port": 1999},
	}, p.GetMetadataSchema())
	if err == nil || !strings.Contains(err.Error(), "host") {
		t.Fatalf("Validate() error = %v, want missing tcp.host rejection", err)
	}
}

func TestMetadataSchemaRejectsMissingSink(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	if err := util.Validate(map[string]any{"level": "WARN"}, p.GetMetadataSchema()); err == nil {
		t.Fatal("Validate() error = nil, want metadata without a sink rejected")
	}
}

type fakeKafkaSender struct {
	mu         sync.Mutex
	messages   []kafkaMessage
	delivered  chan struct{}
	closeCalls int
	closeErr   error
	blockSend  chan struct{}
}

func (f *fakeKafkaSender) Send(_ context.Context, message kafkaMessage) error {
	f.mu.Lock()
	f.messages = append(f.messages, message)
	f.mu.Unlock()
	if f.delivered != nil {
		select {
		case f.delivered <- struct{}{}:
		default:
		}
	}
	if f.blockSend != nil {
		<-f.blockSend
	}
	return nil
}

func (f *fakeKafkaSender) Close() error {
	f.mu.Lock()
	f.closeCalls++
	f.mu.Unlock()
	return f.closeErr
}

func (f *fakeKafkaSender) messagesCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.messages)
}

func (f *fakeKafkaSender) closeCallsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeCalls
}

func TestStopUnregistersObserverAndClosesKafkaWriterOnce(t *testing.T) {
	captured := make(chan logger.Entry, 8)
	stopCapture := logger.ReplaceObserver("error-log-logger-close-test", func(entry logger.Entry) {
		captured <- entry
	})
	t.Cleanup(stopCapture)

	sender := &fakeKafkaSender{closeErr: fmt.Errorf("kafka close boom")}
	p := newTestPlugin(t, Config{
		Kafka: &KafkaConfig{
			Brokers:    []KafkaBroker{{Host: "127.0.0.1", Port: 9092}},
			KafkaTopic: "apisix-error-logs",
		},
		Level:           "WARN",
		BatchMaxSize:    1,
		BufferDuration:  60,
		InactiveTimeout: 60,
	})
	p.kafkaSender = sender

	p.StartObserving()
	logger.Warn("delivered before stop")
	waitFor(t, func() bool { return sender.messagesCount() == 1 }, "first delivery")

	processor := p.BatchProcessor
	p.Stop()
	if err := processor.Shutdown(context.Background()); err != nil {
		t.Fatalf("batch Shutdown() error = %v", err)
	}
	if got := sender.closeCallsCount(); got != 1 {
		t.Fatalf("kafka writer close calls = %d, want 1", got)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		select {
		case entry := <-captured:
			if strings.Contains(entry.Message, "kafka close boom") {
				goto closeErrorLogged
			}
		case <-time.After(time.Until(deadline)):
			t.Fatal("timed out waiting for the kafka close error log")
		}
	}
closeErrorLogged:

	// The observer was unregistered: nothing is delivered after Stop.
	logger.Warn("delivered after stop")
	time.Sleep(100 * time.Millisecond)
	if got := sender.messagesCount(); got != 1 {
		t.Fatalf("deliveries after Stop = %d, want the single pre-stop delivery", got)
	}

	// Stop is idempotent: the writer is closed exactly once.
	p.Stop()
	if got := sender.closeCallsCount(); got != 1 {
		t.Fatalf("kafka writer close calls after second Stop = %d, want 1", got)
	}
}

func TestStopReturnsBeforeInflightKafkaSendAndDefersClosing(t *testing.T) {
	sender := &fakeKafkaSender{blockSend: make(chan struct{})}
	p := newTestPlugin(t, Config{
		Kafka: &KafkaConfig{
			Brokers:    []KafkaBroker{{Host: "127.0.0.1", Port: 9092}},
			KafkaTopic: "apisix-error-logs",
		},
		Level:           "WARN",
		BatchMaxSize:    1,
		BufferDuration:  60,
		InactiveTimeout: 60,
	})
	p.kafkaSender = sender

	p.StartObserving()
	logger.Warn("in-flight send")
	waitFor(t, func() bool { return sender.messagesCount() == 1 }, "send in flight")
	processor := p.BatchProcessor

	stopped := make(chan struct{})
	go func() {
		p.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop blocked on the in-flight send")
	}
	if got := sender.closeCallsCount(); got != 0 {
		t.Fatalf("kafka writer closed while a send was in flight, close calls = %d", got)
	}

	close(sender.blockSend)
	if err := processor.Shutdown(context.Background()); err != nil {
		t.Fatalf("batch Shutdown() error = %v", err)
	}
	if got := sender.closeCallsCount(); got != 1 {
		t.Fatalf("kafka writer close calls = %d, want 1 after join", got)
	}
}

func waitFor(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		t.Fatalf("atoi %q: %v", s, err)
	}
	return n
}

type deadlineWriteConn struct {
	mu       sync.Mutex
	deadline time.Time
	closed   bool
}

func (c *deadlineWriteConn) Write([]byte) (int, error) {
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

func (c *deadlineWriteConn) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (c *deadlineWriteConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *deadlineWriteConn) LocalAddr() net.Addr {
	return errorLoggerTestAddr("local")
}

func (c *deadlineWriteConn) RemoteAddr() net.Addr {
	return errorLoggerTestAddr("remote")
}

func (c *deadlineWriteConn) SetDeadline(time.Time) error {
	return nil
}

func (c *deadlineWriteConn) SetReadDeadline(time.Time) error {
	return nil
}

func (c *deadlineWriteConn) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = deadline
	return nil
}

type errorLoggerTestAddr string

func (a errorLoggerTestAddr) Network() string {
	return "test"
}

func (a errorLoggerTestAddr) String() string {
	return string(a)
}
