package response_rewrite

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	brotlienc "github.com/andybalholm/brotli"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/json"
	"github.com/wklken/apisix-go/pkg/plugin/ai_runtime"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/body_transformer"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
)

type responseRewriteScopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type responseRewriteScopedSecretBroker struct {
	mu     sync.Mutex
	values map[string]string
	fail   map[string]error
	calls  []responseRewriteScopedSecretCall
}

func (*responseRewriteScopedSecretBroker) AuthorizeCandidate(
	context.Context, secret.AttemptID, generation.ApplyTicket, generation.PublicationSet,
) error {
	return nil
}

func (*responseRewriteScopedSecretBroker) AuthorizeRecovery(
	context.Context, secret.AttemptID, generation.RevisionSet,
	map[generation.Domain]generation.PublishedGeneration,
) error {
	return errors.New("recovery is not used by this leaf fixture")
}

func (broker *responseRewriteScopedSecretBroker) ResolveScoped(
	ctx context.Context, scope secret.Scope, raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, responseRewriteScopedSecretCall{Scope: scope, Raw: raw})
	if err := broker.fail[raw]; err != nil {
		return "", err
	}
	if value, ok := broker.values[raw]; ok {
		return value, nil
	}
	return raw, nil
}

func (*responseRewriteScopedSecretBroker) RevokeAttempt(context.Context, secret.AttemptID) error {
	return nil
}

func newResponseRewriteScopedSecretHarness(
	t *testing.T, revision uint64, resourceID string, rawConfig map[string]any, values map[string]string,
) (secret.GenerationCapability, secret.Scope, *responseRewriteScopedSecretBroker, func()) {
	t.Helper()
	key := generation.ResourceKey{Kind: "routes", ID: resourceID}
	resourceJSON, err := json.Marshal(map[string]any{
		"plugins": map[string]any{name: rawConfig},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := generation.NewSnapshot(revision, []generation.Resource{{
		Key: key, Value: resourceJSON,
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
			Key: key, Disposition: generation.DispositionPublished, Code: "response-rewrite-test",
		}},
	}
	ticket := generation.ApplyTicket{
		DesiredRevision: revision, RequiredDomains: []generation.Domain{generation.DomainHTTP},
	}
	set := generation.PublicationSet{
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
	broker := &responseRewriteScopedSecretBroker{
		values: values,
		fail:   make(map[string]error),
	}
	registration, err := secret.NewScopedMaterializer(broker, catalog).
		RegisterCandidate(context.Background(), ticket, set)
	if err != nil {
		t.Fatal(err)
	}
	capabilityValue, err := secret.NewGenerationCapability(registration, revision)
	if err != nil {
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
			t.Fatalf("close scoped secret registration: %v", err)
		}
	}
}

func assertResponseRewriteDescriptorFor(t *testing.T, value, plaintext string) {
	t.Helper()
	digest := sha256.Sum256([]byte(plaintext))
	want := "plugin_config#sha256:" + hex.EncodeToString(digest[:])
	if value != want {
		t.Fatalf("response-rewrite descriptor = %q, want admitted-plaintext descriptor %q", value, want)
	}
}

func TestMaterializeScopedSecretsOwnsResponseBodies(t *testing.T) {
	contextualBody, err := data_encryption.EncryptForContext(
		"contextual-body", "0123456789abcdef", "response-rewrite.body",
	)
	if err != nil {
		t.Fatalf("EncryptForContext(body) error = %v", err)
	}
	contextualSecret, err := data_encryption.EncryptForContext(
		"strict-secret-body", "0123456789abcdef", "response-rewrite.body_secret",
	)
	if err != nil {
		t.Fatalf("EncryptForContext(body_secret) error = %v", err)
	}
	tests := []struct {
		name       string
		field      string
		raw        string
		resolved   string
		bodySecret bool
	}{
		{name: "literal body", field: "body", raw: "literal-body", resolved: "literal-body"},
		{name: "environment body", field: "body", raw: "$ENV://RESPONSE_BODY", resolved: "environment-body"},
		{name: "resolved empty body", field: "body", raw: "$ENV://EMPTY_RESPONSE_BODY", resolved: ""},
		{name: "managed body", field: "body", raw: "$secret://vault/response-body", resolved: "managed-body"},
		{name: "contextual body", field: "body", raw: contextualBody, resolved: "contextual-body"},
		{
			name: "strict contextual body_secret", field: "body_secret",
			raw: contextualSecret, resolved: "strict-secret-body", bodySecret: true,
		},
	}
	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rawConfig := map[string]any{tt.field: tt.raw}
			capabilityValue, scope, broker, closeAttempt := newResponseRewriteScopedSecretHarness(
				t, uint64(index+1), fmt.Sprintf("response-route-%d", index), rawConfig,
				map[string]string{tt.raw: tt.resolved},
			)
			defer closeAttempt()

			p := &Plugin{}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}
			if err := util.Parse(rawConfig, p.Config()); err != nil {
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

			if len(broker.calls) != 1 {
				t.Fatalf("scoped calls = %#v, want one exact %s call", broker.calls, tt.field)
			}
			wantScope := scope
			wantScope.Field = tt.field
			if call := broker.calls[0]; call.Raw != tt.raw || call.Scope != wantScope {
				t.Fatalf("scoped call = %#v, want raw %q and scope %#v", call, tt.raw, wantScope)
			}
			cfg := p.Config().(*Config)
			if tt.bodySecret {
				if cfg.Body != nil {
					t.Fatalf("public body = %q, want nil instead of resolved body_secret", *cfg.Body)
				}
				if cfg.BodySecret == nil {
					t.Fatal("public body_secret = nil, want descriptor")
				}
				assertResponseRewriteDescriptorFor(t, *cfg.BodySecret, tt.resolved)
			} else {
				if cfg.Body == nil {
					t.Fatal("public body = nil, want descriptor")
				}
				assertResponseRewriteDescriptorFor(t, *cfg.Body, tt.resolved)
				if cfg.BodySecret != nil {
					t.Fatalf("public body_secret = %q, want nil", *cfg.BodySecret)
				}
			}

			req := httptest.NewRequest(http.MethodGet, "/rewrite", nil)
			response := httptest.NewRecorder()
			chain := p.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("upstream"))
			}))
			var (
				retained  *base.BufferedResponseWriter
				bodyAlias []byte
			)
			capture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				chain.ServeHTTP(w, r)
				retained = base.GetOrCreateTransformResponseWriter(r)
				bodyAlias = retained.Body()
				if got := string(bodyAlias); got != tt.resolved {
					t.Fatalf("shared pipeline body before owner return = %q, want %q", got, tt.resolved)
				}
			})
			base.WithTransformPipeline(2)(capture).ServeHTTP(response, req)
			if got := response.Body.String(); got != tt.resolved {
				t.Fatalf("response body = %q, want admitted body %q", got, tt.resolved)
			}
			if retained == nil {
				t.Fatal("retained buffered response writer = nil")
			}
			if len(retained.Body()) != 0 {
				t.Fatalf("retained buffered response body = %q, want empty", retained.Body())
			}
			for index, value := range bodyAlias {
				if value != 0 {
					t.Fatalf("retained backing byte %d = %d, want zero", index, value)
				}
			}
		})
	}

	t.Run("empty optional body", func(t *testing.T) {
		rawConfig := map[string]any{"body": ""}
		capabilityValue, scope, broker, closeAttempt := newResponseRewriteScopedSecretHarness(
			t, 20, "response-empty", rawConfig, nil,
		)
		defer closeAttempt()
		p := &Plugin{}
		if err := p.Init(); err != nil {
			t.Fatal(err)
		}
		if err := util.Parse(rawConfig, p.Config()); err != nil {
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
		if len(broker.calls) != 0 {
			t.Fatalf("empty optional body materialization calls = %#v, want none", broker.calls)
		}
		response := performRequest(p, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("upstream"))
		})
		if response.Body.Len() != 0 {
			t.Fatalf("empty body response = %q, want empty replacement", response.Body.String())
		}
	})

	t.Run("body and body_secret conflict before resolver", func(t *testing.T) {
		rawConfig := map[string]any{"body": "plain", "body_secret": contextualSecret}
		capabilityValue, scope, broker, closeAttempt := newResponseRewriteScopedSecretHarness(
			t, 21, "response-conflict", rawConfig,
			map[string]string{"plain": "plain", contextualSecret: "strict-secret-body"},
		)
		defer closeAttempt()
		p := &Plugin{}
		if err := p.Init(); err != nil {
			t.Fatal(err)
		}
		if err := util.Parse(rawConfig, p.Config()); err != nil {
			t.Fatal(err)
		}
		validator, ok := any(p).(interface{ ValidatePreMaterialization() error })
		if !ok {
			t.Fatal("response-rewrite does not implement pre-materialization validation")
		}
		if err := validator.ValidatePreMaterialization(); err == nil ||
			err.Error() != "response-rewrite body and body_secret cannot be configured together" {
			t.Fatalf("ValidatePreMaterialization() error = %v, want body/body_secret conflict", err)
		}
		if err := base.MaterializeScopedPluginSecrets(
			context.Background(), scope, capabilityValue, p,
		); err == nil {
			t.Fatal("MaterializeScopedPluginSecrets() error = nil, want conflict rejection")
		}
		if len(broker.calls) != 0 {
			t.Fatalf("conflict resolver calls = %#v, want zero", broker.calls)
		}
	})
}

func TestResponseRewritePreservesInnerBodyAcrossTransformPipeline(t *testing.T) {
	inner := newTestPlugin(t, Config{Body: new("inner-body")})
	outer := newTestPlugin(t, Config{
		StatusCode: http.StatusAccepted,
		Headers:    Headers{Set: map[string]string{"X-Outer": "yes"}},
	})
	handler := base.WithTransformPipeline(2)(outer.Handler(inner.Handler(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("upstream"))
		},
	))))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/pipeline", nil))
	if response.Code != http.StatusAccepted || response.Body.String() != "inner-body" {
		t.Fatalf("pipeline response = %d/%q, want 202/inner-body", response.Code, response.Body.String())
	}
	if got := response.Header().Get("X-Outer"); got != "yes" {
		t.Fatalf("pipeline X-Outer = %q, want yes", got)
	}
}

func TestTransformPipelineClearsLongAdmittedBodyAfterShorterOuterTransform(t *testing.T) {
	const (
		raw      = "$ENV://PRIVATE_RESPONSE_BODY"
		resolved = "admitted-private-response-with-a-long-tail"
		final    = "short"
	)
	rawConfig := map[string]any{
		"body":        raw,
		"status_code": http.StatusAccepted,
		"headers": map[string]any{
			"set": map[string]any{"X-Rewritten": "yes"},
		},
	}
	capabilityValue, scope, _, closeAttempt := newResponseRewriteScopedSecretHarness(
		t, 60, "response-identity-pipeline", rawConfig, map[string]string{raw: resolved},
	)
	defer closeAttempt()
	rewrite := &Plugin{}
	if err := rewrite.Init(); err != nil {
		t.Fatal(err)
	}
	if err := util.Parse(rawConfig, rewrite.Config()); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, rewrite,
	); err != nil {
		t.Fatal(err)
	}
	if err := rewrite.PostInit(); err != nil {
		t.Fatal(err)
	}

	outer := &body_transformer.Plugin{}
	if err := outer.Init(); err != nil {
		t.Fatal(err)
	}
	if err := util.Parse(map[string]any{
		"response": map[string]any{
			"input_format": "plain",
			"template":     final,
		},
	}, outer.Config()); err != nil {
		t.Fatal(err)
	}
	if err := outer.PostInit(); err != nil {
		t.Fatal(err)
	}

	terminal := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "8")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("upstream"))
	})
	var (
		shared    *base.BufferedResponseWriter
		bodyAlias []byte
	)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rewrite.Handler(terminal).ServeHTTP(w, r)
		shared = base.GetOrCreateTransformResponseWriter(r)
		bodyAlias = shared.Body()
		if got := string(bodyAlias); got != resolved {
			t.Fatalf("shared pipeline body before outer transform = %q, want %q", got, resolved)
		}
	})
	chain := outer.Handler(inner)
	capture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chain.ServeHTTP(w, r)
		if got := string(shared.Body()); got != final {
			t.Fatalf("shared pipeline body before owner return = %q, want %q", got, final)
		}
	})
	handler := base.WithTransformPipeline(2)(capture)
	request := httptest.NewRequest(http.MethodGet, "/shorter-outer-transform", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted || response.Body.String() != final {
		t.Fatalf("final response = %d/%q, want 202/%q", response.Code, response.Body.String(), final)
	}
	if got := response.Header().Get("X-Rewritten"); got != "yes" {
		t.Fatalf("X-Rewritten = %q, want yes", got)
	}
	if got := response.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want invalidated", got)
	}
	if shared == nil {
		t.Fatal("shared pipeline writer = nil")
	}
	if len(shared.Body()) != 0 {
		t.Fatalf("shared pipeline after ServeHTTP = %#v/%q, want empty", shared, shared.Body())
	}
	for index, value := range bodyAlias {
		if value != 0 {
			t.Fatalf("shared pipeline backing byte %d = %d, want zero", index, value)
		}
	}
}

func materializeResponseRewriteScopedPlugin(
	t *testing.T, revision uint64, resourceID, raw, resolved string,
) (*Plugin, *responseRewriteScopedSecretBroker, func()) {
	t.Helper()
	rawConfig := map[string]any{"body": raw}
	capabilityValue, scope, broker, closeAttempt := newResponseRewriteScopedSecretHarness(
		t, revision, resourceID, rawConfig, map[string]string{raw: resolved},
	)
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := util.Parse(rawConfig, p.Config()); err != nil {
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
	return p, broker, closeAttempt
}

func TestMaterializeScopedSecretsFailureIsAtomicAndRetryable(t *testing.T) {
	const raw = "$secret://vault/response-failure"
	rawConfig := map[string]any{"body": raw}
	capabilityValue, scope, broker, closeAttempt := newResponseRewriteScopedSecretHarness(
		t, 30, "response-failure", rawConfig, map[string]string{raw: "private-response"},
	)
	defer closeAttempt()
	broker.fail[raw] = errors.New("private resolver failure")
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := util.Parse(rawConfig, p.Config()); err != nil {
		t.Fatal(err)
	}
	err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p)
	if err == nil {
		t.Fatal("MaterializeScopedPluginSecrets() error = nil")
	}
	if strings.Contains(err.Error(), raw) || strings.Contains(err.Error(), "private-response") ||
		strings.Contains(err.Error(), "private resolver") {
		t.Fatalf("materialization error leaked body details: %v", err)
	}
	if p.config.Body == nil || *p.config.Body != raw || p.body != nil {
		t.Fatalf(
			"failed materialization retained state: config=%#v scoped=%#v",
			p.config.Body, p.body,
		)
	}

	broker.mu.Lock()
	delete(broker.fail, raw)
	broker.mu.Unlock()
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("retry materialization error = %v", err)
	}
	assertResponseRewriteDescriptorFor(t, *p.config.Body, "private-response")
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() after retry error = %v", err)
	}
}

func TestMaterializeScopedSecretsBase64FailureIsAtomicAndRetryable(t *testing.T) {
	const raw = "$ENV://RESPONSE_BASE64"
	rawConfig := map[string]any{"body": raw, "body_base64": true}
	capabilityValue, scope, broker, closeAttempt := newResponseRewriteScopedSecretHarness(
		t, 31, "response-base64", rawConfig, map[string]string{raw: "not-base64"},
	)
	defer closeAttempt()
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := util.Parse(rawConfig, p.Config()); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err == nil {
		t.Fatal("invalid admitted base64 materialized successfully")
	}
	if p.config.Body == nil || *p.config.Body != raw || p.body != nil {
		t.Fatalf("failed base64 materialization retained state: config=%#v scoped=%#v", p.config.Body, p.body)
	}

	broker.mu.Lock()
	broker.values[raw] = base64.StdEncoding.EncodeToString([]byte("retried-body"))
	broker.mu.Unlock()
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, capabilityValue, p,
	); err != nil {
		t.Fatalf("base64 retry materialization error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() after base64 retry error = %v", err)
	}
	response := performRequest(p, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("upstream"))
	})
	if got := response.Body.String(); got != "retried-body" {
		t.Fatalf("retried response body = %q, want retried-body", got)
	}
}

func TestResponseBodyRotationDoesNotCrossGenerationsAndStopIsRepeatable(t *testing.T) {
	pN, _, closeN := materializeResponseRewriteScopedPlugin(
		t, 40, "response-n", "$ENV://RESPONSE_N", "body-n",
	)
	defer closeN()
	pNPlusOne, _, closeNPlusOne := materializeResponseRewriteScopedPlugin(
		t, 41, "response-n-plus-one", "$ENV://RESPONSE_N_PLUS_ONE", "body-n-plus-one",
	)
	defer closeNPlusOne()

	rewrite := func(p *Plugin) *httptest.ResponseRecorder {
		t.Helper()
		return performRequest(p, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("upstream"))
		})
	}
	if got := rewrite(pN).Body.String(); got != "body-n" {
		t.Fatalf("generation N body = %q", got)
	}
	if got := rewrite(pNPlusOne).Body.String(); got != "body-n-plus-one" {
		t.Fatalf("generation N+1 body = %q", got)
	}

	owned := pN.body
	pN.Stop()
	pN.Stop()
	if pN.body != nil || !pN.stopped {
		t.Fatalf(
			"generation N retained body state: scoped=%#v stopped=%v",
			pN.body,
			pN.stopped,
		)
	}
	if owned == nil || *owned != (secret.Value{}) {
		t.Fatalf("generation N scoped owner after Stop = %#v, want zero", owned)
	}
	retired := rewrite(pN)
	if retired.Code != http.StatusInternalServerError || strings.Contains(retired.Body.String(), "body-n") {
		t.Fatalf("retired generation response = %d/%q, want redacted 500", retired.Code, retired.Body.String())
	}
	if got := rewrite(pNPlusOne).Body.String(); got != "body-n-plus-one" {
		t.Fatalf("generation N+1 body after N retirement = %q", got)
	}
}

func TestResponseBodyUseBlocksStopUntilResponseWriteFinishes(t *testing.T) {
	p, _, closeAttempt := materializeResponseRewriteScopedPlugin(
		t, 50, "response-stop-barrier", "$ENV://RESPONSE_BARRIER", "barrier-body",
	)
	defer closeAttempt()
	owned := p.body
	response := base.NewBufferedResponseWriter()
	entered := make(chan struct{})
	release := make(chan struct{})
	writeDone := make(chan struct{})
	go func() {
		_, err := p.useBody(func(body string) error {
			response.ReplaceBody([]byte(body))
			close(entered)
			<-release
			response.SetBody(nil)
			return nil
		})
		if err != nil {
			t.Errorf("useBody() error = %v", err)
		}
		close(writeDone)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for response body write")
	}

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
		t.Fatal("Stop returned while response write held the body read lock")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-writeDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for response write release")
	}
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Stop after response write")
	}
	if len(response.Body()) != 0 {
		t.Fatalf("retained buffered response body = %q, want cleared", response.Body())
	}
	if p.body != nil || owned == nil || *owned != (secret.Value{}) {
		t.Fatalf("stopped body ownership = current:%#v saved:%#v", p.body, owned)
	}
	p.Stop()
}

func TestResponseRewriteRunsOneAtomicBufferedBodyCallback(t *testing.T) {
	plugin := newTestPlugin(t, Config{
		StatusCode: http.StatusAccepted,
		Body:       new("rewritten"),
		Headers:    Headers{Set: map[string]string{"X-Rewritten": "yes"}},
	})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req = apisixctx.WithRequestVars(req)
	apisixctx.SetRequestResponseSource(req, apisixctx.ResponseSourceAPISIX)
	state := base.ResponseState{
		Status: http.StatusOK,
		Header: http.Header{"Content-Length": {"8"}, "X-Original": {"yes"}},
		Body:   []byte("upstream"),
	}
	if err := plugin.RunBufferedBodyFilter(req, &state); err != nil {
		t.Fatalf("RunBufferedBodyFilter() error = %v", err)
	}
	if state.Status != http.StatusAccepted || string(state.Body) != "rewritten" {
		t.Fatalf("state = %+v, want 202/rewritten", state)
	}
	if got := state.Header.Get("X-Rewritten"); got != "yes" {
		t.Fatalf("X-Rewritten = %q, want yes", got)
	}
	if got := state.Header.Get("X-Original"); got != "yes" {
		t.Fatalf("X-Original = %q, want preserved", got)
	}
	if got := state.Header.Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want invalidated", got)
	}
}

func newTestPlugin(t *testing.T, cfg Config) *Plugin {
	t.Helper()

	p := &Plugin{config: cfg}
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	capabilityValue, scope, closeAttempt := testutil.ScopedSecretHarness(
		t,
		name,
		nil,
		generation.ApplyTicket{DesiredRevision: 1, RequiredDomains: []generation.Domain{generation.DomainHTTP}},
	)
	t.Cleanup(closeAttempt)
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, capabilityValue, p); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	return p
}

func TestResponseRewriteDescribesAndRunsPureHeaderStreamingConfig(t *testing.T) {
	plugin := newTestPlugin(t, Config{
		Headers: Headers{
			Add: []string{"Set-Cookie: session=1"},
			Set: map[string]string{
				"X-Request":  "$http_x_request",
				"X-Status":   "$status",
				"X-Upstream": "$sent_http_x_upstream",
			},
			Remove: []string{"X-Remove"},
		},
	})
	descriptor, err := plugin.Config().(base.BindingPhaseDescriber).DescribeBindingPhases()
	if err != nil {
		t.Fatalf("DescribeBindingPhases() error = %v", err)
	}
	if descriptor != (base.BindingPhaseDescriptor{RequestStage: "none", StreamingHeader: true}) {
		t.Fatalf("descriptor = %#v, want pure header descriptor", descriptor)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	req.Header.Set("X-Request", "from-request")
	state := base.StreamingResponseState{
		Status: http.StatusAccepted,
		Header: http.Header{
			"X-Remove":   {"delete-me"},
			"X-Upstream": {"upstream-value"},
		},
	}
	if err := plugin.RunStreamingHeaderFilter(req, &state); err != nil {
		t.Fatalf("RunStreamingHeaderFilter() error = %v", err)
	}
	if got := state.Header.Get("X-Request"); got != "from-request" {
		t.Fatalf("X-Request = %q, want from-request", got)
	}
	if got := state.Header.Get("X-Status"); got != "202" {
		t.Fatalf("X-Status = %q, want 202", got)
	}
	if got := state.Header.Get("X-Upstream"); got != "upstream-value" {
		t.Fatalf("X-Upstream = %q, want upstream-value", got)
	}
	if got := state.Header.Get("X-Remove"); got != "" {
		t.Fatalf("X-Remove = %q, want removed", got)
	}
	if got := state.Header.Values("Set-Cookie"); len(got) != 1 || got[0] != "session=1" {
		t.Fatalf("Set-Cookie = %v, want [session=1]", got)
	}
}

func TestResponseRewritePureHeaderSelectsAIResponseMode(t *testing.T) {
	config := Config{Headers: Headers{Set: map[string]string{"X-Mode": "$llm_model"}}}
	descriptor, err := config.DescribeResponseMode()
	if err != nil {
		t.Fatalf("DescribeResponseMode() error = %v", err)
	}
	wantModes := base.ResponseModeBounded | base.ResponseModeStreaming
	if descriptor.Modes != wantModes {
		t.Fatalf("response modes = %d, want bounded|streaming", descriptor.Modes)
	}
	p := &Plugin{config: config}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request = ai_runtime.WithExecution(request, "openai", func(http.ResponseWriter, *http.Request) {})
	if mode := p.SelectResponseMode(request); mode != base.RequestResponseModeBounded {
		t.Fatalf("non-streaming AI mode = %d, want bounded", mode)
	}
	ai_runtime.FromRequest(request).SetStreamingIntent(true)
	if mode := p.SelectResponseMode(request); mode != base.RequestResponseModeStreaming {
		t.Fatalf("streaming AI mode = %d, want streaming", mode)
	}
}

func TestResponseRewriteExclusionsRemainBuffered(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "status",
			cfg:  Config{Headers: Headers{Set: map[string]string{"X-Mode": "status"}}, StatusCode: http.StatusCreated},
		},
		{
			name: "vars",
			cfg: Config{
				Headers: Headers{Set: map[string]string{"X-Mode": "vars"}},
				Vars:    []any{[]any{"status", "==", http.StatusOK}},
			},
		},
		{name: "body", cfg: Config{Headers: Headers{Set: map[string]string{"X-Mode": "body"}}, Body: new("rewritten")}},
		{
			name: "body_secret",
			cfg: Config{
				Headers:    Headers{Set: map[string]string{"X-Mode": "body_secret"}},
				BodySecret: new("ciphertext"),
			},
		},
		{
			name: "filters",
			cfg: Config{
				Headers: Headers{Set: map[string]string{"X-Mode": "filters"}},
				Filters: []Filter{{Regex: "old", Replace: "new"}},
			},
		},
		{name: "bytes_sent", cfg: Config{Headers: Headers{Set: map[string]string{"X-Mode": "$bytes_sent"}}}},
		{name: "body_bytes_sent", cfg: Config{Headers: Headers{Set: map[string]string{"X-Mode": "$body_bytes_sent"}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			descriptor, err := test.cfg.DescribeBindingPhases()
			if err != nil {
				t.Fatalf("DescribeBindingPhases() error = %v", err)
			}
			if !descriptor.BufferedBody || descriptor.Header || descriptor.StreamingHeader {
				t.Fatalf("descriptor = %#v, want buffered body only", descriptor)
			}
		})
	}
}

func TestHandlerRewritesStatusAndBody(t *testing.T) {
	p := newTestPlugin(t, Config{
		StatusCode: 201,
		Body:       new(`{"ok":true}`),
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "8")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("upstream"))
	})

	if res.Code != http.StatusCreated {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusCreated)
	}
	if got := res.Body.String(); got != `{"ok":true}` {
		t.Fatalf("body = %q, want %q", got, `{"ok":true}`)
	}
	if got := res.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want removed after body rewrite", got)
	}
}

func TestHandlerBodyReplacementInvalidatesRepresentationHeaders(t *testing.T) {
	p := newTestPlugin(t, Config{Body: new("rewritten")})

	res := performRequest(p, func(w http.ResponseWriter, _ *http.Request) {
		setRewriteRepresentationHeaders(w.Header())
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream"))
	})

	for _, field := range rewriteRepresentationHeaders() {
		if values := res.Header().Values(field); len(values) != 0 {
			t.Errorf("%s = %v, want removed after body replacement", field, values)
		}
	}
}

func TestHandlerDecodesBase64Body(t *testing.T) {
	p := newTestPlugin(t, Config{
		Body:       new("aGVsbG8="),
		BodyBase64: new(true),
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream"))
	})

	if got := res.Body.String(); got != "hello" {
		t.Fatalf("body = %q, want hello", got)
	}
}

func TestPostInitRejectsMixedBodySecretConfiguration(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "body",
			cfg: Config{
				Body:       new("plain"),
				BodySecret: new("secret"),
			},
		},
		{
			name: "filters",
			cfg: Config{
				BodySecret: new("secret"),
				Filters:    []Filter{{Regex: "secret", Replace: "redacted"}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := &Plugin{config: test.cfg}
			p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
			if err := p.Init(); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			if err := p.ValidatePreMaterialization(); err == nil {
				t.Fatal("ValidatePreMaterialization() error = nil, want mixed body_secret rejection")
			}
		})
	}
}

func TestHandlerAppliesHeaderOperations(t *testing.T) {
	p := newTestPlugin(t, Config{
		Headers: Headers{
			Add:    []string{"Set-Cookie: a=1", "Set-Cookie: b=2"},
			Set:    map[string]string{"X-Mode": "rewritten"},
			Remove: []string{"X-Remove"},
		},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Mode", "upstream")
		w.Header().Set("X-Remove", "delete-me")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("upstream"))
	})

	if got := res.Header().Get("X-Mode"); got != "rewritten" {
		t.Fatalf("X-Mode = %q, want rewritten", got)
	}
	if got := res.Header().Get("X-Remove"); got != "" {
		t.Fatalf("X-Remove = %q, want removed", got)
	}
	if got := res.Header().Values("Set-Cookie"); len(got) != 2 || got[0] != "a=1" || got[1] != "b=2" {
		t.Fatalf("Set-Cookie values = %v, want [a=1 b=2]", got)
	}
	if got := res.Body.String(); got != "upstream" {
		t.Fatalf("body = %q, want upstream", got)
	}
}

func TestHandlerSupportsOldHeaderSetForm(t *testing.T) {
	p := newTestPlugin(t, Config{
		Headers: Headers{
			LegacySet: map[string]string{"X-Legacy": "yes"},
		},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream"))
	})

	if got := res.Header().Get("X-Legacy"); got != "yes" {
		t.Fatalf("X-Legacy = %q, want yes", got)
	}
}

func TestHandlerDeletesLegacyHeadersWithEmptyValues(t *testing.T) {
	p := newTestPlugin(t, Config{Headers: Headers{LegacySet: map[string]string{
		"Content-Type": "",
	}}})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	})

	if values := res.Header().Values("Content-Type"); len(values) != 0 {
		t.Fatalf("Content-Type = %q, want header deleted", res.Header().Get("Content-Type"))
	}
}

func TestHandlerSuppressesDeletedContentTypeOverHTTP(t *testing.T) {
	p := newTestPlugin(t, Config{
		Body: new("rewritten"),
		Headers: Headers{LegacySet: map[string]string{
			"Content-Type": "",
		}},
	})
	server := httptest.NewServer(p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("upstream"))
	})))
	defer server.Close()

	response, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatalf("GET rewritten response: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if values := response.Header.Values("Content-Type"); len(values) != 0 {
		t.Fatalf("Content-Type values = %v, want header suppressed", values)
	}
}

func TestHandlerBodyRewriteRemovesInvalidatedEntityHeaders(t *testing.T) {
	p := newTestPlugin(t, Config{Body: new("rewritten")})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "8")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
		w.Header().Set("ETag", `"upstream"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream"))
	})

	for _, name := range []string{"Content-Length", "Content-Encoding", "Last-Modified", "ETag"} {
		if values := res.Header().Values(name); len(values) != 0 {
			t.Errorf("%s values = %v, want header removed", name, values)
		}
	}
}

func TestHandlerResolvesHeaderValueVariables(t *testing.T) {
	p := newTestPlugin(t, Config{
		StatusCode: 201,
		Headers: Headers{
			Set: map[string]string{"X-Rewrite-Status": "$status"},
		},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	if got := res.Header().Get("X-Rewrite-Status"); got != "201" {
		t.Fatalf("X-Rewrite-Status = %q, want 201", got)
	}
}

func TestHandlerSkipsRewriteWhenVarsDoNotMatch(t *testing.T) {
	p := newTestPlugin(t, Config{
		StatusCode: 201,
		Body:       new("rewritten"),
		Vars:       []any{[]any{"status", "==", 404}},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream"))
	})

	if res.Code != http.StatusOK {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusOK)
	}
	if got := res.Body.String(); got != "upstream" {
		t.Fatalf("body = %q, want upstream", got)
	}
}

func TestHandlerAppliesRewriteWhenVarsMatchResponseStatus(t *testing.T) {
	p := newTestPlugin(t, Config{
		StatusCode: 202,
		Body:       new("accepted"),
		Vars:       []any{[]any{"status", "==", 404}},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("missing"))
	})

	if res.Code != http.StatusAccepted {
		t.Fatalf("response code = %d, want %d", res.Code, http.StatusAccepted)
	}
	if got := res.Body.String(); got != "accepted" {
		t.Fatalf("body = %q, want accepted", got)
	}
}

func TestHandlerAppliesResponseBodyFilters(t *testing.T) {
	p := newTestPlugin(t, Config{
		Filters: []Filter{
			{Regex: `token=\w+`, Replace: "token=hidden"},
			{Regex: `secret`, Replace: "redacted", Scope: "global"},
		},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "42")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("token=abc token=def secret secret"))
	})

	if got := res.Body.String(); got != "token=hidden token=def redacted redacted" {
		t.Fatalf("body = %q, want filtered body", got)
	}
	if got := res.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want removed after filters", got)
	}
}

func TestHandlerDecodesGzipBodyBeforeFilters(t *testing.T) {
	p := newTestPlugin(t, Config{
		Filters: []Filter{
			{Regex: `secret`, Replace: "redacted", Scope: "global"},
		},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		body := gzipBody(t, "secret token")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", "42")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})

	if got := res.Body.String(); got != "redacted token" {
		t.Fatalf("body = %q, want decoded and filtered body", got)
	}
	if got := res.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want removed after decoded filter rewrite", got)
	}
	if got := res.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want removed after decoded filter rewrite", got)
	}
}

func TestHandlerDecodesBrotliBodyBeforeFilters(t *testing.T) {
	p := newTestPlugin(t, Config{
		Filters: []Filter{
			{Regex: `secret`, Replace: "redacted", Scope: "global"},
		},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		body := brotliBody(t, "secret token")
		w.Header().Set("Content-Encoding", "br")
		w.Header().Set("Content-Length", "42")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})

	if got := res.Body.String(); got != "redacted token" {
		t.Fatalf("body = %q, want decoded and filtered body", got)
	}
	if got := res.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want removed after decoded filter rewrite", got)
	}
}

func TestHandlerClearsGzipEncodingWhenDecompressedBodyExceedsLimit(t *testing.T) {
	testOversizedEncodedBodyHeaderIsCleared(t, "gzip")
}

func TestHandlerClearsBrotliEncodingWhenDecompressedBodyExceedsLimit(t *testing.T) {
	testOversizedEncodedBodyHeaderIsCleared(t, "br")
}

func testOversizedEncodedBodyHeaderIsCleared(t *testing.T, encoding string) {
	t.Helper()
	expanded := bytes.Repeat([]byte("secret"), int(base.DefaultBufferedResponseMaxBytes/int64(len("secret")))+1)
	var encoded []byte
	if encoding == "gzip" {
		encoded = gzipBytesBody(t, expanded)
	} else {
		encoded = brotliBytesBody(t, expanded)
	}

	p := newTestPlugin(t, Config{
		Filters: []Filter{{Regex: `secret`, Replace: "redacted", Scope: "global"}},
	})
	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", encoding)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(encoded)
	})

	if got := res.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want cleared before filter dispatch", got)
	}
	if got := res.Body.Bytes(); !bytes.Equal(got, encoded) {
		t.Fatalf(
			"body changed after oversized %s decode: got %d bytes, want original %d bytes",
			encoding,
			len(got),
			len(encoded),
		)
	}
}

func TestHandlerSkipsFiltersWhenEncodedBodyCannotBeDecoded(t *testing.T) {
	tests := []struct {
		name     string
		encoding string
	}{
		{name: "unsupported encoding", encoding: "zstd"},
		{name: "invalid brotli", encoding: "br"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPlugin(t, Config{
				Filters: []Filter{{Regex: `secret`, Replace: "redacted", Scope: "global"}},
			})

			res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
				setRewriteRepresentationHeaders(w.Header())
				w.Header().Set("Content-Encoding", tt.encoding)
				w.Header().Set("Content-Length", "12")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("secret token"))
			})

			if got := res.Body.String(); got != "secret token" {
				t.Fatalf("body = %q, want encoded body left unfiltered", got)
			}
			for _, field := range rewriteRepresentationHeaders() {
				want := "stale"
				switch field {
				case "Content-Length", "Content-Encoding", "ETag", "Last-Modified":
					want = ""
				}
				if got := res.Header().Get(field); got != want {
					t.Errorf("%s = %q, want %q", field, got, want)
				}
			}
		})
	}
}

func TestHandlerWarnsWhenFiltersSeeUnsupportedEncoding(t *testing.T) {
	p := newTestPlugin(t, Config{
		Filters: []Filter{{Regex: `secret`, Replace: "redacted", Scope: "global"}},
	})

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "deflate")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("secret token"))
	})
	if got := res.Body.String(); got != "secret token" {
		t.Fatalf("body = %q, want encoded body left unfiltered", got)
	}
	if got := res.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want cleared before filter dispatch", got)
	}
}

func TestHandlerSupportsNestedRestyExpressionOperators(t *testing.T) {
	p := newTestPlugin(t, Config{
		Body: new("matched"),
		Vars: []any{
			"AND",
			[]any{"status", ">=", 200},
			[]any{"request_method", "in", []any{"GET", "HEAD"}},
			[]any{"remote_addr", "ipmatch", []any{"192.0.2.0/24"}},
			[]any{"sent_http_set_cookie", "has", "session=ok"},
			[]any{"http_x_env", "~*", "^prod$"},
			[]any{"http_x_skip", "!", "==", "yes"},
			[]any{
				"OR",
				[]any{"arg_mode", "==", "rewrite"},
				[]any{"status", "==", 500},
			},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get?mode=rewrite", nil)
	req.RemoteAddr = "192.0.2.40:12345"
	req.Header.Set("X-Env", "PrOd")
	res := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "theme=dark")
		w.Header().Add("Set-Cookie", "session=ok")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("upstream"))
	})).ServeHTTP(res, req)

	if got := res.Body.String(); got != "matched" {
		t.Fatalf("body = %q, want nested expression rewrite", got)
	}
}

func TestPostInitRejectsInvalidVarsExpression(t *testing.T) {
	tests := []struct {
		name string
		vars []any
	}{
		{name: "unknown operator", vars: []any{[]any{"status", "bogus", 200}}},
		{name: "dangling logic", vars: []any{[]any{"status", "==", 200}, "OR"}},
		{name: "consecutive logic", vars: []any{[]any{"status", "==", 200}, "OR", "AND", []any{"status", "==", 201}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &Plugin{config: Config{Vars: tt.vars}}
			p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
			if err := p.Init(); err != nil {
				t.Fatalf("Init() error = %v", err)
			}
			if err := p.PostInit(); err == nil {
				t.Fatal("PostInit() error = nil, want invalid vars expression rejected")
			}
		})
	}
}

func TestConfigAcceptsNumericHeaderValues(t *testing.T) {
	p := &Plugin{}
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := util.Parse(map[string]any{
		"headers": map[string]any{
			"set": map[string]any{"X-Retry-After": 12},
		},
	}, p.Config()); err != nil {
		t.Fatalf("Parse() numeric header error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	res := performRequest(p, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	if got := res.Header().Get("X-Retry-After"); got != "12" {
		t.Fatalf("X-Retry-After = %q, want 12", got)
	}
}

func TestSchemaValidatesOfficialHeaderForms(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	valid := []map[string]any{
		{"headers": map[string]any{"X-Retry-After": 12}},
		{"headers": map[string]any{"add": []any{"Set-Cookie: a=1"}}},
		{"headers": map[string]any{"set": map[string]any{"X-Retry-After": 12}}},
		{"headers": map[string]any{"remove": []any{"X-Legacy"}}},
	}
	for _, config := range valid {
		if err := util.Validate(config, p.GetSchema()); err != nil {
			t.Fatalf("Validate(%v) error = %v", config, err)
		}
	}

	invalid := []map[string]any{
		{"headers": map[string]any{"X-Enabled": true}},
		{"headers": map[string]any{"add": []any{}}},
		{"headers": map[string]any{"remove": []any{"Bad:Header"}}},
	}
	for _, config := range invalid {
		if err := util.Validate(config, p.GetSchema()); err == nil {
			t.Fatalf("Validate(%v) error = nil, want invalid header config rejected", config)
		}
	}
}

func TestPostInitRejectsBodyAndFiltersTogether(t *testing.T) {
	p := &Plugin{
		config: Config{
			Body:    new("body"),
			Filters: []Filter{{Regex: "old", Replace: "new"}},
		},
	}
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.ValidatePreMaterialization(); err == nil {
		t.Fatal("ValidatePreMaterialization() error = nil, want body and filters conflict")
	}
}

func performRequest(p *Plugin, upstream func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.com/get", nil)
	rr := httptest.NewRecorder()

	p.Handler(http.HandlerFunc(upstream)).ServeHTTP(rr, req)
	return rr
}

func rewriteRepresentationHeaders() []string {
	return []string{
		"Content-Length", "Content-Encoding", "Content-Range", "Content-MD5",
		"Digest", "Content-Digest", "Repr-Digest", "ETag", "Last-Modified",
	}
}

func setRewriteRepresentationHeaders(header http.Header) {
	for _, field := range rewriteRepresentationHeaders() {
		header.Set(field, "stale")
	}
}

func gzipBody(t *testing.T, value string) []byte {
	t.Helper()
	return gzipBytesBody(t, []byte(value))
}

func gzipBytesBody(t *testing.T, value []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(value); err != nil {
		t.Fatalf("write gzip body: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close gzip body: %v", err)
	}
	return buf.Bytes()
}

func brotliBody(t *testing.T, value string) []byte {
	t.Helper()
	return brotliBytesBody(t, []byte(value))
}

func brotliBytesBody(t *testing.T, value []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	writer := brotlienc.NewWriter(&buf)
	if _, err := writer.Write(value); err != nil {
		t.Fatalf("write brotli body: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close brotli body: %v", err)
	}
	return buf.Bytes()
}

func TestPostInitRejectsUnknownFilterOptionsFlag(t *testing.T) {
	p := &Plugin{
		config: Config{
			Filters: []Filter{{Regex: "hello", Replace: "HELLO", Options: "h"}},
		},
	}
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	err := p.PostInit()
	if err == nil {
		t.Fatal("PostInit() error = nil, want unknown flag rejection")
	}
	if !strings.Contains(err.Error(), `unknown flag "h"`) {
		t.Fatalf("PostInit() error = %v, want unknown flag message", err)
	}
}

func TestHandlerRemoveWinsOverAddForSameHeader(t *testing.T) {
	p := &Plugin{
		config: Config{
			Headers: Headers{
				Add:    []string{"Set-Cookie: <cookie-name>=<cookie-value>; Max-Age=<number>"},
				Set:    map[string]string{"Cache-Control": "max-age=0, must-revalidate"},
				Remove: []string{"Set-Cookie", "Cache-Control"},
			},
		},
	}
	p.SetDependencies(base.Dependencies{DataEncryption: testutil.DataEncryptionService(false, nil).Resolver()})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if err := p.PostInit(); err != nil {
		t.Fatalf("PostInit() error = %v", err)
	}

	rr := performRequest(p, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Set-Cookie", "session=1")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello world"))
	})
	if got := rr.Header().Get("Set-Cookie"); got != "" {
		t.Fatalf("Set-Cookie = %q, want removed after add", got)
	}
	if got := rr.Header().Get("Cache-Control"); got != "" {
		t.Fatalf("Cache-Control = %q, want removed after set", got)
	}
}
