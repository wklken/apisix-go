package request_validation

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/secret"
)

func TestSensitiveCompileSemaphoreEnforcesGenerationLimit(t *testing.T) {
	p, closeAttempt := newQueuedSensitiveCompilePlugin(t, 714)
	defer closeAttempt()
	releases := make([]func(), 0, requestValidationSensitiveCompileConcurrency)
	for range requestValidationSensitiveCompileConcurrency {
		release, err := p.secrets.acquireSensitiveCompile(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	defer func() {
		for _, release := range releases {
			release()
		}
		p.Stop()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.secrets.acquireSensitiveCompile(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("compile beyond generation limit error = %v, want deadline exceeded", err)
	}

	releases[0]()
	releases = releases[1:]
	release, err := p.secrets.acquireSensitiveCompile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	releases = append(releases, release)
}

func TestSensitiveCompileSemaphoreIsSharedByAttemptAndBindingStopIsLocal(t *testing.T) {
	const (
		raw       = "$ENV://REQUEST_VALIDATION_SHARED_COMPILE"
		plaintext = "shared-compile-private"
	)
	capabilityValue, scope, _, closeAttempt := newRequestValidationSecretHarness(
		t, 710, map[string]string{raw: plaintext},
	)
	defer closeAttempt()
	first := newSensitiveCompilePluginWithAccess(t, capabilityValue, scope, raw)
	second := newSensitiveCompilePluginWithAccess(t, capabilityValue, scope, raw)
	defer second.Stop()

	releases := holdSensitiveCompileSlots(t, first)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	release, err := second.secrets.acquireSensitiveCompile(ctx)
	if release != nil {
		release()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fifth compile across two bindings = %v, want shared-attempt limit", err)
	}
	for _, release := range releases {
		release()
	}

	first.Stop()
	release, err = second.secrets.acquireSensitiveCompile(context.Background())
	if err != nil {
		t.Fatalf("stopping one binding closed shared attempt gate: %v", err)
	}
	release()
}

func TestSensitiveCompileSemaphoreIsIsolatedAcrossAttempts(t *testing.T) {
	const raw = "$ENV://REQUEST_VALIDATION_ISOLATED_COMPILE"
	firstCapability, firstScope, _, closeFirst := newRequestValidationSecretHarness(
		t, 711, map[string]string{raw: "first-private"},
	)
	defer closeFirst()
	secondCapability, secondScope, _, closeSecond := newRequestValidationSecretHarness(
		t, 712, map[string]string{raw: "second-private"},
	)
	defer closeSecond()
	first := newSensitiveCompilePluginWithAccess(t, firstCapability, firstScope, raw)
	second := newSensitiveCompilePluginWithAccess(t, secondCapability, secondScope, raw)
	defer first.Stop()
	defer second.Stop()
	releases := holdSensitiveCompileSlots(t, first)
	defer func() {
		for _, release := range releases {
			release()
		}
	}()

	release, err := second.secrets.acquireSensitiveCompile(context.Background())
	if err != nil {
		t.Fatalf("different attempt shared compile capacity: %v", err)
	}
	release()
}

func TestSensitiveCompileCancellationWinsWhenSlotBecomesReady(t *testing.T) {
	p, closeAttempt := newQueuedSensitiveCompilePlugin(t, 713)
	defer closeAttempt()
	defer p.Stop()
	releases := holdSensitiveCompileSlots(t, p)
	defer func() {
		for _, release := range releases {
			release()
		}
	}()

	type result struct {
		release func()
		err     error
	}
	for iteration := range 4096 {
		ctx, cancel := context.WithCancel(context.Background())
		resultCh := make(chan result, 1)
		start := make(chan struct{})
		go func() {
			<-start
			release, err := p.secrets.acquireSensitiveCompile(ctx)
			resultCh <- result{release: release, err: err}
		}()
		close(start)
		cancel()
		releases[len(releases)-1]()
		releases = releases[:len(releases)-1]
		got := <-resultCh
		if got.release != nil {
			got.release()
			t.Fatalf("iteration %d entered compile after cancellation", iteration)
		}
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("iteration %d cancellation error = %v", iteration, got.err)
		}
		release, err := p.secrets.acquireSensitiveCompile(context.Background())
		if err != nil {
			t.Fatalf("iteration %d did not release canceled slot: %v", iteration, err)
		}
		releases = append(releases, release)
	}
}

func TestQueuedSensitiveValidationStopsWaitingWhenRequestIsCanceled(t *testing.T) {
	p, closeAttempt := newQueuedSensitiveCompilePlugin(t, 707)
	defer closeAttempt()
	releases := holdSensitiveCompileSlots(t, p)
	defer func() {
		for _, release := range releases {
			release()
		}
		p.Stop()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"token":"queued-private"}`))
	request = request.WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("canceled queued validation reached downstream")
		})).ServeHTTP(response, request)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("queued validation returned before request cancellation")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("queued validation did not wake after request cancellation")
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("canceled queued validation status = %d, want 503", response.Code)
	}
}

func TestStopWakesQueuedSensitiveValidationWithoutCompileSlot(t *testing.T) {
	p, closeAttempt := newQueuedSensitiveCompilePlugin(t, 708)
	defer closeAttempt()
	releases := holdSensitiveCompileSlots(t, p)
	defer func() {
		for _, release := range releases {
			release()
		}
	}()

	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"token":"queued-private"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	requestDone := make(chan struct{})
	go func() {
		p.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("stopped queued validation reached downstream")
		})).ServeHTTP(response, request)
		close(requestDone)
	}()
	select {
	case <-requestDone:
		t.Fatal("queued validation returned before Stop")
	case <-time.After(50 * time.Millisecond):
	}

	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-requestDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not wake queued validation")
	}
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop remained blocked by queued validation")
	}
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("stopped queued validation status = %d, want 503", response.Code)
	}
}

func newQueuedSensitiveCompilePlugin(t *testing.T, revision uint64) (*Plugin, func()) {
	t.Helper()
	const (
		raw       = "$ENV://REQUEST_VALIDATION_QUEUED_COMPILE"
		plaintext = "queued-private"
	)
	capabilityValue, scope, _, closeAttempt := newRequestValidationSecretHarness(
		t, revision, map[string]string{raw: plaintext},
	)
	p := &Plugin{config: Config{BodySchema: map[string]any{
		"type": "object", "properties": map[string]any{
			"token": map[string]any{"const": raw},
		},
	}}}
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
}

func newSensitiveCompilePluginWithAccess(
	t *testing.T,
	capabilityValue secret.GenerationCapability,
	scope secret.Scope,
	raw string,
) *Plugin {
	t.Helper()
	p := &Plugin{config: Config{BodySchema: map[string]any{
		"type": "string", "const": raw,
	}}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatal(err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	return p
}

func holdSensitiveCompileSlots(t *testing.T, p *Plugin) []func() {
	t.Helper()
	releases := make([]func(), 0, requestValidationSensitiveCompileConcurrency)
	for range requestValidationSensitiveCompileConcurrency {
		release, err := p.secrets.acquireSensitiveCompile(context.Background())
		if err != nil {
			for _, held := range releases {
				held()
			}
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	return releases
}
