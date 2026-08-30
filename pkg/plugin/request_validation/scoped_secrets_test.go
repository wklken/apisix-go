package request_validation

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
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

type requestValidationSecretCall struct {
	scope secret.Scope
	raw   string
}

type requestValidationSecretBroker struct {
	mu      sync.Mutex
	values  map[string]string
	fail    map[string]error
	calls   []requestValidationSecretCall
	revokes int
}

func (*requestValidationSecretBroker) AuthorizeCandidate(
	context.Context, secret.AttemptID, generation.ApplyTicket, generation.PublicationSet,
) error {
	return nil
}

func (*requestValidationSecretBroker) AuthorizeRecovery(
	context.Context,
	secret.AttemptID,
	generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return errors.New("recovery is not used by this fixture")
}

func (broker *requestValidationSecretBroker) ResolveScoped(
	ctx context.Context, scope secret.Scope, raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, requestValidationSecretCall{scope: scope, raw: raw})
	if err := broker.fail[raw]; err != nil {
		return "", err
	}
	value, ok := broker.values[raw]
	if !ok {
		return "", errors.New("missing request-validation scoped value")
	}
	return value, nil
}

func (broker *requestValidationSecretBroker) RevokeAttempt(context.Context, secret.AttemptID) error {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.revokes++
	return nil
}

func (broker *requestValidationSecretBroker) callsSnapshot() []requestValidationSecretCall {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return append([]requestValidationSecretCall(nil), broker.calls...)
}

func (broker *requestValidationSecretBroker) setFailure(raw string, err error) {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if err == nil {
		delete(broker.fail, raw)
		return
	}
	broker.fail[raw] = err
}

func (broker *requestValidationSecretBroker) revokeCount() int {
	broker.mu.Lock()
	defer broker.mu.Unlock()
	return broker.revokes
}

func newRequestValidationSecretHarness(
	t *testing.T, revision uint64, values map[string]string,
) (secret.GenerationCapability, secret.Scope, *requestValidationSecretBroker, func()) {
	t.Helper()
	resourceKey := generation.ResourceKey{
		Kind: "routes", ID: fmt.Sprintf("request-validation-scoped-%d", revision),
	}
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{
		Key: resourceKey, Value: []byte(`{"plugins":{}}`),
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
		Closure:  []generation.ResourceKey{resourceKey},
		Decisions: []generation.ResourceDecision{{
			Key: resourceKey, Disposition: generation.DispositionPublished,
			Code: "request-validation-scoped-test",
		}},
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: revision, RequiredDomains: []generation.Domain{generation.DomainHTTP},
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
	broker := &requestValidationSecretBroker{
		values: maps.Clone(values), fail: make(map[string]error),
	}
	registration, err := testutil.NewSecretMaterializer(broker, catalog).RegisterCandidate(
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
		Generation: revision, Attempt: registration.AttemptID(), Domain: generation.DomainHTTP,
		Plugin: name, Resource: resourceKey, Source: capability.SecretPluginConfig,
	}
	return capabilityValue, scope, broker, func() {
		if err := registration.Close(context.Background()); err != nil {
			t.Errorf("close scoped request-validation attempt: %v", err)
		}
	}
}

func TestSecretBackedSchemaDoesNotEscapeUseIntoGenerationCompiledState(t *testing.T) {
	const (
		raw       = "$ENV://REQUEST_VALIDATION_EPHEMERAL_COMPILE"
		plaintext = "generation-private-value"
	)
	capabilityValue, scope, _, closeAttempt := newRequestValidationSecretHarness(
		t, 307, map[string]string{raw: plaintext},
	)
	defer closeAttempt()
	p := &Plugin{config: Config{BodySchema: map[string]any{
		"type": "object", "properties": map[string]any{
			"token": map[string]any{"const": raw},
		},
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
	if p.config.bodySchema != nil {
		t.Fatal("secret-backed body schema escaped Value.Use into generation-long compiled state")
	}

	valid := performRequest(p, http.MethodPost, "/", `{"token":"`+plaintext+`"}`, map[string]string{
		"Content-Type": "application/json",
	})
	if valid.Code != http.StatusNoContent {
		t.Fatalf("valid secret-backed request = %d/%q", valid.Code, valid.Body.String())
	}
	invalid := performRequest(p, http.MethodPost, "/", `{"token":"wrong"}`, map[string]string{
		"Content-Type": "application/json",
	})
	if got := strings.TrimSpace(invalid.Body.String()); invalid.Code != http.StatusBadRequest ||
		got != "request does not match schema" || strings.Contains(got, plaintext) {
		t.Fatalf("invalid secret-backed request = %d/%q", invalid.Code, got)
	}
	p.Stop()
}

func TestScopedSecretsCoverAllTerminalStringSchemaRolesWithoutResolvingMapKeys(t *testing.T) {
	const (
		typeRaw        = "$ENV://REQUEST_VALIDATION_SCHEMA_TYPE"
		refRaw         = "$ENV://REQUEST_VALIDATION_SCHEMA_REF"
		requiredRaw    = "$ENV://REQUEST_VALIDATION_SCHEMA_REQUIRED"
		patternRaw     = "$ENV://REQUEST_VALIDATION_SCHEMA_PATTERN"
		formatRaw      = "$ENV://REQUEST_VALIDATION_SCHEMA_FORMAT"
		literalRaw     = "$ENV://REQUEST_VALIDATION_SCHEMA_LITERAL"
		defaultRaw     = "$secret://request-validation/schema-default"
		annotationRaw  = "$ENV://REQUEST_VALIDATION_SCHEMA_ANNOTATION"
		mapKeyEnvelope = "$ENV://REQUEST_VALIDATION_SCHEMA_MAP_KEY"
	)
	values := map[string]string{
		typeRaw:       "string",
		refRaw:        "#/$defs/token",
		requiredRaw:   "token",
		patternRaw:    `^[^@]+@example\.com$`,
		formatRaw:     "email",
		literalRaw:    "private@example.com",
		defaultRaw:    "default-private-value",
		annotationRaw: "annotation-private-value",
	}
	capabilityValue, scope, broker, closeAttempt := newRequestValidationSecretHarness(t, 308, values)
	defer closeAttempt()
	p := &Plugin{config: Config{BodySchema: map[string]any{
		"type": "object",
		"$defs": map[string]any{
			"token": map[string]any{
				"type": typeRaw, "pattern": patternRaw, "format": formatRaw,
				"enum": []any{literalRaw}, "const": literalRaw,
				"default": defaultRaw, "description": annotationRaw,
				"examples": []any{literalRaw},
			},
		},
		"properties": map[string]any{
			"token":        map[string]any{"$ref": refRaw},
			mapKeyEnvelope: map[string]any{"type": "string"},
		},
		"required": []any{requiredRaw},
		"title":    annotationRaw,
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

	calls := broker.callsSnapshot()
	if len(calls) != 11 {
		t.Fatalf("terminal secret materializations = %d, want 11: %#v", len(calls), calls)
	}
	for _, call := range calls {
		if call.raw == mapKeyEnvelope {
			t.Fatalf("schema map key was materialized: %#v", call)
		}
	}
	properties := p.config.BodySchema["properties"].(map[string]any)
	if _, ok := properties[mapKeyEnvelope]; !ok {
		t.Fatalf("schema map key changed: %#v", properties)
	}

	valid := performRequest(
		p,
		http.MethodPost,
		"/",
		`{"token":"private@example.com"}`,
		map[string]string{"Content-Type": "application/json"},
	)
	if valid.Code != http.StatusNoContent {
		t.Fatalf("terminal-role schema response = %d/%q", valid.Code, valid.Body.String())
	}
	p.Stop()
}

func TestScopedSecretsMaterializeRecursiveHeaderAndBodySchemaValues(t *testing.T) {
	const (
		headerRaw = "$ENV://REQUEST_VALIDATION_HEADER_ENUM"
		bodyRaw   = "$secret://vault/request-validation/body-token"
		header    = "private-header-value"
		body      = "private-body-value"
	)
	capabilityValue, scope, broker, closeAttempt := newRequestValidationSecretHarness(
		t, 301, map[string]string{headerRaw: header, bodyRaw: body},
	)
	defer closeAttempt()
	p := &Plugin{config: Config{
		HeaderSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				headerRaw: map[string]any{"type": "string"},
				"x-private": map[string]any{
					"type": "string", "enum": []any{headerRaw},
				},
			},
		},
		BodySchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"token": map[string]any{"type": "string", "const": bodyRaw},
			},
		},
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}

	calls := broker.callsSnapshot()
	if len(calls) != 2 {
		t.Fatalf("broker calls = %#v, want two recursive string values", calls)
	}
	for index, want := range []requestValidationSecretCall{
		{scope: scope, raw: headerRaw},
		{scope: scope, raw: bodyRaw},
	} {
		want.scope.Field = []string{"header_schema", "body_schema"}[index]
		if calls[index] != want {
			t.Fatalf("broker call[%d] = %#v, want %#v", index, calls[index], want)
		}
	}
	publicConfig := fmt.Sprintf("%#v", p.Config())
	for _, forbidden := range []string{bodyRaw, header, body} {
		if strings.Contains(publicConfig, forbidden) {
			t.Fatalf("public config leaked %q: %s", forbidden, publicConfig)
		}
	}
	properties := p.config.HeaderSchema["properties"].(map[string]any)
	if _, ok := properties[headerRaw]; !ok {
		t.Fatalf("secret-looking JSON Schema property name was rewritten: %#v", properties)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	valid := performRequest(
		p, http.MethodPost, "http://example.com/validate", `{"token":"`+body+`"}`,
		map[string]string{"Content-Type": "application/json", "X-Private": header},
	)
	if valid.Code != http.StatusNoContent {
		t.Fatalf("valid secret-backed request = %d/%q", valid.Code, valid.Body.String())
	}
	invalid := performRequest(
		p, http.MethodPost, "http://example.com/validate", `{"token":"wrong"}`,
		map[string]string{"Content-Type": "application/json", "X-Private": header},
	)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid secret-backed request = %d/%q", invalid.Code, invalid.Body.String())
	}
	for _, forbidden := range []string{header, body} {
		if strings.Contains(invalid.Body.String(), forbidden) {
			t.Fatalf("validation diagnostic leaked %q: %q", forbidden, invalid.Body.String())
		}
	}
	if got := strings.TrimSpace(invalid.Body.String()); got != "request does not match schema" {
		t.Fatalf("sensitive body diagnostic = %q", got)
	}
	invalid = performRequest(
		p, http.MethodPost, "http://example.com/validate", `{"token":"`+body+`"}`,
		map[string]string{"Content-Type": "application/json", "X-Private": "wrong"},
	)
	if got := strings.TrimSpace(invalid.Body.String()); invalid.Code != http.StatusBadRequest ||
		got != "request does not match schema" {
		t.Fatalf("sensitive header rejection = %d/%q", invalid.Code, got)
	}
	p.Stop()
}

func TestScopedSecretFailureIsAtomicAndRetryable(t *testing.T) {
	const (
		headerRaw = "$ENV://REQUEST_VALIDATION_RETRY_HEADER"
		bodyRaw   = "$secret://vault/request-validation/retry-body"
		header    = "retry-private-header"
		body      = "retry-private-body"
	)
	capabilityValue, scope, broker, closeAttempt := newRequestValidationSecretHarness(
		t, 302, map[string]string{headerRaw: header, bodyRaw: body},
	)
	defer closeAttempt()
	broker.setFailure(bodyRaw, errors.New("backend failed with "+body))
	p := &Plugin{config: Config{
		HeaderSchema: map[string]any{"type": "object", "properties": map[string]any{
			"x-private": map[string]any{"const": headerRaw},
		}},
		BodySchema: map[string]any{"type": "object", "properties": map[string]any{
			"token": map[string]any{"const": bodyRaw},
		}},
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
	if !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("first materialization error = %v", err)
	}
	for _, forbidden := range []string{headerRaw, bodyRaw, header, body} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("materialization error leaked %q: %v", forbidden, err)
		}
	}
	if p.config.HeaderSchema["properties"].(map[string]any)["x-private"].(map[string]any)["const"] != headerRaw ||
		p.config.BodySchema["properties"].(map[string]any)["token"].(map[string]any)["const"] != bodyRaw {
		t.Fatalf("failed materialization changed config: %#v", p.config)
	}
	postInitErr := p.PostInit()
	if !errors.Is(postInitErr, secret.ErrCredentialUnavailable) {
		t.Fatalf("PostInit() after failed materialization = %v", postInitErr)
	}
	for _, forbidden := range []string{headerRaw, bodyRaw, header, body} {
		if strings.Contains(postInitErr.Error(), forbidden) {
			t.Fatalf("PostInit() error leaked %q: %v", forbidden, postInitErr)
		}
	}
	unavailable := performRequest(
		p, http.MethodPost, "/", `{"token":"`+body+`"}`,
		map[string]string{"Content-Type": "application/json", "X-Private": header},
	)
	if unavailable.Code != http.StatusServiceUnavailable {
		t.Fatalf(
			"handler after failed preparation = %d/%q, want 503",
			unavailable.Code, unavailable.Body.String(),
		)
	}
	for _, forbidden := range []string{headerRaw, bodyRaw, header, body} {
		if strings.Contains(unavailable.Body.String(), forbidden) {
			t.Fatalf("unavailable response leaked %q: %q", forbidden, unavailable.Body.String())
		}
	}

	broker.setFailure(bodyRaw, nil)
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("retry materialization error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() after retry error = %v", err)
	}
	valid := performRequest(p, http.MethodPost, "/", `{"token":"`+body+`"}`, map[string]string{
		"Content-Type": "application/json", "X-Private": header,
	})
	if valid.Code != http.StatusNoContent {
		t.Fatalf("retried plugin response = %d/%q", valid.Code, valid.Body.String())
	}
	p.Stop()
}

func TestScopedSecretsRotateWithGenerationAuthority(t *testing.T) {
	const raw = "$ENV://REQUEST_VALIDATION_ROTATED_VALUE"
	prepare := func(
		revision uint64, plaintext string,
	) (*Plugin, *requestValidationSecretBroker, func()) {
		capabilityValue, scope, broker, closeAttempt := newRequestValidationSecretHarness(
			t, revision, map[string]string{raw: plaintext},
		)
		p := &Plugin{config: Config{BodySchema: map[string]any{
			"type": "object", "properties": map[string]any{
				"token": map[string]any{"const": raw},
			},
		}}}
		if err := p.Init(); err != nil {
			t.Fatal(err)
		}
		if err := base.MaterializeScopedPluginSecrets(
			context.Background(), scope, capabilityValue, p,
		); err != nil {
			t.Fatalf("generation %d materialization: %v", revision, err)
		}
		if err := p.PostInit(); err != nil {
			t.Fatalf("generation %d PostInit: %v", revision, err)
		}
		return p, broker, closeAttempt
	}
	first, firstBroker, closeFirst := prepare(303, "first-generation-private")
	second, secondBroker, closeSecond := prepare(304, "second-generation-private")
	defer func() {
		second.Stop()
		closeSecond()
	}()

	assertBody := func(p *Plugin, body string, want int) {
		t.Helper()
		response := performRequest(p, http.MethodPost, "/", `{"token":"`+body+`"}`, map[string]string{
			"Content-Type": "application/json",
		})
		if response.Code != want {
			t.Fatalf("body %q response = %d/%q, want %d", body, response.Code, response.Body.String(), want)
		}
	}
	assertBody(first, "first-generation-private", http.StatusNoContent)
	assertBody(second, "first-generation-private", http.StatusBadRequest)
	assertBody(second, "second-generation-private", http.StatusNoContent)

	first.Stop()
	closeFirst()
	if firstBroker.revokeCount() != 1 {
		t.Fatalf("first generation revokes = %d, want 1", firstBroker.revokeCount())
	}
	assertBody(second, "second-generation-private", http.StatusNoContent)
	if secondBroker.revokeCount() != 0 {
		t.Fatalf("second generation revoked while active: %d", secondBroker.revokeCount())
	}
}

func TestStopDrainsActiveValidationAndRetiresSecrets(t *testing.T) {
	const (
		raw       = "$ENV://REQUEST_VALIDATION_STOP_VALUE"
		plaintext = "stop-private-value"
	)
	capabilityValue, scope, broker, closeAttempt := newRequestValidationSecretHarness(
		t, 305, map[string]string{raw: plaintext},
	)
	p := &Plugin{config: Config{HeaderSchema: map[string]any{
		"type": "object", "properties": map[string]any{
			"x-private": map[string]any{"const": raw},
		},
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

	entered := make(chan struct{})
	releaseRequest := make(chan struct{})
	requestDone := make(chan struct{})
	handler := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-releaseRequest
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptestNewRequestWithHeaders(t, map[string]string{"X-Private": plaintext})
	go func() {
		handler.ServeHTTP(newDiscardingResponseWriter(), request)
		close(requestDone)
	}()
	<-entered
	stopStarted := make(chan struct{})
	stopDone := make(chan struct{})
	go func() {
		close(stopStarted)
		p.Stop()
		close(stopDone)
	}()
	<-stopStarted
	select {
	case <-stopDone:
		t.Fatal("Stop returned before active validation request drained")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseRequest)
	select {
	case <-requestDone:
	case <-time.After(2 * time.Second):
		t.Fatal("active validation request did not finish")
	}
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not finish after request drained")
	}
	p.secrets.mu.Lock()
	retainedSecrets := len(p.secrets.headerSecrets) + len(p.secrets.bodySecrets)
	retainedHeaderCompiled := p.config.headerSchema
	retainedBodyCompiled := p.config.bodySchema
	p.secrets.mu.Unlock()
	if retainedSecrets != 0 || retainedHeaderCompiled != nil || retainedBodyCompiled != nil {
		t.Fatalf(
			"Stop retained secret-owned state: values=%d header-compiled=%v body-compiled=%v",
			retainedSecrets,
			retainedHeaderCompiled != nil,
			retainedBodyCompiled != nil,
		)
	}
	if config := fmt.Sprintf("%#v", p.Config()); strings.Contains(config, plaintext) {
		t.Fatalf("Stop retained recoverable plaintext in plugin config: %s", config)
	}

	postStop := performRequest(p, http.MethodGet, "/", "", map[string]string{"X-Private": plaintext})
	if postStop.Code != http.StatusServiceUnavailable || strings.Contains(postStop.Body.String(), plaintext) {
		t.Fatalf("post-Stop response = %d/%q", postStop.Code, postStop.Body.String())
	}
	closeAttempt()
	if broker.revokeCount() != 1 {
		t.Fatalf("attempt revokes = %d, want 1", broker.revokeCount())
	}
	p.Stop()
}

func TestResolvedInvalidSchemaFailsClosedWithoutPlaintextDiagnostic(t *testing.T) {
	const (
		raw       = "$secret://vault/request-validation/invalid-type"
		plaintext = "private-invalid-schema-type"
	)
	capabilityValue, scope, _, closeAttempt := newRequestValidationSecretHarness(
		t, 306, map[string]string{raw: plaintext},
	)
	defer closeAttempt()
	p := &Plugin{config: Config{BodySchema: map[string]any{"type": raw}}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatal(err)
	}
	err := p.PostInit()
	if !errors.Is(err, secret.ErrCredentialUnavailable) {
		t.Fatalf("PostInit() error = %v, want credential unavailable", err)
	}
	for _, forbidden := range []string{raw, plaintext} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("PostInit() error leaked %q: %v", forbidden, err)
		}
	}
	p.Stop()
}

type discardingResponseWriter struct {
	header http.Header
	status int
}

func newDiscardingResponseWriter() *discardingResponseWriter {
	return &discardingResponseWriter{header: make(http.Header)}
}

func (writer *discardingResponseWriter) Header() http.Header { return writer.header }

func (writer *discardingResponseWriter) Write(data []byte) (int, error) {
	if writer.status == 0 {
		writer.status = http.StatusOK
	}
	return len(data), nil
}

func (writer *discardingResponseWriter) WriteHeader(status int) { writer.status = status }

func httptestNewRequestWithHeaders(t *testing.T, headers map[string]string) *http.Request {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	return request
}
