# Immutable Task 6 Lane M2 Special Metadata Owners Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** migrate the five non-ordinary metadata owners to immutable generation-local metadata, preserve their package-specific precedence and lifecycle semantics, and leave last-good/fail-closed selection exclusively in C6.5.

**Architecture:** a concrete compiler `MetadataPreparer` converts only the exact final HTTP publication candidate into one immutable `runtime.MetadataView` after attempt registration. It generically materializes both current `plugin_metadata` declaration owners (`azure-functions` and `error-log-logger`); M1 consumes the resulting Azure view while this lane owns the error-log special lifecycle. Four request/runtime owners decode the view once during construction; `error_log_logger` additionally binds its process-global observer and asynchronous resources to the prepared generation's task/stop lifetime. No package polls Store or maintains a second last-good cache.

**Tech Stack:** Go 1.26, `runtime.MetadataView`, `compiler.PreparationAttempt`, capability declaration catalog, attempt-scoped secret materialization, Casbin, OpenTelemetry SDK, `runtime.TaskRegistry`, focused race tests.

**Spec:** `docs/superpowers/plans/2026-08-24-immutable-task6-c6.4-plugin-runtime.md` Task 9, with ownership boundaries inherited from `docs/superpowers/plans/2026-08-24-immutable-task6-lane-m1-metadata.md` and `docs/superpowers/plans/2026-08-24-immutable-task6-lane-s2-raw-resolvers.md` Task 16.

## Global Constraints

- Every worktree starts from the current Task 6 integration HEAD **after all named dependencies in its row are merged**. Record that SHA with `git rev-parse HEAD`; do not hard-code an older checkpoint.
- Leaf workers modify only their exclusive files, return a diff plus command evidence, and do not commit, push, open a PR, merge, or delegate recursively. The integration owner reviews and creates each checkpoint commit.
- `error_log_logger` starts only after S2-E1 is merged. S2 owns `plugin_config` secret fields and private send-path installation; M2 owns `plugin_metadata`, metadata/route selection, observer registration, task binding, and retirement.
- Raw metadata schema admission and C6.5 refinement happen before final attempt registration. Metadata secret materialization happens only after registration.
- Last-good/fail-closed belongs only to `Compiler.PreparePublication`: valid predecessor means per-resource last-good; first startup without a valid predecessor fails closed. No M2 package stores, reloads, retries, or polls a last-good value.
- `opentelemetry` is the canonical metadata key and `otel` is its compatibility alias. Precedence is exactly `plugin_metadata > plugin_attr > defaults`; canonical key wins over alias within the same source.
- Request handlers, log callbacks, and trace finalizers never call `MetadataView.Decode`, Store, a secret resolver, or a schema compiler.
- Public plugin config, logs, and errors must not expose a raw reference, environment name, Vault path, ciphertext, or resolved secret. The private generation-local `MetadataView` necessarily carries resolved fields to its prepared consumers. `secret.Value` strings cannot be claimed as zeroed; retirement drops all generation references and closes attempt-owned state.
- Prefix every Go command with `source .envrc && export GOFLAGS=-mod=readonly`. Use impact-scoped tests; do not run the repository-wide unit aggregation or `make test`.
- Format only touched Go files. Inspect every generated diff and reject unrelated rewrites.

---

## Frozen Current-State Inventory

| Owner | Current mutable/global behavior | Immutable target | M2-specific proof |
| --- | --- | --- | --- |
| `chaitin_waf` | every request calls `GetPluginMetadataRaw`, compares bytes, then calls `GetPluginMetadata`; a mutex protects an instance cache | decode once in `PostInit`, compute route-over-metadata-over-default effective config once, and use it for every request | N and N+1 keep different node/mode/config values; concurrent requests perform zero metadata work |
| `authz_casbin` | metadata-backed requests call Store and rebuild the enforcer when metadata changes | decode once and construct exactly one immutable enforcer per plugin instance | N policy remains active while N+1 has a different policy; no request-time mutex or Store call |
| `batch_requests` | `NewHandler` seeds and reloads Store-owned validated metadata; Store owns last-good | decode one final view into `Limits` and construct a fixed handler | N/N+1 enforce different limits; invalid desired is decided in compiler, not in the handler |
| `error_log_logger` | Builder manually treats `Schema` as metadata schema, materializes through the legacy backend, replaces a process-global observer, and owns batch/Kafka shutdown outside the prepared task graph | explicit metadata schema, compiler-owned metadata secret materialization, one prepared instance, generation task/stop ownership | exact metadata secret scopes, replacement-safe N/N+1 observer overlap, task cancellation, reverse/idempotent cleanup |
| `otel` | `loadMetadata` reads `plugin_attr` and then Store using only the canonical key | decode canonical/alias view once, overlay it over canonical/alias plugin attributes, then defaults; build one provider | full alias/precedence matrix, N/N+1 provider independence, provider shutdown on retirement |

The compiler metadata producer is part of M2 because special metadata preparation needs one exact attempt-bound owner. It is generic: it may not contain a factory switch or a private secret-field table. The manifest currently declares:

| Factory | Source | Field | Strict |
| --- | --- | --- | --- |
| `azure-functions` | `plugin_metadata` | `master_apikey` | false |
| `error-log-logger` | `plugin_metadata` | `clickhouse.password` | true |
| `error-log-logger` | `plugin_metadata` | `kafka.brokers.*.sasl_config.password` | true |

`azure_functions` package migration remains M1 ownership. C0 supplies its materialized view contract before the M1 Azure leaf lands; the M1 leaf adds the real schema witness and consumes that view without rematerializing it.

## Interfaces and Exact Ownership

M2 consumes the already-landed contracts without changing their signatures:

```go
type MetadataPreparer interface {
	PrepareMetadata(context.Context, PreparationAttempt) (runtime.MetadataView, error)
}

func (attempt PreparationAttempt) Candidate(generation.Domain) (generation.PublicationCandidate, bool)
func (attempt PreparationAttempt) Occurrences(capability.SecretDeclarationSource) []FactoryOccurrence
func (attempt PreparationAttempt) MaterializeSecret(
	context.Context,
	FactoryOccurrence,
	string,
	string,
) (secret.Value, error)
```

The concrete producer added by this plan is deliberately package-private:

```go
type metadataPreparer struct {
	schemas *schemaSet
}

func newMetadataPreparer(schemas *schemaSet) (*metadataPreparer, error)
func (p *metadataPreparer) PrepareMetadata(
	ctx context.Context,
	attempt PreparationAttempt,
) (runtime.MetadataView, error)
```

`error_log_logger` adds one narrow lifecycle seam that CP5's prepared plugin owner can recognize without importing the package:

```go
func (p *Plugin) StartObservingWithTasks(tasks *runtime.TaskRegistry) error
```

The current parameterless `StartObserving` remains a transitional old-Builder caller. It delegates to observer installation but cannot invent a background registry. C6.6 records the live call; Task 7 supplies detached immutable observation ownership, and Task 9 deletes this method with the old Builder path.

## Dependency and Worktree Topology

```text
CP3 final-attempt boundary + already-landed runtime.MetadataView/BasePlugin seam
  -> M2-C0 compiler metadata preparer core
  -> M2-W chaitin_waf --------+
  -> M2-C authz_casbin -------+
  -> M2-B batch_requests -----+--> M2-I compiler integration/last-good corpus
  -> M2-O otel ---------------+         -> lane acceptance

S2-E1 error_log_logger plugin_config secrets
  + M2-C0 --------------------> M2-E error_log_logger ----+
```

| Worktree | Must branch after | Exclusive files | Integration checkpoint |
| --- | --- | --- | --- |
| `m2-c0-metadata-preparer` | CP3.4 plus the already-landed `runtime.MetadataView` / `BasePlugin.MetadataView` seam; it does not wait for M1 leaf completion | `pkg/compiler/metadata_preparer.go`, `pkg/compiler/metadata_preparer_test.go` | `feat(compiler): materialize immutable metadata views` |
| `m2-w-chaitin` | M2-C0 | `pkg/plugin/chaitin_waf/plugin.go`, `pkg/plugin/chaitin_waf/plugin_test.go` | `refactor(chaitin-waf): bind generation metadata` |
| `m2-c-casbin` | M2-C0 | `pkg/plugin/authz_casbin/plugin.go`, `pkg/plugin/authz_casbin/plugin_test.go` | `refactor(authz-casbin): freeze metadata enforcers` |
| `m2-b-batch` | M2-C0 | `pkg/plugin/batch_requests/plugin.go`, `pkg/plugin/batch_requests/plugin_test.go` | `refactor(batch-requests): bind immutable limits` |
| `m2-o-otel` | M2-C0 | `pkg/plugin/otel/plugin.go`, `pkg/plugin/otel/provider.go`, `pkg/plugin/otel/plugin_test.go` | `refactor(opentelemetry): bind immutable metadata` |
| `m2-e-error-log` | M2-C0 and S2-E1 | `pkg/plugin/error_log_logger/plugin.go`, `pkg/plugin/error_log_logger/plugin_test.go` | `refactor(error-log-logger): own metadata observer lifecycle` |
| `m2-i-integration` | all six checkpoints reviewed and merged | `pkg/compiler/special_metadata_test.go`, support/guard tests only if required by their existing owner | `test(compiler): enforce special metadata generations` |

W/C/B/O can run in parallel. E can join that group only after S2-E1 is in its baseline. No worker resolves same-file conflicts: if a dependency touched an owned package, recreate the worktree from the merged integration HEAD.

---

### Task 1: M2-C0 — Implement the exact final metadata preparer

**Files:**
- Create: `pkg/compiler/metadata_preparer.go`
- Create: `pkg/compiler/metadata_preparer_test.go`

**Interfaces:**
- Consumes: `PreparationAttempt`, `FactoryOccurrence`, `schemaSet.catalog`, `normalizeContext`, `generation.ValidatePublicationCandidate`, `runtime.NewMetadataView`.
- Produces: `newMetadataPreparer(*schemaSet)` and a `MetadataPreparer` that contains no Store or raw resolver access.

- [ ] **Step 1: Write the red exact-final-view tests**

Add tests named:

```go
func TestMetadataPreparerUsesExactFinalPublishedDocuments(t *testing.T)
func TestMetadataPreparerRejectsMissingDuplicateOrForeignOccurrence(t *testing.T)
func TestMetadataPreparerReturnsEmptyViewForStreamOnlyAttempt(t *testing.T)
func TestMetadataPreparerMaterializesAzureMetadataWithExactOccurrence(t *testing.T)
func TestMetadataPreparerKeepsCandidateAndRecoveryAttemptsDistinct(t *testing.T)
```

The no-secret success fixture contains an HTTP `plugin_metadata/chaitin-waf` document:

```json
{"nodes":[{"host":"127.0.0.1","port":8000}],"mode":"monitor"}
```

Assert `view.Decode("chaitin-waf", &got)` returns that object, the source snapshot still contains the original bytes, a tombstoned metadata document is absent, and zero secret calls occur because that factory has no metadata declarations. Construct duplicate/foreign HTTP occurrences with the existing package-private attempt helpers and prove rejection happens before resolver access. A stream-only attempt must return an empty view and perform zero metadata/secret work because `generation.DomainsForResourceKind("plugin_metadata")` is exactly HTTP.

The Azure exact-secret fixture is:

```json
{"master_apikey":"$ENV://AZURE_MASTER_APIKEY","master_clientid":"client-n"}
```

Until the M1 Azure leaf adds the real package schema witness, this direct hook test replaces only the test compiler's `azure-functions` `factorySchemas.metadata` with a locally compiled schema accepting those two optional strings, then manually constructs an authorized final HTTP attempt with the existing test registration helpers. It does not call `PreparePublication` with a witness that has not landed. This is a test bridge, not production source of truth; Task 7 removes the override and repeats the assertion through the real witness and full `attemptFactory`. Require one call with resource `plugin_metadata/azure-functions`, factory `azure-functions`, source `plugin_metadata`, field `master_apikey`, domain HTTP, and the current attempt ID. `master_clientid` causes no materialization call.

- [ ] **Step 2: Run red**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/compiler -run "^TestMetadataPreparer" -count=1'
```

Expected: FAIL because `metadataPreparer` and `newMetadataPreparer` do not exist.

- [ ] **Step 3: Implement deterministic occurrence indexing**

Use this exact identity and reject every duplicate:

```go
type metadataOccurrenceKey struct {
	resource generation.ResourceKey
	factory  string
}
```

Only accept source `capability.SecretPluginMetadata`, domain `generation.DomainHTTP`, resource kind `plugin_metadata`, and `resource.ID == factory`. Read only `attempt.Candidate(generation.DomainHTTP)` and iterate resources through `normalizedInput.keys()`. A missing HTTP candidate produces an empty view only when the occurrence set is also empty. Tombstones never create documents or occurrences; any stream metadata occurrence is invalid authority rather than a second metadata namespace.

- [ ] **Step 4: Implement catalog-driven materialization and resolved validation**

For each final HTTP metadata resource:

1. validate the candidate with `generation.ValidatePublicationCandidate`;
2. normalize the exact candidate snapshot and require no issues;
3. clone the decoded document;
4. call `catalog.TransformDeclaredFields(factory, SecretPluginMetadata, document, callback)`;
5. materialize only string fields using the exact occurrence and `declaration.Field`;
6. use the value only inside `Value.Use` to replace that leaf in the private document;
7. validate the resolved document with `schemas.factories[factory].metadata`;
8. marshal it and pass it to `runtime.NewMetadataView`.

Use one stable wrapper error:

```go
var errMetadataPreparationFailed = fmt.Errorf(
	"%w: metadata preparation failed",
	ErrInvalidInput,
)
```

Return `ctx.Err()` unchanged. Never wrap an underlying resolver/schema error because it may contain a reference or plaintext. Metadata is HTTP-only, so no domain merge exists. Candidate and recovery attempts may carry different committed HTTP artifact revisions, but every materialization scope remains `DomainHTTP`; `Scope.Generation` is the desired generation and `Scope.Attempt` distinguishes candidate from recovery.

- [ ] **Step 5: Add redaction, cancellation, and empty-view tests**

Add:

```go
func TestMetadataPreparerMaterializationFailureIsRedacted(t *testing.T)
func TestMetadataPreparerReturnsEmptyViewForNoPublishedMetadata(t *testing.T)
func TestMetadataPreparerHonorsCancellationBeforeResolverAccess(t *testing.T)
```

The redaction test broker returns an error containing `VAULT_PATH_DO_NOT_LEAK` and the raw fixture uses `$secret://vault/private/path`; assert neither substring occurs in the returned error.

- [ ] **Step 6: Run green and race**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/compiler -run "^TestMetadataPreparer" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/compiler -run "^TestMetadataPreparer" -count=1'
```

Expected: PASS.

- [ ] **Step 7: Return the leaf handoff**

Return `git diff -- pkg/compiler/metadata_preparer.go pkg/compiler/metadata_preparer_test.go`, the two command outputs, and `git status --short`. The integration owner inspects the exact path set and creates the checkpoint commit.

---

### Task 2: M2-W — Freeze `chaitin_waf` effective metadata

**Files:**
- Modify: `pkg/plugin/chaitin_waf/plugin.go`
- Modify: `pkg/plugin/chaitin_waf/plugin_test.go`

**Interfaces:**
- Consumes: `BasePlugin.MetadataView().Decode("chaitin-waf", &Metadata{})`.
- Produces: one instance-owned `effectiveConfig`; request code has no Store/cache path.

- [ ] **Step 1: Replace the polling tests with red generation tests**

Delete Store fixture globals and replace `TestHandlerRebuildsMetadataOnlyWhenConfigurationChanges` / `TestHandlerConcurrentRequestsShareStableMetadata` with:

```go
func TestPreparedGenerationsRetainChaitinMetadata(t *testing.T)
func TestChaitinRouteConfigOverridesMetadataThenDefaults(t *testing.T)
func TestConcurrentRequestsUsePreparedChaitinMetadata(t *testing.T)
```

N uses node A, `mode=monitor`, and `read_timeout=25`; N+1 uses node B, `mode=block`, and `read_timeout=50`. Keep both plugins alive, send requests to both, and assert each contacts only its own server. In the precedence test, set only route `mode` and `config.req_body_size`; metadata supplies nodes/read timeout; defaults supply connect/send/keepalive values.

- [ ] **Step 2: Run red**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/chaitin_waf -run "(PreparedGenerationsRetainChaitinMetadata|RouteConfigOverridesMetadata|ConcurrentRequestsUsePrepared)" -count=1'
```

Expected: FAIL because `PostInit` still polls Store and the injected view is not authoritative.

- [ ] **Step 3: Decode and freeze once**

Replace `metaMu`, `metaRaw`, `metaCfg`, `metaValid`, and `metaBuilds` with:

```go
effective effectiveConfig
```

In `PostInit`, preserve the raw route config before applying defaults, decode metadata once, and construct exact precedence:

```text
defaults -> plugin_metadata -> explicitly supplied route fields
```

`Mode`, `Nodes`, and every non-zero/non-nil `WAFConfig` route field override metadata; absent route values do not overwrite metadata with defaults. Build the HTTP client from `effective.Config.ReadTimeout`. `doAccess` reads `p.effective` directly.

- [ ] **Step 4: Remove mutable metadata machinery**

Delete `effectiveConfig()`, `buildEffectiveConfig()`, `loadMetadata()`, the Store import, raw-byte comparison, validation logging, and the metadata cache mutex. Keep `validateMetadata` only if a package test still directly proves the schema; compiler raw admission remains authoritative.

- [ ] **Step 5: Run package and race gates**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/chaitin_waf -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/chaitin_waf -run "(PreparedGenerationsRetain|ConcurrentRequestsUsePrepared|MovesPastFailedWAFNode)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && ! rg -n "GetPluginMetadata|GetPluginMetadataRaw|metaRaw|metaBuilds" pkg/plugin/chaitin_waf -g "*.go" -g "!*_test.go"'
```

- [ ] **Step 6: Return the leaf handoff**

Return the two-file diff and evidence. The integration owner creates `refactor(chaitin-waf): bind generation metadata` after review.

---

### Task 3: M2-C — Construct one `authz_casbin` enforcer per generation

**Files:**
- Modify: `pkg/plugin/authz_casbin/plugin.go`
- Modify: `pkg/plugin/authz_casbin/plugin_test.go`

**Interfaces:**
- Consumes: immutable metadata key `authz-casbin`.
- Produces: explicit metadata schema and a request-time read-only `*casbin.SyncedEnforcer`.

- [ ] **Step 1: Write red schema and N/N+1 tests**

Add `metadataSchema` requiring non-empty string `model` and `policy`. Add:

```go
func TestMetadataSchemaRequiresCasbinModelAndPolicy(t *testing.T)
func TestPreparedGenerationsRetainCasbinMetadata(t *testing.T)
func TestRouteCasbinConfigOverridesPreparedMetadata(t *testing.T)
```

For N allow Alice only; for N+1 allow Bob only. Keep both instances active and assert four requests: N/Alice allowed, N/Bob forbidden, N+1/Alice forbidden, N+1/Bob allowed. The route precedence test supplies inline route model/policy and conflicting metadata, proving route config wins.

- [ ] **Step 2: Run red**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/authz_casbin -run "(MetadataSchemaRequires|PreparedGenerationsRetain|RouteCasbinConfigOverrides)" -count=1'
```

- [ ] **Step 3: Freeze the enforcer in `PostInit`**

Set `p.MetadataSchema = metadataSchema` in `Init`. In `PostInit`, decode metadata exactly once. If the route contains a complete path or inline pair, construct from route config. Otherwise require a found metadata document and build from its model/policy. Store only the constructed enforcer.

Make `currentEnforcer` a nil check:

```go
func (p *Plugin) currentEnforcer() (*casbin.SyncedEnforcer, error) {
	if p.enforcer == nil {
		return nil, errors.New("casbin enforcer is not initialized")
	}
	return p.enforcer, nil
}
```

Delete `metadataEnforcer`, `metadata`, `mu`, `sync`, and the Store import. Request handling must never rebuild or lock around metadata.

- [ ] **Step 4: Replace legacy reload/concurrency tests**

Delete Store event fixtures and `TestHandlerReloadsCasbinPluginMetadata`. Keep enforcement concurrency coverage, but run concurrent requests against the already-built N and N+1 instances without mutating metadata.

- [ ] **Step 5: Run package and race gates**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/authz_casbin -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/authz_casbin -run "(PreparedGenerationsRetain|ConcurrentEnforce)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && ! rg -n "pkg/store|GetPluginMetadata|metadataEnforcer" pkg/plugin/authz_casbin -g "*.go" -g "!*_test.go"'
```

- [ ] **Step 6: Return the leaf handoff**

Return the two-file diff and evidence. The integration owner creates `refactor(authz-casbin): freeze metadata enforcers` after review.

---

### Task 4: M2-B — Bind `batch_requests` to immutable limits

**Files:**
- Modify: `pkg/plugin/batch_requests/plugin.go`
- Modify: `pkg/plugin/batch_requests/plugin_test.go`

**Interfaces:**
- Consumes: `runtime.MetadataView.Decode("batch-requests", &Limits{})`.
- Produces: `NewHandlerFromMetadata(http.Handler, runtime.MetadataView) (http.Handler, error)` and fixed per-handler limits.

- [ ] **Step 1: Write red construction and overlap tests**

Add:

```go
func TestNewHandlerFromMetadataDecodesFinalLimitsOnce(t *testing.T)
func TestPreparedGenerationsRetainBatchRequestLimits(t *testing.T)
func TestNewHandlerFromMetadataUsesDefaultsWhenDocumentIsAbsent(t *testing.T)
```

N sets `max_pipeline_items=1`, N+1 sets `max_pipeline_items=2`. Build both handlers before sending requests; a two-item request fails against N and succeeds against N+1. Mutating the source map after `runtime.NewMetadataView` must not affect either handler.

- [ ] **Step 2: Run red**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/batch_requests -run "(NewHandlerFromMetadata|PreparedGenerationsRetainBatchRequestLimits)" -count=1'
```

- [ ] **Step 3: Implement one-time decode**

Add:

```go
func NewHandlerFromMetadata(
	dispatcher http.Handler,
	view runtime.MetadataView,
) (http.Handler, error) {
	var limits Limits
	if _, err := view.Decode(name, &limits); err != nil {
		return nil, fmt.Errorf("batch-requests metadata decode failed: %w", err)
	}
	return NewHandlerWithLimits(dispatcher, applyLimitDefaults(limits)), nil
}
```

Keep `NewHandlerWithLimits` as the pure constructor. Change transitional `NewHandler` to default-only construction with no Store access. Task 7's HTTP compiler calls `NewHandlerFromMetadata`; C6.6 records the remaining old-route caller, and Task 9 deletes `NewHandler` with the Builder path.

- [ ] **Step 4: Delete the package last-good implementation**

Delete `newMetadataHandler`, `loadLimits`, `compiledBatchLimitsSchema` if it has no remaining package use, the Store import, and the `errors` import used only for `store.ErrNotFound`. Replace the two legacy metadata-handler tests with immutable construction tests. Do not add an atomic pointer, loader callback, refresh goroutine, or last-good field.

- [ ] **Step 5: Run package and race gates**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/batch_requests -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/batch_requests -run "(PreparedGenerationsRetain|ParentCancellation|TimeoutJoins|BoundsCancellation)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && ! rg -n "pkg/store|GetValidatedPluginMetadata|newMetadataHandler|loadLimits" pkg/plugin/batch_requests -g "*.go" -g "!*_test.go"'
```

- [ ] **Step 6: Return the leaf handoff**

Return the diff and evidence, including an explicit Task 7/9 handoff: detached HTTP compilation must call `NewHandlerFromMetadata`, and transitional `NewHandler` remains until the joint production cutover deletes `pkg/route/extra.go`'s old call.

---

### Task 5: M2-O — Preserve OpenTelemetry aliases and precedence

**Files:**
- Modify: `pkg/plugin/otel/plugin.go`
- Modify: `pkg/plugin/otel/provider.go`
- Modify: `pkg/plugin/otel/plugin_test.go`

**Interfaces:**
- Consumes: metadata keys `opentelemetry` then `otel`; plugin attributes with the same canonical-first order.
- Produces: one generation-owned tracer provider built from `plugin_metadata > plugin_attr > defaults`.

- [ ] **Step 1: Write the red precedence matrix**

Add:

```go
func TestMetadataSchemaAcceptsOpenTelemetryDocument(t *testing.T)
func TestLoadMetadataPrecedenceAndAliases(t *testing.T)
func TestPreparedGenerationsRetainOpenTelemetryMetadata(t *testing.T)
func TestStopShutsDownOnlyItsOpenTelemetryProvider(t *testing.T)
```

Use table rows:

| Row | View | Plugin attr | Expected |
| --- | --- | --- | --- |
| defaults | none | none | random, `127.0.0.1:4318`, timeout 3, not configured |
| canonical attr | none | `opentelemetry` | canonical attr |
| alias attr | none | `otel` | alias attr |
| both attrs | none | both | `opentelemetry` |
| alias metadata over canonical attr | `otel` | `opentelemetry` | alias metadata |
| canonical metadata over alias metadata | both | both | `opentelemetry` metadata |

N and N+1 use different `trace_id_source`, resource service name, collector address, and headers. Assert one instance's metadata/provider state never changes after constructing the other.

- [ ] **Step 2: Run red**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/otel -run "(MetadataSchemaAccepts|LoadMetadataPrecedenceAndAliases|PreparedGenerationsRetain|StopShutsDownOnly)" -count=1'
```

- [ ] **Step 3: Add the explicit metadata schema witness**

Define `metadataSchema` for `trace_id_source`, `resource`, `collector.address`, `collector.request_timeout`, `collector.request_headers`, `batch_span_processor` numeric/bool fields, and `set_ngx_var`. Keep schema acceptance separate from `validateMetadata`: `set_ngx_var=true` and non-zero `inactive_timeout` remain schema-valid but fail with the existing bounded unsupported-feature errors before provider allocation. Set `p.MetadataSchema` in `Init`.

- [ ] **Step 4: Replace Store lookup with deterministic overlay**

Change the loader to:

```go
func loadMetadata(
	view runtime.MetadataView,
	pluginAttr map[string]map[string]any,
) (Metadata, bool, error)
```

Select canonical attr, else alias attr. Then decode canonical metadata; if absent decode alias metadata. A found metadata document replaces the entire attribute document, matching current Store-over-attribute behavior; it does not deep-merge. Apply defaults only after the source is selected. Delete `safeGetPluginMetadata` and the Store import.

- [ ] **Step 5: Bind the provider and cleanup once**

`PostInit` calls the new loader once and stores the selected metadata. Preserve the current fallback provider behavior for ordinary exporter construction errors and the fail-before-allocation behavior for `errUnsupportedMetadata`. Make `Stop` idempotent with `sync.Once` so concurrent retirement cannot call provider shutdown twice.

- [ ] **Step 6: Run package and race gates**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/otel -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/otel -run "(PreparedGenerationsRetain|StopShutsDownOnly|TraceStarts|TraceRequestPhase)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && ! rg -n "pkg/store|safeGetPluginMetadata|GetPluginMetadata" pkg/plugin/otel -g "*.go" -g "!*_test.go"'
```

- [ ] **Step 7: Return the leaf handoff**

Return the three-file diff and evidence. The integration owner verifies both registry keys still map to `otel.Plugin` and creates `refactor(opentelemetry): bind immutable metadata`.

---

### Task 6: M2-E — Own `error_log_logger` metadata, observer, and task retirement

**Depends on:** M2-C0 and the accepted S2-E1 implementation.

**Files:**
- Modify: `pkg/plugin/error_log_logger/plugin.go`
- Modify: `pkg/plugin/error_log_logger/plugin_test.go`
- Do not modify: `pkg/plugin/error_log_logger/manifest_test.go`

**Interfaces:**
- Consumes: resolved immutable metadata key `error-log-logger`, S2's private ClickHouse/Kafka secret-installation helpers, `runtime.TaskRegistry`.
- Produces: explicit metadata schema, metadata-over-empty/system selection, route-over-metadata compatibility, task-bound observer registration, idempotent `Stop`.

- [ ] **Step 1: Verify the S2 handoff before editing**

Run:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/error_log_logger -run "(MaterializeScopedSecrets|ClickHousePassword|KafkaPassword|SASL|SendLogs)" -count=1'
```

Inspect the diff that introduced S2-E1. Record the names of its private secret fields/helpers and preserve them; do not restore `DataEncryption`, `ResolveForContext`, or plaintext public config.

- [ ] **Step 2: Write red metadata selection and secret-consumption tests**

Add:

```go
func TestMetadataSchemaIsExplicitForErrorLogLogger(t *testing.T)
func TestPostInitUsesPreparedErrorLogMetadata(t *testing.T)
func TestRouteErrorLogConfigOverridesPreparedMetadata(t *testing.T)
func TestPreparedGenerationsRetainErrorLogMetadata(t *testing.T)
func TestPreparedErrorLogMetadataSecretsArePrivateAndRedacted(t *testing.T)
```

The metadata success fixture uses TCP N and TCP N+1 with different listeners and levels. The route precedence fixture gives route TCP plus conflicting metadata ClickHouse and proves no ClickHouse request is attempted. The secret test injects an already-resolved ClickHouse password and two already-resolved Kafka passwords through `MetadataView`, verifies the instance public config contains only `plugin_metadata#sha256:<digest>` descriptors, and verifies the outgoing ClickHouse header/Kafka mechanism receives the resolved values. Assert the package never invokes S2's `plugin_config` materializer for a metadata-sourced instance; M2-C0 already performed that attempt-scoped resolution.

- [ ] **Step 3: Run red**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/error_log_logger -run "(MetadataSchemaIsExplicit|PostInitUsesPrepared|RouteErrorLogConfigOverrides|PreparedGenerationsRetain)" -count=1'
```

- [ ] **Step 4: Install explicit schema and selection**

Keep `p.Schema = schema` during the additive window and also set `p.MetadataSchema = schema`. Decode `MetadataView` once in `PostInit`. If route/plugin config explicitly names any sink or legacy host/port, route config wins; otherwise require and use the metadata document. Apply defaults and allocate client, Kafka sender, and batch processor only after selection succeeds.

Use a named helper that checks all sink fields rather than reflection:

```go
func hasExplicitConfig(config Config) bool {
	return config.TCP != nil || config.Skywalking != nil || config.Clickhouse != nil ||
		config.Kafka != nil || config.Host != "" || config.Port != 0
}
```

Decode even when route config wins so malformed immutable-view state fails construction rather than becoming a conditional request-time error.

For a metadata-selected instance, immediately move the three declared secret paths out of the selected public config:

```go
type metadataSecret struct {
	plaintext  string
	descriptor secret.Descriptor
}

func newMetadataSecret(plaintext string) (metadataSecret, error) {
	digest := sha256.Sum256([]byte(plaintext))
	descriptor, err := secret.NewDescriptor(capability.SecretPluginMetadata, digest)
	if err != nil {
		return metadataSecret{}, err
	}
	return metadataSecret{plaintext: plaintext, descriptor: descriptor}, nil
}
```

Store one private ClickHouse value and private Kafka values keyed by broker index; replace their `Config` strings with `descriptor.String()`. Make `sendToClickHouse` use one common `useClickHousePassword(func(string) error) error` helper that selects S2's `secret.Value` for plugin config or the M2 private metadata value for metadata. Refactor Kafka writer construction to accept a resolved private clone; S2 populates that clone from its `secret.Value` objects, M2 populates it from the indexed metadata values, and the public descriptor-bearing config is never passed to SASL. On `Stop`, assign empty strings/nil to M2 private references after transports close; report this as dropping references, never as wiping Go strings.

If `newMetadataSecret` fails, return a fixed `error-log-logger metadata secret installation failed` error. Do not include the plaintext, descriptor input, broker address, or underlying error.

- [ ] **Step 5: Write red observer/task lifetime tests**

Add:

```go
func TestStartObservingWithTasksRejectsMissingOrStoppedRegistry(t *testing.T)
func TestPreparedGenerationsReplaceErrorLogObserverSafely(t *testing.T)
func TestTaskStopUnregistersObserverAndClosesResourcesOnce(t *testing.T)
func TestPreparationFailureClosesErrorLogResourcesInReverseOrder(t *testing.T)
```

For N/N+1: start N with registry N, start N+1 with registry N+1, retire N, emit a marker, and prove only N+1 receives it. Then stop registry N+1 and prove no delivery occurs. Count observer stop, batch stop, in-flight send completion, and Kafka close; each must be exactly one.

- [ ] **Step 6: Bind observer lifetime to the task registry**

Implement `StartObservingWithTasks` so admission is transactional:

1. reject nil tasks before installing an observer;
2. install with `logger.ReplaceObserver`;
3. register owner `plugin/error-log-logger/observer` with criticality `runtime.TaskPlugin`;
4. the task waits for `ctx.Done()`, calls `p.Stop()`, and returns nil;
5. if `TaskRegistry.Go` rejects admission, invoke the just-created stop function and clear it before returning the stable error.

Keep `Stop` protected by the existing `stopOnce`. It unregisters this generation's observer first, then stops/drains the batch processor, then closes the Kafka writer/private secret-backed clients. `ReplaceObserver` generation identity guarantees retiring N cannot remove N+1.

- [ ] **Step 7: Run package and isolated race gates**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/error_log_logger -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race -p=1 ./pkg/plugin/error_log_logger -run "(PreparedGenerationsRetain|StartObservingWithTasks|TaskStop|StopUnregisters|StopWaits|MaterializeScopedSecrets)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && ! rg -n "GetPluginMetadata|GetPluginMetadataRaw|GetValidatedPluginMetadata" pkg/plugin/error_log_logger -g "*.go" -g "!*_test.go"'
```

- [ ] **Step 8: Return the leaf handoff**

Return the two-file diff and evidence. Explicitly identify the C6.6 transitional parameterless `StartObserving` call if it remains. The integration owner creates `refactor(error-log-logger): own metadata observer lifecycle` after checking S2 code was preserved.

---

### Task 7: M2-I — Prove metadata secrets and C6.5 last-good/fail-closed ownership

**Files:**
- Create: `pkg/compiler/special_metadata_test.go`
- Modify: `pkg/compiler/metadata_preparer_test.go` only if the integration owner can do so without overwriting the accepted C0 diff.

**Interfaces:**
- Consumes: the five M2 schemas, the M1-owned Azure metadata schema, and the generic metadata preparer.
- Produces: one cross-package regression corpus proving plugins have no local fallback responsibility.

- [ ] **Step 1: Replace the Azure test bridge with the real witness**

After the M1 Azure leaf is merged, delete the C0 test's injected `factorySchemas.metadata` override. Prepare this exact document through the real `attemptFactory`:

```json
{"master_apikey":"$ENV://AZURE_MASTER_APIKEY","master_clientid":"client-n"}
```

Require exactly one HTTP `plugin_metadata/azure-functions`, `azure-functions`, `master_apikey` scope. Decode the final view and prove `master_apikey` is resolved while `master_clientid` is unchanged. Recovery uses the committed HTTP artifact revision but sets `Scope.Generation` to `RevisionSet.Desired` and uses a distinct recovery AttemptID.

- [ ] **Step 2: Add the exact error-log metadata secret test**

Create a final HTTP candidate with:

```json
{
  "clickhouse": {
    "endpoint_addr": "http://127.0.0.1:8123",
    "user": "default",
    "password": "$ENV://ERROR_LOG_CLICKHOUSE_PASSWORD",
    "database": "apisix",
    "logtable": "error_log"
  }
}
```

Prepare through the real `attemptFactory` and assert exactly one scope:

```go
secret.Scope{
	Generation: revision,
	Attempt: prepared.attempt.AttemptID(),
	Domain: generation.DomainHTTP,
	Plugin: "error-log-logger",
	Resource: generation.ResourceKey{Kind: "plugin_metadata", ID: "error-log-logger"},
	Source: capability.SecretPluginMetadata,
	Field: "clickhouse.password",
}
```

Add a Kafka document with two brokers and require two calls using canonical field `kafka.brokers.*.sasl_config.password`. Assert candidate and recovery AttemptIDs are distinct, every scope is HTTP, every scope generation equals the desired generation, and closing one attempt does not revoke the other.

- [ ] **Step 3: Run the new secret tests**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/compiler -run "SpecialMetadata.*Secret|MetadataPreparer.*(Azure|ErrorLog)" -count=1'
```

- [ ] **Step 4: Add a table-driven last-good/fail-closed corpus**

Use these invalid desired documents against valid predecessors:

```go
map[string]string{
	"chaitin-waf":     `{"nodes":[]}`,
	"authz-casbin":    `{"model":"model-without-policy"}`,
	"batch-requests":  `{"max_concurrency":0}`,
	"error-log-logger": `{"level":"WARN"}`,
	"opentelemetry":   `{"collector":{"request_timeout":"bad"}}`,
}
```

For each row prove:

- valid predecessor exists: the final candidate reuses exactly the predecessor resource bytes and records `DispositionLastGood`;
- no predecessor exists: the resource records `DispositionFailClosed` and `attemptFactory.prepareCandidateAttempt` performs zero registration, zero secret resolution, zero plugin construction, and returns failure;
- the package under test has no cache/load/retry call because all behavior occurs before its hook.

- [ ] **Step 5: Add N/N+1 metadata view independence under race**

Prepare two complete attempts containing all five metadata documents. Concurrently decode both views 1,000 times, close N, and assert N+1 remains readable and its task/observer/provider owners remain live. Then close N+1. Do not claim closed `MetadataView` bytes are zeroed; assert only that the owning `PreparedGeneration` no longer exposes N at the later CP5 boundary.

- [ ] **Step 6: Run compiler integration gates**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/compiler -run "(SpecialMetadata|MetadataPreparer|LastGood|FailClosed)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/compiler -run "(SpecialMetadata|MetadataPreparer)" -count=1'
```

- [ ] **Step 7: Return integration evidence**

Return the compiler test diff and command output. The integration owner creates `test(compiler): enforce special metadata generations` after verifying no production factory switch was added.

---

### Task 8: Integrate M2 and run the acceptance checkpoint

**Ownership:** Task 6 integration owner only.

**Files:**
- Review and commit the six implementation diffs plus the integration-test diff.
- Do not modify `pkg/route/**`, `pkg/server/**`, or `cmd/**`; new production call sites remain Task 7 ownership and activation/deletion remains Task 9 ownership. C6.6 only records the compatibility inventory and may remove a legacy seam after proving it has zero callers.

**Interfaces:**
- Consumes: C0/W/C/B/O/E/I worker diffs and their focused evidence.
- Produces: reviewed Task 6 integration commits plus the exact C6.6 handoff; no master merge, push, or PR.

- [ ] **Step 1: Review and commit in dependency order**

Accept C0 first. W/C/B/O may be accepted in any order. Accept E only after verifying its baseline contains S2-E1. Accept I last. For every leaf run:

```bash
git diff --name-only <recorded-worktree-base>..HEAD
git diff --check
```

Workers do not create commits. The integration owner stages only the named files and uses the checkpoint messages in the topology table.

- [ ] **Step 2: Run all five package tests**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test \
  ./pkg/plugin/chaitin_waf ./pkg/plugin/authz_casbin ./pkg/plugin/batch_requests \
  ./pkg/plugin/error_log_logger ./pkg/plugin/otel -count=1'
```

- [ ] **Step 3: Run split race gates**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race \
  ./pkg/plugin/chaitin_waf ./pkg/plugin/authz_casbin ./pkg/plugin/batch_requests ./pkg/plugin/otel \
  -run "(PreparedGenerationsRetain|Concurrent|StopShutsDown|ParentCancellation)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race -p=1 \
  ./pkg/plugin/error_log_logger \
  -run "(PreparedGenerationsRetain|StartObservingWithTasks|TaskStop|Stop|MaterializeScopedSecrets)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/compiler \
  -run "(SpecialMetadata|MetadataPreparer)" -count=1'
```

- [ ] **Step 4: Run schema, alias, source-boundary, and generator guards**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin \
  -run "(SchemaWitness|MetadataDependencyGuard|FactoryInstance)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/compiler \
  -run "(Schema|SpecialMetadata|MetadataPreparer|LastGood|FailClosed)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go run ./cmd/capability-gen -repo-root . -check'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && \
  ! rg -n "LoadPluginMetadata|GetPluginMetadataRaw|GetValidatedPluginMetadata|GetPluginMetadata\\(" \
    pkg/plugin/{chaitin_waf,authz_casbin,batch_requests,error_log_logger,otel} \
    -g "*.go" -g "!*_test.go"'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && rg -n \
  "\\\"opentelemetry\\\"|\\\"otel\\\"" pkg/plugin/registry_gen.go pkg/plugin/otel'
```

Expected: no M2 production Store metadata call; both OTel registry keys remain; all five metadata schemas are visible to compiler witnesses.

- [ ] **Step 5: Run scoped lint, build, and diff checks**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run \
  ./pkg/compiler/... ./pkg/plugin/chaitin_waf/... ./pkg/plugin/authz_casbin/... \
  ./pkg/plugin/batch_requests/... ./pkg/plugin/error_log_logger/... ./pkg/plugin/otel/...'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && make build'
git diff --check
```

- [ ] **Step 6: Perform the lifecycle and dead-fallback audit**

Confirm with code review and `rg`:

- no handler/finalizer/log callback decodes metadata;
- no M2 package retains a mutable metadata cache or last-good state;
- Azure and error-log metadata materialization use only the manifest catalog and exact HTTP occurrence authority; Azure is consumed by its M1 leaf without a second resolution;
- S2 plugin-config secret helpers remain intact and separate from metadata materialization;
- N observer retirement cannot remove N+1;
- OTel canonical/alias and `plugin_metadata > plugin_attr > defaults` tests exist;
- batch's only remaining transitional default constructor is named in the C6.6 handoff;
- parameterless error-log observer startup, if still present, is named in the C6.6 handoff;
- no files outside the declared M2 ownership entered the lane diff.

- [ ] **Step 7: Independent review checkpoint**

Request one read-only merge-level review of the M2 diff. The reviewer checks schema compatibility, exact secret scope, Store removal, alias/precedence, N/N+1 isolation, task cancellation, observer replacement safety, idempotent/reverse cleanup, and error redaction. Resolve findings only in M2-owned files and rerun the affected gate.

## Task 7/9 Handoff and C6.6 Ledger

M2 is accepted as a leaf lane only after recording these two explicit
current-production seams. C6.6 records them but does not inject a prepared
Builder path:

1. Task 7's detached HTTP compiler receives the final `runtime.MetadataView`
   and calls `batch_requests.NewHandlerFromMetadata`. The old
   `pkg/route/extra.go` caller and default-only `NewHandler` remain until Task 9
   deletes the Builder path.
2. Task 7's effective error-log spec is materialized through CP5, whose private
   lifecycle step calls `StartObservingWithTasks` exactly once. Task 9 deletes
   `configureGlobalErrorLogObserver`, `startGlobalErrorLogObserver`, and the
   parameterless observer interface with the old Builder after the detached
   owner is installed.

Those Task 7/9 edits do not belong to an M2 leaf worker or C6.6.

## Completion Criteria

- the concrete compiler metadata preparer consumes only the exact final HTTP candidate, returns empty for stream-only attempts, and materializes Azure/error-log declared metadata fields only after registration;
- all five packages expose valid metadata schema witnesses and have no production Store metadata call;
- chaitin, Casbin, batch, and OTel decode once during construction and retain immutable N/N+1 behavior;
- OTel preserves both aliases and exact `plugin_metadata > plugin_attr > defaults` precedence;
- Azure and error-log metadata secrets use exact attempt/HTTP-domain/resource/source/factory/field scopes; candidate and recovery attempts use distinct AttemptIDs while both scope generations equal the desired generation;
- error-log observer, batch processor, Kafka writer, and private secret-backed resources retire once with their generation; N retirement cannot remove N+1;
- invalid desired metadata uses C6.5 last-good only with a valid predecessor and fails closed without one;
- workers returned diffs/evidence only, the integration owner reviewed and committed each checkpoint, and no push/PR occurred;
- focused package/compiler tests, split race gates, schema/generator guards, scoped lint, build, diff check, and independent review have no unresolved M2 finding.
