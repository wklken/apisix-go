package elasticsearch_logger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elastic/go-elasticsearch/v8/esapi"
	apisixctx "github.com/wklken/apisix-go/pkg/apisix/ctx"
	apisixlog "github.com/wklken/apisix-go/pkg/apisix/log"
	"github.com/wklken/apisix-go/pkg/capability"
	"github.com/wklken/apisix-go/pkg/data_encryption"
	"github.com/wklken/apisix-go/pkg/generation"
	"github.com/wklken/apisix-go/pkg/logger"
	"github.com/wklken/apisix-go/pkg/plugin/base"
	"github.com/wklken/apisix-go/pkg/plugin/logger_batch"
	"github.com/wklken/apisix-go/pkg/runtime"
	"github.com/wklken/apisix-go/pkg/secret"
	"github.com/wklken/apisix-go/pkg/testutil"
	"github.com/wklken/apisix-go/pkg/util"
)

func TestPostInitWarnsOnlyForInsecureEndpointAddrs(t *testing.T) {
	tests := []struct {
		name     string
		tls      bool
		wantWarn bool
	}{
		{name: "http", wantWarn: true},
		{name: "https", tls: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-Elastic-Product", "Elasticsearch")
				_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			})
			var server *httptest.Server
			if test.tls {
				server = httptest.NewTLSServer(handler)
			} else {
				server = httptest.NewServer(handler)
			}
			t.Cleanup(server.Close)

			var warnings []logger.Entry
			stop := logger.ReplaceObserver(
				"elasticsearch-logger-security-warning-"+test.name,
				func(entry logger.Entry) {
					if entry.Level == "WARN" &&
						strings.Contains(entry.Message, "elasticsearch-logger endpoint_addrs") {
						warnings = append(warnings, entry)
					}
				},
			)
			defer stop()

			sslVerify := false
			p := newTestPlugin(t, Config{
				EndpointAddrs: []string{server.URL},
				Field:         FieldConfig{Index: "services"},
				SslVerify:     &sslVerify,
			})
			t.Cleanup(p.Stop)
			if got := len(warnings); got != 0 && !test.wantWarn {
				t.Fatalf("warnings = %#v, want none for TLS endpoints", warnings)
			}
			if got := len(warnings); got != 1 && test.wantWarn {
				t.Fatalf("warnings = %#v, want one insecure endpoint warning", warnings)
			}
		})
	}
}

type elasticsearchScopedSecretCall struct {
	Scope secret.Scope
	Raw   string
}

type elasticsearchScopedSecretBroker struct {
	mu     sync.Mutex
	values map[string]string
	fail   map[string]error
	calls  []elasticsearchScopedSecretCall
}

func (broker *elasticsearchScopedSecretBroker) ResolveScoped(
	ctx context.Context, scope secret.Scope, raw string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	broker.calls = append(broker.calls, elasticsearchScopedSecretCall{Scope: scope, Raw: raw})
	if err := broker.fail[raw]; err != nil {
		return "", err
	}
	if value, ok := broker.values[raw]; ok {
		return value, nil
	}
	return raw, nil
}

func newElasticsearchScopedSecretHarness(
	t *testing.T,
	revision uint64,
	resourceID string,
	rawConfig map[string]any,
	values map[string]string,
	keyring ...string,
) (secret.GenerationSecrets, secret.Scope, *elasticsearchScopedSecretBroker, func()) {
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
			Key: key, Disposition: generation.DispositionPublished, Code: "elasticsearch-test",
		}},
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
	broker := &elasticsearchScopedSecretBroker{values: values, fail: make(map[string]error)}
	materialization, err := testutil.NewSecretMaterializerWithKeyring(broker, catalog, keyring).
		PrepareGeneration(context.Background(), set)
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
			t.Fatalf("close scoped secret registration: %v", err)
		}
	}
}

func assertElasticsearchDescriptorFor(t *testing.T, value, plaintext string) {
	t.Helper()
	digest := sha256.Sum256([]byte(plaintext))
	want := "plugin_config#sha256:" + hex.EncodeToString(digest[:])
	if value != want {
		t.Fatalf("elasticsearch descriptor = %q, want %q", value, want)
	}
}

func TestMaterializeScopedSecretsOwnsElasticsearchCredentials(t *testing.T) {
	contextual, err := data_encryption.EncryptForContext(
		"contextual-password", "0123456789abcdef", name+".auth.password",
	)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name          string
		passwordRaw   string
		password      string
		authorization *string
		resolvedAuth  string
		lowerAuth     string
		wantCalls     []string
	}{
		{
			name:        "literal and absent optional",
			passwordRaw: "literal-password",
			password:    "literal-password",
			wantCalls:   nil,
		},
		{
			name:        "contextual ciphertext",
			passwordRaw: contextual,
			password:    "contextual-password",
			wantCalls:   nil,
		},
		{
			name:          "environment and exact Authorization",
			passwordRaw:   "$ENV://ES_PASSWORD",
			password:      "environment-password",
			authorization: new("$secret://vault/es-token"),
			resolvedAuth:  "Bearer managed",
			wantCalls:     []string{"auth.password", "headers.Authorization"},
		},
		{
			name:        "lowercase authorization is ordinary",
			passwordRaw: "$secret://vault/es-password",
			password:    "managed-password",
			lowerAuth:   "ordinary-lowercase",
			wantCalls:   []string{"auth.password"},
		},
	}
	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := map[string]string{"X-Cluster": "logs"}
			if tt.authorization != nil {
				headers["Authorization"] = *tt.authorization
			}
			if tt.lowerAuth != "" {
				headers["authorization"] = tt.lowerAuth
			}
			raw := map[string]any{
				"auth":    map[string]any{"username": "elastic", "password": tt.passwordRaw},
				"headers": headers,
			}
			values := map[string]string{tt.passwordRaw: tt.password}
			if tt.authorization != nil {
				values[*tt.authorization] = tt.resolvedAuth
			}
			secrets, scope, broker, closeAttempt := newElasticsearchScopedSecretHarness(
				t,
				uint64(index+1),
				"es-route",
				raw,
				values,
				"0123456789abcdef",
			)
			defer closeAttempt()
			p := &Plugin{config: Config{
				EndpointAddrs: []string{"http://127.0.0.1:9200"},
				Field:         FieldConfig{Index: "logs"},
				LogFormat: map[string]string{
					"request_id": "$request_id",
				},
				Auth:    &AuthConfig{Username: "elastic", Password: tt.passwordRaw},
				Headers: headers,
			}}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}
			if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
				t.Fatal(err)
			}
			if len(broker.calls) != len(tt.wantCalls) {
				t.Fatalf("resolver calls = %#v, want %v", broker.calls, tt.wantCalls)
			}
			for i, field := range tt.wantCalls {
				wantScope := scope
				wantScope.Field = field
				if broker.calls[i].Scope != wantScope {
					t.Fatalf(
						"resolver call %d scope = %#v, want %#v",
						i,
						broker.calls[i].Scope,
						wantScope,
					)
				}
			}
			assertElasticsearchDescriptorFor(t, p.config.Auth.Password, tt.password)
			if tt.authorization != nil {
				assertElasticsearchDescriptorFor(
					t,
					p.config.Headers["Authorization"],
					tt.resolvedAuth,
				)
			}
			if tt.lowerAuth != "" && p.config.Headers["authorization"] != tt.lowerAuth {
				t.Fatalf(
					"lowercase authorization = %q, want ordinary header unchanged",
					p.config.Headers["authorization"],
				)
			}
		})
	}
}

func TestMaterializeScopedSecretsFailureIsAtomicRetryableAndSingleflight(t *testing.T) {
	const (
		passwordRaw      = "$ENV://ES_RETRY_PASSWORD"
		authorizationRaw = "$secret://vault/es-retry-token"
	)
	raw := map[string]any{
		"auth":    map[string]any{"username": "elastic", "password": passwordRaw},
		"headers": map[string]string{"Authorization": authorizationRaw},
	}
	secrets, scope, broker, closeAttempt := newElasticsearchScopedSecretHarness(
		t, 20, "es-retry", raw, map[string]string{
			passwordRaw: "retry-password", authorizationRaw: "Bearer retry-token",
		},
	)
	defer closeAttempt()
	p := &Plugin{config: Config{
		Auth:    &AuthConfig{Username: "elastic", Password: passwordRaw},
		Headers: map[string]string{"Authorization": authorizationRaw},
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	broker.fail[authorizationRaw] = errors.New("private resolver failure")
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err == nil {
		t.Fatal("first materialization error = nil")
	}
	if p.config.Auth.Password != passwordRaw || p.config.Headers["Authorization"] != authorizationRaw ||
		p.password != nil ||
		p.authorization != nil {
		t.Fatalf(
			"failed materialization retained partial state: config=%#v password=%#v authorization=%#v",
			p.config,
			p.password,
			p.authorization,
		)
	}
	broker.mu.Lock()
	delete(broker.fail, authorizationRaw)
	broker.mu.Unlock()

	const workers = 32
	start := make(chan struct{})
	errs := make([]error, workers)
	var group sync.WaitGroup
	for index := range workers {
		group.Go(func() {
			<-start
			errs[index] = base.MaterializeScopedPluginSecrets(
				context.Background(),
				scope,
				secrets,
				p,
			)
		})
	}
	close(start)
	group.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("concurrent retry %d error = %v", index, err)
		}
	}
	if len(broker.calls) != 4 {
		t.Fatalf(
			"resolver calls = %#v, want failed password/auth then one successful password/auth sequence",
			broker.calls,
		)
	}
	for index, field := range []string{"auth.password", "headers.Authorization", "auth.password", "headers.Authorization"} {
		wantScope := scope
		wantScope.Field = field
		if broker.calls[index].Scope != wantScope {
			t.Fatalf(
				"resolver call %d scope = %#v, want %#v",
				index,
				broker.calls[index].Scope,
				wantScope,
			)
		}
	}
	assertElasticsearchDescriptorFor(t, p.config.Auth.Password, "retry-password")
	assertElasticsearchDescriptorFor(t, p.config.Headers["Authorization"], "Bearer retry-token")
}

func TestMaterializeScopedSecretsRejectsBlankCredentialsAndRetries(t *testing.T) {
	for index, field := range []string{"auth.password", "headers.Authorization"} {
		t.Run(field, func(t *testing.T) {
			passwordRaw := "$ENV://ES_BLANK_PASSWORD"
			authorizationRaw := "$ENV://ES_BLANK_AUTHORIZATION"
			raw := map[string]any{
				"auth":    map[string]any{"username": "elastic", "password": passwordRaw},
				"headers": map[string]string{"Authorization": authorizationRaw},
			}
			values := map[string]string{passwordRaw: "password", authorizationRaw: "Bearer token"}
			if field == "auth.password" {
				values[passwordRaw] = " \t "
			} else {
				values[authorizationRaw] = "\n"
			}
			secrets, scope, broker, closeAttempt := newElasticsearchScopedSecretHarness(
				t, uint64(30+index), "es-blank", raw, values,
			)
			defer closeAttempt()
			p := &Plugin{config: Config{
				Auth:    &AuthConfig{Username: "elastic", Password: passwordRaw},
				Headers: map[string]string{"Authorization": authorizationRaw},
			}}
			if err := p.Init(); err != nil {
				t.Fatal(err)
			}
			if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err == nil {
				t.Fatal("blank materialization error = nil")
			}
			if p.config.Auth.Password != passwordRaw ||
				p.config.Headers["Authorization"] != authorizationRaw {
				t.Fatal("blank materialization changed public config")
			}
			broker.mu.Lock()
			broker.values[passwordRaw] = "password"
			broker.values[authorizationRaw] = "Bearer token"
			broker.mu.Unlock()
			if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
				t.Fatalf("retry materialization error = %v", err)
			}
		})
	}
}

func TestAuthorizationIsolationAcrossElasticsearchGenerations(t *testing.T) {
	type observedRequest struct {
		path          string
		authorization string
	}
	observed := make(chan observedRequest, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- observedRequest{path: r.URL.Path, authorization: r.Header.Get("Authorization")}
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		if r.URL.Path == "/" {
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(server.Close)

	first := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL}, Field: FieldConfig{Index: "logs"},
		Auth: &AuthConfig{Username: "elastic", Password: "first-password"}, BatchMaxSize: 1,
	})
	second := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL}, Field: FieldConfig{Index: "logs"},
		Auth:    &AuthConfig{Username: "elastic", Password: "second-password"},
		Headers: map[string]string{"Authorization": "Bearer second-token"}, BatchMaxSize: 1,
	})
	t.Cleanup(first.Stop)
	t.Cleanup(second.Stop)
	if len(first.clients) != 1 || len(second.clients) != 1 {
		t.Fatalf(
			"client counts = %d/%d, want one per generation",
			len(first.clients),
			len(second.clients),
		)
	}
	var firstRef, secondRef *esClientRef
	for _, ref := range first.clients {
		firstRef = ref
	}
	for _, ref := range second.clients {
		secondRef = ref
	}
	if firstRef.client == secondRef.client || firstRef.credentials == secondRef.credentials {
		t.Fatal("two generations reused a credential-bearing Elasticsearch client")
	}
	if firstRef.credentials.transport != secondRef.credentials.transport {
		t.Fatal("credential-neutral transport was not reused across compatible generations")
	}
	if _, err := first.SendBatch(context.Background(), []map[string]any{{"generation": 1}}, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := second.SendBatch(context.Background(), []map[string]any{{"generation": 2}}, 1); err != nil {
		t.Fatal(err)
	}

	want := map[string]int{
		"Basic " + base64.StdEncoding.EncodeToString([]byte("elastic:first-password")): 2,
		"Bearer second-token": 2,
	}
	for range 4 {
		select {
		case request := <-observed:
			if request.path != "/" && request.path != "/_bulk" {
				t.Fatalf("request path = %q", request.path)
			}
			want[request.authorization]--
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for generation requests")
		}
	}
	for authorization, remaining := range want {
		if remaining != 0 {
			t.Fatalf("Authorization %q remaining count = %d", authorization, remaining)
		}
	}
	assertElasticsearchClientRetainsNone(
		t,
		firstRef.client,
		"first-password",
		"Basic "+base64.StdEncoding.EncodeToString([]byte("elastic:first-password")),
	)
	assertElasticsearchClientRetainsNone(
		t,
		secondRef.client,
		"second-password",
		"Bearer second-token",
	)
}

func assertElasticsearchClientRetainsNone(t *testing.T, client any, forbidden ...string) {
	t.Helper()
	if path, value, ok := findElasticsearchRetainedSecret(
		reflect.ValueOf(client), "client", forbidden, make(map[uintptr]struct{}), 0,
	); ok {
		t.Fatalf("Elasticsearch client retained secret at %s: %q", path, value)
	}
}

func findElasticsearchRetainedSecret(
	value reflect.Value,
	path string,
	forbidden []string,
	visited map[uintptr]struct{},
	depth int,
) (string, string, bool) {
	if !value.IsValid() || depth > 14 {
		return "", "", false
	}
	for value.Kind() == reflect.Interface {
		if value.IsNil() {
			return "", "", false
		}
		value = value.Elem()
	}
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return "", "", false
		}
		pointer := value.Pointer()
		if _, ok := visited[pointer]; ok {
			return "", "", false
		}
		visited[pointer] = struct{}{}
		if value.Type() == reflect.TypeFor[*http.Transport]() {
			return "", "", false
		}
		return findElasticsearchRetainedSecret(value.Elem(), path, forbidden, visited, depth+1)
	}
	switch value.Kind() {
	case reflect.String:
		text := value.String()
		for _, secretValue := range forbidden {
			if secretValue != "" && strings.Contains(text, secretValue) {
				return path, text, true
			}
		}
	case reflect.Slice:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			text := string(value.Bytes())
			for _, secretValue := range forbidden {
				if secretValue != "" && strings.Contains(text, secretValue) {
					return path, text, true
				}
			}
			return "", "", false
		}
		for index := range value.Len() {
			if foundPath, text, ok := findElasticsearchRetainedSecret(
				value.Index(index), fmt.Sprintf("%s[%d]", path, index), forbidden, visited, depth+1,
			); ok {
				return foundPath, text, true
			}
		}
	case reflect.Map:
		iterator := value.MapRange()
		for iterator.Next() {
			if foundPath, text, ok := findElasticsearchRetainedSecret(
				iterator.Value(), path+"[map]", forbidden, visited, depth+1,
			); ok {
				return foundPath, text, true
			}
		}
	case reflect.Struct:
		credentialType := reflect.TypeFor[elasticsearchCredentialTransport]()
		for index := range value.NumField() {
			field := value.Type().Field(index)
			if value.Type() == credentialType &&
				(field.Name == "owner" || field.Name == "transport") {
				continue
			}
			if foundPath, text, ok := findElasticsearchRetainedSecret(
				value.Field(index), path+"."+field.Name, forbidden, visited, depth+1,
			); ok {
				return foundPath, text, true
			}
		}
	}
	return "", "", false
}

func TestLowercaseAuthorizationRemainsOrdinaryHeader(t *testing.T) {
	received := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Authorization")
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		if r.URL.Path == "/" {
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
		} else {
			_, _ = w.Write([]byte(`{"errors":false}`))
		}
	}))
	t.Cleanup(server.Close)
	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL}, Field: FieldConfig{Index: "logs"},
		Auth:    &AuthConfig{Username: "elastic", Password: "private-password"},
		Headers: map[string]string{"authorization": "ordinary-lowercase"}, BatchMaxSize: 1,
	})
	t.Cleanup(p.Stop)
	if _, err := p.SendBatch(context.Background(), []map[string]any{{"path": "/"}}, 1); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case got := <-received:
			if got != "ordinary-lowercase" {
				t.Fatalf(
					"Authorization = %q, want ordinary lowercase header to retain precedence",
					got,
				)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for Elasticsearch request")
		}
	}
}

type retainingElasticsearchRoundTripper struct {
	request *http.Request
}

func (transport *retainingElasticsearchRoundTripper) RoundTrip(
	req *http.Request,
) (*http.Response, error) {
	transport.request = req
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"errors":false}`)),
		Request:    req,
	}, nil
}

func TestElasticsearchCredentialTransportClearsRetainedAuthorization(t *testing.T) {
	retained := &retainingElasticsearchRoundTripper{}
	const rawAuthorization = "$ENV://ES_TRANSPORT_AUTHORIZATION"
	secrets, scope, _, closeAttempt := newElasticsearchScopedSecretHarness(
		t,
		42,
		"es-transport",
		map[string]any{"headers": map[string]string{"Authorization": rawAuthorization}},
		map[string]string{rawAuthorization: "Bearer private-token"},
	)
	defer closeAttempt()
	owner := &Plugin{config: Config{Headers: map[string]string{"Authorization": rawAuthorization}}}
	if err := owner.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, owner,
	); err != nil {
		t.Fatal(err)
	}
	credentials := &elasticsearchCredentialTransport{
		owner: owner, transport: retained, override: true,
	}
	var derived []byte
	credentials.afterDerive = func(value []byte) { derived = value }
	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/_bulk",
		strings.NewReader("{}\n"),
	)
	req.Header.Set("Authorization", "ordinary")
	resp, err := credentials.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if req.Header.Get("Authorization") != "ordinary" {
		t.Fatalf(
			"caller request Authorization = %q, want untouched ordinary value",
			req.Header.Get("Authorization"),
		)
	}
	if retained.request == req {
		t.Fatal("credential transport passed the caller request directly")
	}
	if got := retained.request.Header.Get("Authorization"); got != "" {
		t.Fatalf("retained request Authorization = %q, want cleared after RoundTrip", got)
	}
	if resp.Request != retained.request || resp.Request.Header.Get("Authorization") != "" {
		t.Fatal("response retained credential-bearing request header")
	}
	if len(derived) == 0 {
		t.Fatal("credential transport did not derive request-local Authorization bytes")
	}
	for index, value := range derived {
		if value != 0 {
			t.Fatalf("derived Authorization byte %d = %d, want zero after RoundTrip", index, value)
		}
	}
	credentialType := reflect.TypeFor[elasticsearchCredentialTransport]()
	for field := range credentialType.Fields() {
		if field.Type.Kind() == reflect.String || field.Type.Kind() == reflect.Slice ||
			field.Type.Kind() == reflect.Array {
			t.Fatalf(
				"credential transport persistently retains credential-capable field %s %s",
				field.Name,
				field.Type,
			)
		}
	}
	credentials.destroy()
	if credentials.owner != nil || credentials.transport != nil {
		t.Fatal("destroy retained credential transport state")
	}
}

func TestStopDrainsActiveScopedElasticsearchSend(t *testing.T) {
	const rawPassword = "$secret://vault/es-scoped-stop"
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		if r.URL.Path == "/" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		server.Close()
	})
	raw := map[string]any{
		"endpoint_addrs": []string{server.URL},
		"field":          map[string]any{"index": "logs"},
		"log_format":     map[string]any{"request_id": "$request_id"},
		"auth":           map[string]any{"username": "elastic", "password": rawPassword},
	}
	secrets, scope, _, closeAttempt := newElasticsearchScopedSecretHarness(
		t, 41, "es-scoped-stop", raw, map[string]string{rawPassword: "scoped-password"},
	)
	defer closeAttempt()
	p := &Plugin{config: Config{
		EndpointAddrs: []string{server.URL}, Field: FieldConfig{Index: "logs"},
		LogFormat: map[string]string{"request_id": "$request_id"},
		Auth:      &AuthConfig{Username: "elastic", Password: rawPassword}, BatchMaxSize: 1,
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	if err := base.MaterializeScopedPluginSecrets(context.Background(), scope, secrets, p); err != nil {
		t.Fatal(err)
	}
	if p.TaskOwner() == nil {
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	}
	if err := p.PostInit(); err != nil {
		t.Fatal(err)
	}
	stopAttempted := make(chan struct{})
	p.stopBeforeLock = func() { close(stopAttempted) }
	activeResult := make(chan error, 1)
	go func() {
		_, err := p.SendBatch(context.Background(), []map[string]any{{"active": true}}, 1)
		activeResult <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for scoped send")
	}
	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopAttempted:
	case <-time.After(time.Second):
		t.Fatal("scoped Stop did not reach scheduler seal barrier")
	}
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("scoped Stop blocked on active credential use before sealing the scheduler")
	}
	processor := p.BatchProcessor
	p.clientMu.Lock()
	clientsRetained := len(p.clients) > 0
	p.clientMu.Unlock()
	if !clientsRetained {
		t.Fatal("scoped Stop released the active client before credential use drained")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-activeResult:
		if err != nil {
			t.Fatalf("active scoped send error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("active scoped send did not finish")
	}
	if err := processor.Shutdown(context.Background()); err != nil {
		t.Fatalf("batch Shutdown() error = %v", err)
	}
	if p.password != nil || p.authorization != nil || len(p.clients) != 0 {
		t.Fatal("scoped Stop retained credentials or clients")
	}
}

func TestPostInitCannotPublishBatchProcessorAfterStop(t *testing.T) {
	const rawPassword = "$secret://vault/es-post-init-stop"
	p := &Plugin{config: Config{
		EndpointAddrs: []string{"http://127.0.0.1:9200"},
		Field:         FieldConfig{Index: "logs"},
		LogFormat:     map[string]string{"request_id": "$request_id"},
		Auth:          &AuthConfig{Username: "elastic", Password: rawPassword},
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	secrets, scope, _, closeAttempt := newElasticsearchScopedSecretHarness(
		t,
		42,
		"es-post-init-stop",
		map[string]any{
			"endpoint_addrs": []string{"http://127.0.0.1:9200"},
			"field":          map[string]any{"index": "logs"},
			"log_format":     map[string]any{"request_id": "$request_id"},
			"auth": map[string]any{
				"username": "elastic", "password": rawPassword,
			},
		},
		map[string]string{rawPassword: "scoped-post-init-password"},
	)
	defer closeAttempt()
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatalf("MaterializeScopedPluginSecrets() error = %v", err)
	}

	atPublishBarrier := make(chan struct{})
	releasePostInit := make(chan struct{})
	var candidate *logger_batch.Processor
	p.postInitBeforePublish = func(processor *logger_batch.Processor) {
		candidate = processor
		close(atPublishBarrier)
		<-releasePostInit
	}
	postInitResult := make(chan error, 1)
	go func() { postInitResult <- p.PostInit() }()
	select {
	case <-atPublishBarrier:
	case <-time.After(time.Second):
		t.Fatal("PostInit did not reach the batch publication barrier")
	}

	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("Stop did not finish while PostInit was paused before publication")
	}
	close(releasePostInit)
	select {
	case err := <-postInitResult:
		if !errors.Is(err, secret.ErrCredentialUnavailable) {
			t.Fatalf("PostInit() after Stop error = %v, want credential unavailable", err)
		}
	case <-time.After(time.Second):
		t.Fatal("PostInit did not return after publication barrier was released")
	}
	if p.BatchProcessor != nil {
		t.Fatal("PostInit published a batch processor after Stop")
	}
	if candidate == nil || candidate.Push(map[string]any{"late": true}) {
		t.Fatal("PostInit left the unpublished batch processor alive after Stop")
	}
	p.Stop()
	if !p.stopped.Load() || p.BatchProcessor != nil || len(p.clients) != 0 ||
		p.password != nil || p.authorization != nil {
		t.Fatal("repeated Stop left Elasticsearch generation state")
	}
}

func TestNewElasticsearchClientUsesOfficialBulkTransport(t *testing.T) {
	const (
		username = "elastic"
		password = "secret"
	)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/_bulk" {
			t.Errorf("path = %q, want /_bulk", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-ndjson" {
			t.Errorf("Content-Type = %q, want application/x-ndjson", got)
		}
		if got := r.Header.Get("X-Cluster"); got != "logs" {
			t.Errorf("X-Cluster = %q, want logs", got)
		}
		gotUsername, gotPassword, ok := r.BasicAuth()
		if !ok || gotUsername != username || gotPassword != password {
			t.Errorf(
				"BasicAuth() = %q/%q/%v, want %q/%q/true",
				gotUsername,
				gotPassword,
				ok,
				username,
				password,
			)
		}
		if r.URL.RawQuery != "" {
			t.Errorf(
				"RawQuery = %q, want empty to match APISIX 3.17 client-only timeout",
				r.URL.RawQuery,
			)
		}

		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(server.Close)

	sslVerify := true
	const rawPassword = "$ENV://ES_OFFICIAL_TRANSPORT_PASSWORD"
	p := &Plugin{config: Config{
		Auth:    &AuthConfig{Username: username, Password: rawPassword},
		Headers: map[string]string{"X-Cluster": "logs"},
		Timeout: 10, SslVerify: &sslVerify,
	}}
	if err := p.Init(); err != nil {
		t.Fatal(err)
	}
	secrets, scope, _, closeAttempt := newElasticsearchScopedSecretHarness(
		t,
		43,
		"es-official-transport",
		map[string]any{"auth": map[string]any{"username": username, "password": rawPassword}},
		map[string]string{rawPassword: password},
	)
	defer closeAttempt()
	if err := base.MaterializeScopedPluginSecrets(
		context.Background(), scope, secrets, p,
	); err != nil {
		t.Fatal(err)
	}
	client, credentials, release, err := p.newPluginOwnedClient(server.URL)
	if err != nil {
		t.Fatalf("newPluginOwnedClient() error = %v", err)
	}
	t.Cleanup(func() {
		credentials.destroy()
		release()
	})

	resp, err := (esapi.BulkRequest{
		Body:   strings.NewReader("{}\n"),
		Header: http.Header{"Content-Type": []string{"application/x-ndjson"}},
	}).Do(context.Background(), client)
	if err != nil {
		t.Fatalf("BulkRequest.Do() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.IsError() {
		t.Fatalf("BulkRequest.Do() status = %s, want success", resp.Status())
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("bulk attempts = %d, want retry after one 503", got)
	}
}

func TestRunLogPhasePreservesIndexAndDetachedHostFields(t *testing.T) {
	delivered := make(chan map[string]any, 1)
	p := &Plugin{
		config: Config{Field: FieldConfig{Index: "apisix-$route_id"}},
		BaseLoggerPlugin: base.BaseLoggerPlugin{LogFormat: map[string]string{
			"host": "$host", "remote": "$remote_addr",
		}},
	}
	p.BatchProcessor = newOwnedBatchProcessorForTest(t, logger_batch.Config{
		BatchMaxSize: 1, MaxPendingEntries: 1, InactiveTimeout: time.Hour,
		BufferDuration: time.Hour, ShutdownTimeout: time.Second,
	}, func(_ context.Context, entries []map[string]any, _ int) (int, error) {
		delivered <- entries[0]
		return 0, nil
	})
	t.Cleanup(p.Stop)
	snapshot := base.LogSnapshot{Request: apisixlog.RequestLogSnapshot{
		Host: "gateway.example", RemoteAddr: "192.0.2.10:8443",
		APISIXVars: map[string]any{"$route_id": "r-17"},
	}}
	if err := p.RunLogPhase(snapshot); err != nil {
		t.Fatalf("RunLogPhase() error = %v", err)
	}
	select {
	case entry := <-delivered:
		if entry[elasticsearchIndexField] != "apisix-r-17" {
			t.Fatalf("index = %#v, want resolved route index", entry[elasticsearchIndexField])
		}
		if entry["host"] != "gateway.example" || entry["remote"] != "192.0.2.10" {
			t.Fatalf("detached host fields = %#v/%#v", entry["host"], entry["remote"])
		}
	case <-time.After(time.Second):
		t.Fatal("detached Elasticsearch entry was not delivered")
	}
}

func TestSendBatchCancelsElasticsearchBulkWithContext(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		close(started)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL},
		Field:         FieldConfig{Index: "apisix-logs"},
		Timeout:       10,
	})
	t.Cleanup(p.BatchProcessor.Stop)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan error, 1)
	go func() {
		_, err := p.SendBatch(ctx, []map[string]any{{"path": "/cancel"}}, 1)
		result <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Elasticsearch bulk request")
	}
	cancel()

	var err error
	select {
	case err = <-result:
	case <-time.After(time.Second):
		t.Fatal("SendBatch() did not return after context cancellation")
	}
	if err == nil {
		t.Fatal("SendBatch() error = nil, want context cancellation")
	}
	select {
	case <-canceled:
	case <-time.After(100 * time.Millisecond):
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf(
				"SendBatch() error = %v, want context cancellation when backend did not observe it",
				err,
			)
		}
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
	return p
}

func TestEffectiveLogFormatRouteWins(t *testing.T) {
	route := map[string]string{"route": "$request_id"}
	metadata := map[string]string{"metadata": "$route_id"}
	view := metadataViewForElasticsearchLogFormat(t, metadata, 0)

	p := newRawTestPlugin(t, Config{
		Field:     FieldConfig{Index: "apisix"},
		LogFormat: route,
	}, view)
	if len(p.LogFormat) != 1 || p.LogFormat["route"] != route["route"] {
		t.Fatalf(
			"effective format = %#v, want route format over metadata %#v",
			p.LogFormat,
			metadata,
		)
	}
	route["route"] = "mutated"
	if p.LogFormat["route"] == "mutated" {
		t.Fatal("effective route format was not cloned")
	}
}

func TestEffectiveLogFormatUsesMetadataFallback(t *testing.T) {
	metadata := map[string]string{"route": "$route_id"}
	view := metadataViewForElasticsearchLogFormat(t, metadata, 0)

	p := newRawTestPlugin(t, Config{Field: FieldConfig{Index: "apisix"}}, view)
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
		EndpointAddrs: []string{"http://127.0.0.1:9200"},
		Field:         FieldConfig{Index: "apisix"},
	}}
	p.SetDependencies(
		base.Dependencies{
			Tasks:          newLoggerTestTaskOwner(t),
			DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
		},
	)
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if p.TaskOwner() == nil {
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	}
	err := p.PostInit()
	if err == nil || !strings.Contains(err.Error(), name) ||
		!strings.Contains(err.Error(), "log_format") {
		t.Fatalf("PostInit() error = %v, want %s log_format rejection", err, name)
	}
	if p.BatchProcessor != nil || len(p.clients) != 0 {
		t.Fatalf(
			"PostInit() side effects = batch=%v clients=%d, want none",
			p.BatchProcessor,
			len(p.clients),
		)
	}
}

func TestPreparedGenerationsRetainMetadataFormat(t *testing.T) {
	nSource := []byte(`{"log_format":{"generation":"n"},"max_pending_entries":11}`)
	nView := mustMetadataView(t, map[string][]byte{name: nSource})
	clear(nSource)
	n := newRawTestPlugin(t, Config{Field: FieldConfig{Index: "apisix"}}, nView)

	n1Source := []byte(`{"log_format":{"generation":"n1"},"max_pending_entries":12}`)
	n1View := mustMetadataView(t, map[string][]byte{name: n1Source})
	clear(n1Source)
	n1 := newRawTestPlugin(t, Config{Field: FieldConfig{Index: "apisix"}}, n1View)

	if got := n.LogFormat["generation"]; got != "n" || n.config.MaxPendingEntries != 11 {
		t.Fatalf("N metadata = format %q pending %d, want n/11", got, n.config.MaxPendingEntries)
	}
	if got := n1.LogFormat["generation"]; got != "n1" || n1.config.MaxPendingEntries != 12 {
		t.Fatalf(
			"N+1 metadata = format %q pending %d, want n1/12",
			got,
			n1.config.MaxPendingEntries,
		)
	}
}

func TestPostInitRejectsInvalidMetadataBeforeSideEffects(t *testing.T) {
	p := &Plugin{config: Config{
		Field:     FieldConfig{Index: "apisix"},
		LogFormat: map[string]string{"route": "$route_id"},
	}}
	p.SetDependencies(base.Dependencies{
		Tasks:          newLoggerTestTaskOwner(t),
		DataEncryption: testutil.DataEncryptionService(false, nil).Resolver(),
		Metadata: mustMetadataView(t, map[string][]byte{
			name: []byte(`{"log_format":"sensitive-invalid-metadata"}`),
		}),
	})
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	t.Cleanup(p.Stop)
	if p.TaskOwner() == nil {
		p.SetDependencies(base.Dependencies{Tasks: newLoggerTestTaskOwner(t)})
	}
	err := p.PostInit()
	if err == nil || !strings.Contains(err.Error(), "elasticsearch-logger metadata decode failed") {
		t.Fatalf("PostInit() error = %v, want redacted metadata decode failure", err)
	}
	if strings.Contains(err.Error(), "sensitive-invalid-metadata") {
		t.Fatalf("PostInit() leaked metadata: %v", err)
	}
	if p.BatchProcessor != nil || len(p.clients) != 0 {
		t.Fatalf(
			"PostInit() side effects = batch=%v clients=%d, want none",
			p.BatchProcessor,
			len(p.clients),
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

func metadataViewForElasticsearchLogFormat(
	t *testing.T, logFormat map[string]string, maxPendingEntries int,
) runtime.MetadataView {
	t.Helper()
	metadata := map[string]any{"log_format": logFormat}
	if maxPendingEntries > 0 {
		metadata["max_pending_entries"] = maxPendingEntries
	}
	value, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return mustMetadataView(t, map[string][]byte{name: value})
}

func mustMetadataView(t *testing.T, documents map[string][]byte) runtime.MetadataView {
	t.Helper()
	view, err := runtime.NewMetadataView(documents)
	if err != nil {
		t.Fatalf("NewMetadataView() error = %v", err)
	}
	return view
}

func TestPostInitDefaultsWithoutMetadataStore(t *testing.T) {
	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{"http://127.0.0.1:9200"},
		Field:         FieldConfig{Index: "apisix"},
	})

	if p.config.Timeout != 10 {
		t.Fatalf("timeout = %d, want official default 10 seconds", p.config.Timeout)
	}
	if p.config.SslVerify == nil || !*p.config.SslVerify {
		t.Fatalf("ssl_verify = %v, want true", p.config.SslVerify)
	}
	if p.config.BatchMaxSize != 1000 {
		t.Fatalf("batch_max_size = %d, want 1000", p.config.BatchMaxSize)
	}
}

func TestSendWritesBulkNDJSONWithHeadersAndAuth(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		if r.URL.Path != "/_bulk" {
			t.Fatalf("path = %q, want /_bulk", r.URL.Path)
		}
		if r.URL.RawQuery != "" {
			t.Fatalf(
				"RawQuery = %q, want empty to match APISIX 3.17 client-only timeout",
				r.URL.RawQuery,
			)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if contentType := r.Header.Get("Content-Type"); contentType != "application/x-ndjson" {
			t.Fatalf("Content-Type = %q, want application/x-ndjson", contentType)
		}
		if r.Header.Get("X-Cluster") != "logs" {
			t.Fatalf("X-Cluster = %q, want logs", r.Header.Get("X-Cluster"))
		}
		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("elastic:secret"))
		if r.Header.Get("Authorization") != wantAuth {
			t.Fatalf("Authorization = %q, want %q", r.Header.Get("Authorization"), wantAuth)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read bulk body: %v", err)
		}
		received <- string(body)
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL},
		Field:         FieldConfig{Index: "apisix-logs"},
		Auth:          &AuthConfig{Username: "elastic", Password: "secret"},
		Headers:       map[string]string{"X-Cluster": "logs"},
		Timeout:       10,
	})
	p.Send(map[string]any{"path": "/orders"})

	select {
	case body := <-received:
		if !strings.Contains(body, `{"index":{"_index":"apisix-logs"}}`+"\n") {
			t.Fatalf("bulk body = %q, want index action", body)
		}
		if !strings.Contains(body, `"path":"/orders"`) {
			t.Fatalf("bulk body = %q, want log document", body)
		}
		if !strings.HasSuffix(body, "\n") {
			t.Fatalf("bulk body = %q, want trailing newline", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch bulk request")
	}
}

func TestSendBatchDetectsBulkItemFailures(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read bulk body: %v", err)
		}
		received <- string(body)
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(
			`{"errors":true,"items":[` +
				`{"index":{"_index":"apisix-logs","status":201}},` +
				`{"index":{"_index":"apisix-logs","status":429,"error":{"type":"too_many_requests"}}},` +
				`{"index":{"_index":"apisix-logs","status":201}}]}`,
		))
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL},
		Field:         FieldConfig{Index: "apisix-logs"},
		BatchMaxSize:  3,
		Timeout:       10,
	})

	firstFail, err := p.SendBatch(
		context.Background(),
		[]map[string]any{{"path": "/a"}, {"path": "/b"}, {"path": "/c"}},
		3,
	)
	if err == nil {
		t.Fatal("SendBatch() error = nil, want detected bulk item failure")
	}
	if firstFail != 2 {
		t.Fatalf("firstFail = %d, want 2 (second bulk item)", firstFail)
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch bulk request")
	}
}

func TestSendBatchMalformedBulkResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":`))
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL},
		Field:         FieldConfig{Index: "apisix-logs"},
		BatchMaxSize:  1,
		Timeout:       10,
	})

	firstFail, err := p.SendBatch(context.Background(), []map[string]any{{"path": "/a"}}, 1)
	if err == nil {
		t.Fatal("SendBatch() error = nil, want malformed bulk response error")
	}
	if firstFail != 1 {
		t.Fatalf("firstFail = %d, want 1 for an undecodable bulk result", firstFail)
	}
}

func TestSendBatchWritesMultipleBulkEntries(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read bulk body: %v", err)
		}
		received <- string(body)
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL},
		Field:         FieldConfig{Index: "apisix-logs"},
		BatchMaxSize:  2,
	})

	if _, err := p.SendBatch(context.Background(), []map[string]any{{"path": "/a"}, {"path": "/b"}}, 2); err != nil {
		t.Fatalf("SendBatch() error = %v", err)
	}

	select {
	case body := <-received:
		lines := strings.Split(strings.TrimSpace(body), "\n")
		if len(lines) != 4 {
			t.Fatalf("bulk lines = %d, want 4, body = %q", len(lines), body)
		}
		if !strings.Contains(body, `"path":"/a"`) || !strings.Contains(body, `"path":"/b"`) {
			t.Fatalf("bulk body = %q, want both documents", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch batch bulk request")
	}
}

func TestSendSelectsRandomEndpointAddr(t *testing.T) {
	firstRequests := make(chan struct{}, 1)
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstRequests <- struct{}{}
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(first.Close)

	secondRequests := make(chan struct{}, 1)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		if r.URL.Path != "/_bulk" {
			t.Fatalf("path = %q, want /_bulk", r.URL.Path)
		}
		secondRequests <- struct{}{}
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(second.Close)

	oldRandomEndpointIndex := randomEndpointIndex
	randomEndpointIndex = func(n int) int {
		if n != 2 {
			t.Fatalf("random endpoint count = %d, want 2", n)
		}
		return 1
	}
	t.Cleanup(func() {
		randomEndpointIndex = oldRandomEndpointIndex
	})

	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{first.URL, second.URL},
		Field:         FieldConfig{Index: "apisix-logs"},
		Timeout:       10,
	})
	p.Send(map[string]any{"path": "/orders"})

	select {
	case <-secondRequests:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for selected Elasticsearch endpoint")
	}

	select {
	case <-firstRequests:
		t.Fatal("first Elasticsearch endpoint received request, want selected second endpoint only")
	default:
	}
}

func TestSendDiscoversOlderElasticsearchVersion(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"6.8.23"}}`))
		case "/_bulk":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read bulk body: %v", err)
			}
			received <- string(body)
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"errors":false}`))
		default:
			t.Fatalf("path = %q, want / or /_bulk", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL},
		Field:         FieldConfig{Index: "apisix-logs"},
		Timeout:       10,
	})
	p.Send(map[string]any{"path": "/orders"})

	select {
	case body := <-received:
		if !strings.Contains(body, `"_type":"_doc"`) {
			t.Fatalf("bulk body = %q, want _type _doc for Elasticsearch 6", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch bulk request")
	}
}

func TestBulkBodyIgnoresUnsupportedConfiguredType(t *testing.T) {
	configuredType := "collector"
	for _, test := range []struct {
		version  string
		wantType string
	}{
		{version: "9"},
		{version: "8"},
		{version: "7"},
		{version: "6", wantType: "_doc"},
	} {
		t.Run(test.version, func(t *testing.T) {
			p := &Plugin{
				config: Config{
					Field: FieldConfig{Index: "services", Type: &configuredType},
				},
				esVersion: test.version,
			}

			body, err := p.bulkBodyEntry(map[string]any{"test": "test"})
			if err != nil {
				t.Fatalf("bulkBodyEntry() error = %v", err)
			}
			action := strings.SplitN(string(body), "\n", 2)[0]
			if strings.Contains(action, configuredType) {
				t.Fatalf("bulk action = %s, want unsupported configured type omitted", action)
			}
			if test.wantType == "" {
				if strings.Contains(action, `"_type"`) {
					t.Fatalf(
						"bulk action = %s, want no _type for Elasticsearch %s",
						action,
						test.version,
					)
				}
				return
			}
			if !strings.Contains(action, `"_type":"`+test.wantType+`"`) {
				t.Fatalf("bulk action = %s, want _type %q", action, test.wantType)
			}
		})
	}
}

func TestElasticsearchLogFieldsPreservesNginxHostAndRemoteAddress(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://unused.example/orders", nil)
	req.Host = "logs.example"
	req.RemoteAddr = "127.0.0.1:54321"

	fields := elasticsearchLogFields(req, map[string]string{
		"custom_host":      "$host",
		"custom_client_ip": "$remote_addr",
	})
	if fields["custom_host"] != "logs.example" {
		t.Fatalf("custom_host = %#v, want logs.example", fields["custom_host"])
	}
	if fields["custom_client_ip"] != "127.0.0.1" {
		t.Fatalf("custom_client_ip = %#v, want 127.0.0.1", fields["custom_client_ip"])
	}
}

func TestHandlerResolvesIndexTimeAndApisixVariables(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read bulk body: %v", err)
		}
		received <- string(body)
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL},
		Field:         FieldConfig{Index: "apisix-$route_id-{%Y}"},
		LogFormat:     map[string]string{"path": "$uri"},
		Timeout:       10,
		BatchMaxSize:  1,
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/orders", nil)
	req = apisixctx.WithApisixVars(req, map[string]string{"$route_id": "route-1"})
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	select {
	case body := <-received:
		wantIndex := `"apisix-route-1-` + time.Now().Format("2006") + `"`
		if !strings.Contains(body, `"_index":`+wantIndex) {
			t.Fatalf("bulk body = %q, want resolved index containing %s", body, wantIndex)
		}
		if strings.Contains(body, elasticsearchIndexField) {
			t.Fatalf("bulk body = %q, want internal index field omitted from document", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch bulk request")
	}
}

func TestHandlerIncludesRequestAndResponseBody(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		if r.URL.Path != "/_bulk" {
			t.Fatalf("path = %q, want /_bulk", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read bulk body: %v", err)
		}
		received <- string(body)
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs:    []string{server.URL},
		Field:            FieldConfig{Index: "apisix-logs"},
		Timeout:          10,
		IncludeReqBody:   true,
		IncludeRespBody:  true,
		MaxReqBodyBytes:  32,
		MaxRespBodyBytes: 32,
		BatchMaxSize:     1,
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/orders",
		bytes.NewBufferString(`{"order":1}`),
	)
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
	case body := <-received:
		document := extractBulkDocument(t, body)
		request, ok := document["request"].(map[string]any)
		if !ok {
			t.Fatalf("document request = %#v, want object", document["request"])
		}
		if request["body"] != `{"order":1}` {
			t.Fatalf("document request body = %#v, want original request body", request["body"])
		}

		response, ok := document["response"].(map[string]any)
		if !ok {
			t.Fatalf("document response = %#v, want object", document["response"])
		}
		if response["body"] != `{"ok":true}` {
			t.Fatalf("document response body = %#v, want upstream response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch bulk request")
	}
}

func TestHandlerIncludesBodiesWhenExpressionsMatch(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read bulk body: %v", err)
		}
		received <- string(body)
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs:       []string{server.URL},
		Field:               FieldConfig{Index: "apisix-logs"},
		Timeout:             10,
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: [][]any{{"status", "==", "201"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
		BatchMaxSize:        1,
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/orders",
		bytes.NewBufferString(`{"order":2}`),
	)
	req.Header.Set("X-Log-Body", "yes")
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	})).ServeHTTP(rr, req)

	select {
	case body := <-received:
		document := extractBulkDocument(t, body)
		request, ok := document["request"].(map[string]any)
		if !ok {
			t.Fatalf("document request = %#v, want object", document["request"])
		}
		if request["body"] != `{"order":2}` {
			t.Fatalf("document request body = %#v, want captured request body", request["body"])
		}

		response, ok := document["response"].(map[string]any)
		if !ok {
			t.Fatalf("document response = %#v, want object", document["response"])
		}
		if response["body"] != `{"created":true}` {
			t.Fatalf("document response body = %#v, want captured response body", response["body"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch bulk request")
	}
}

func TestHandlerSkipsBodiesWhenExpressionsDoNotMatch(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read bulk body: %v", err)
		}
		received <- string(body)
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs:       []string{server.URL},
		Field:               FieldConfig{Index: "apisix-logs"},
		Timeout:             10,
		IncludeReqBody:      true,
		IncludeReqBodyExpr:  [][]any{{"http_x_log_body", "==", "yes"}},
		IncludeRespBody:     true,
		IncludeRespBodyExpr: [][]any{{"status", "==", "500"}},
		MaxReqBodyBytes:     32,
		MaxRespBodyBytes:    32,
		BatchMaxSize:        1,
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"http://example.com/orders",
		bytes.NewBufferString(`{"order":3}`),
	)
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
	case body := <-received:
		document := extractBulkDocument(t, body)
		if _, ok := document["request"]; ok {
			t.Fatalf("document request = %#v, want no request body", document["request"])
		}
		if _, ok := document["response"]; ok {
			t.Fatalf("document response = %#v, want no response body", document["response"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch bulk request")
	}
}

func TestSchemaAcceptsOfficialBodyExpressionFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"endpoint_addr":          "http://127.0.0.1:9200",
		"field":                  map[string]any{"index": "apisix"},
		"include_req_body_expr":  []any{[]any{"http_x_log_body", "==", "yes"}},
		"include_resp_body_expr": []any{[]any{"status", "==", "201"}},
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected official body expression fields: %v", err)
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

func TestSchemaAcceptsEndpointAddrHeadersAndBodyFields(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	config := map[string]any{
		"endpoint_addr":       "http://127.0.0.1:9200",
		"field":               map[string]any{"index": "apisix"},
		"headers":             map[string]any{"X-Cluster": "logs"},
		"batch_max_size":      2,
		"max_pending_entries": 100,
		"include_req_body":    true,
		"include_resp_body":   true,
		"max_req_body_bytes":  1024,
		"max_resp_body_bytes": 2048,
	}
	if err := util.Validate(config, p.GetSchema()); err != nil {
		t.Fatalf("schema rejected config fields: %v", err)
	}
}

func TestSchemaMatchesAPISIX317Matrix(t *testing.T) {
	p := &Plugin{}
	if err := p.Init(); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	valid := []map[string]any{
		{
			"endpoint_addr":    "http://127.0.0.1:9200",
			"field":            map[string]any{"index": "services"},
			"auth":             map[string]any{"username": "elastic", "password": "123456"},
			"ssl_verify":       false,
			"timeout":          60,
			"max_retry_count":  0,
			"retry_delay":      1,
			"buffer_duration":  60,
			"inactive_timeout": 2,
			"batch_max_size":   1,
		},
		{
			"endpoint_addr": "http://127.0.0.1:9200",
			"field":         map[string]any{"index": "services"},
		},
	}
	for i, config := range valid {
		if err := util.Validate(config, p.GetSchema()); err != nil {
			t.Fatalf("valid Elasticsearch schema case %d rejected: %v", i+1, err)
		}
	}

	invalid := []map[string]any{
		{"field": map[string]any{"index": "services"}},
		{"endpoint_addr": "http://127.0.0.1:9200"},
		{"endpoint_addr": "http://127.0.0.1:9200", "field": map[string]any{}},
		{"endpoint_addr": "http://127.0.0.1:9200/", "field": map[string]any{"index": "services"}},
	}
	for i, config := range invalid {
		if err := util.Validate(config, p.GetSchema()); err == nil {
			t.Fatalf("invalid Elasticsearch schema case %d accepted: %#v", i+1, config)
		}
	}
}

func TestResolveIndexTimeVarsMatchesAPISIX317(t *testing.T) {
	tests := []struct {
		format  string
		pattern string
	}{
		{format: "%Y", pattern: `^prefix\d{4}suffix$`},
		{format: "%m", pattern: `^prefix\d{2}suffix$`},
		{format: "%d", pattern: `^prefix\d{2}suffix$`},
		{format: "%Y.%m.%d", pattern: `^prefix\d{4}\.\d{2}\.\d{2}suffix$`},
	}
	for _, test := range tests {
		got := replaceIndexTimeVars("prefix{" + test.format + "}suffix")
		if !regexp.MustCompile(test.pattern).MatchString(got) {
			t.Errorf("replaceIndexTimeVars(%q) = %q, want match %q", test.format, got, test.pattern)
		}
	}
}

func extractBulkDocument(t *testing.T, body string) map[string]any {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) != 2 {
		t.Fatalf("bulk body = %q, want action and document lines", body)
	}

	var document map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &document); err != nil {
		t.Fatalf("unmarshal bulk document: %v", err)
	}
	return document
}

func TestHandlerResolvesBraceFormApisixVariableInIndex(t *testing.T) {
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("X-Elastic-Product", "Elasticsearch")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read bulk body: %v", err)
		}
		received <- string(body)
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL},
		Field:         FieldConfig{Index: "services-${arg_id}-{%Y.%m.%d}"},
		LogFormat:     map[string]string{"path": "$uri"},
		Timeout:       10,
		BatchMaxSize:  1,
	})

	req := httptest.NewRequest(http.MethodGet, "http://example.com/hello?id=myservice", nil)
	rr := httptest.NewRecorder()
	p.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)

	select {
	case body := <-received:
		wantIndex := `"services-myservice-` + time.Now().Format("2006.01.02") + `"`
		if !strings.Contains(body, `"_index":`+wantIndex) {
			t.Fatalf(
				"bulk body = %q, want resolved brace-form index containing %s",
				body,
				wantIndex,
			)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Elasticsearch bulk request")
	}
}

func TestResolveIndexVariableReferencesMatchesAPISIXTemplateContract(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://example.test/orders?id=", nil)
	req.Host = "logs.example"

	got := resolveIndexVariableReferences(
		`plain-$host-${ arg_id }-${arg_missing ?? fallback}-${arg_id ?? fallback}-${consumer_name ?? anonymous}-$foo.bar-\$host-${}`,
		req,
	)
	const want = `plain-logs.example--fallback--anonymous--\$host-${}`
	if got != want {
		t.Fatalf("resolved index = %q, want %q", got, want)
	}
}

func TestVersionDetectionRunsOncePerStableConfig(t *testing.T) {
	var versionGets atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			if versionGets.Add(1) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`{"version":{"number":"8.11.0"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errors":false}`))
	}))
	t.Cleanup(server.Close)

	p := newTestPlugin(t, Config{
		EndpointAddrs: []string{server.URL},
		Field:         FieldConfig{Index: "apisix-logs"},
		Timeout:       10,
	})
	p.Send(map[string]any{"path": "/a"})
	p.Send(map[string]any{"path": "/b"})

	if got := versionGets.Load(); got != 1 {
		t.Fatalf("version detection requests = %d, want 1 per stable config", got)
	}
}
