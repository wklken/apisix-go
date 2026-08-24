# Immutable Task 6 Lane M1 Metadata Consumers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move every ordinary plugin metadata consumer from the process-global Store to the immutable `runtime.MetadataView` owned by its prepared generation, while preserving APISIX-compatible metadata keys, precedence, defaults, and generation overlap behavior.

**Architecture:** C6.5 validates and refines raw `plugin_metadata` documents before registration, then its metadata preparer materializes declared metadata secret fields and constructs one immutable `runtime.MetadataView`. Lane M1 changes only ordinary leaf plugins: each instance decodes its exact metadata document once during preparation/PostInit and retains the decoded value for its lifetime; handlers never consult Store or a newer view. M2 retains exclusive ownership of dynamic/special metadata lifecycles.

**Tech Stack:** Go 1.26, standard-library `go/ast`/`go/parser` guards, `runtime.MetadataView`, plugin `base.Dependencies`, JSON Schema witnesses, Go tests and race detector.

**Spec:** `docs/superpowers/plans/2026-08-24-immutable-task6-c6.4-plugin-runtime.md`

## Global Constraints

- CP3.4 architecture prerequisite is `40c04a261e5e7234ed2013714fa1368be807810d`; the actual leaf implementation baseline is integration commit `e5b6a73e` (`feat(secret): add redacted value descriptors`) or a later dependency-merged integration HEAD explicitly named below.
- Run every Go command from the active worktree as `source .envrc && export GOFLAGS=-mod=readonly && ...`.
- M1 owns the 23 ordinary packages frozen below. It must not modify `chaitin_waf`, `authz_casbin`, `batch_requests`, `error_log_logger`, or `otel`; those are M2 owners.
- `graphql-limit-count` intentionally reads the `limit-count` metadata document. Do not create, copy, or fall back to a `graphql-limit-count` metadata document.
- `opentelemetry`/`otel` aliases and `plugin_metadata > plugin_attr > defaults` precedence belong only to M2. M1 must not generalize that alias into a repository-wide fallback mechanism.
- Raw JSON Schema admission precedes attempt registration and metadata materialization. M1 receives only the finalized `runtime.MetadataView`; no leaf package may decrypt, resolve, poll, or read raw Store metadata.
- Metadata is decoded once per plugin instance during preparation/PostInit. Request, log, and body-filter paths use instance-owned values only.
- An absent metadata document preserves today's zero-value/default behavior. Decode failures return a redacted plugin/factory error and never include raw JSON, references, or secret values.
- Do not add a compatibility fallback from `MetadataView.Decode` to `base.LoadPluginMetadata` or `store.GetPluginMetadata*`.
- Do not change plugin behavior, public config schema, factory key, priority, phase, default, or precedence beyond replacing the metadata source.
- Do not modify generated `pkg/plugin/registry_gen.go` or `pkg/capability/manifest_gen.go` by hand. Run the generator check after adding metadata schema witnesses.
- Impact-scoped tests are the normal gate. Do not run `go test ./...`, `go test ./pkg/...`, `make test`, or the full `t/plugin` suite.
- Each worker owns only the package directories assigned in the parallelization table. Shared `pkg/plugin/base/**` and repository-level guard files remain with the M1 integration owner.
- Workers return owned-path diffs and verification evidence only; they do not
  commit. The integration owner reviews and commits accepted diffs. S1/S2/S3
  overlap worktrees branch only after the corresponding secret-lane checkpoint
  has merged into the integration branch.

---

## Frozen Live Inventory

The inventory was taken at CP3.4 commit `40c04a26` in two passes and revalidated against the move to `e5b6a73e`: that commit changes only `pkg/secret/descriptor.go`, its test, and its plan, so the plugin call/schema inventory is unchanged.

1. `go list -json ./pkg/plugin/...` established compiled production files and their resolved imports, so renamed/default import aliases were not inferred from directory names.
2. An exact production-only call inventory matched `base.LoadPluginMetadata`, `store.GetPluginMetadata`, `store.GetPluginMetadataRaw`, and `store.GetValidatedPluginMetadata`, excluding `_test.go`; registry keys and constructor ownership were then checked against generated `pkg/plugin/registry_gen.go`.

Result: **27 production plugin packages directly consume global metadata**. M1 owns 23; M2 owns 4 direct consumers plus one special owner with no direct getter at this baseline.

### M1 ordinary consumers: 23

| Group | Package | Metadata key | Current call | Metadata schema at baseline | Effective precedence to preserve |
| --- | --- | --- | --- | --- | --- |
| A1 | `azure_functions` | `azure-functions` | `base.LoadPluginMetadata` | missing; add | route authorization, then metadata master authorization |
| A1 | `cors` | `cors` | `base.LoadPluginMetadata` | present | explicit origin/regex rules, then selected metadata keys |
| A1 | `datadog` | `datadog` | `base.LoadPluginMetadata` | present | metadata over built-in defaults; route host/port overrides metadata |
| A1 | `error_page` | `error-page` | `base.LoadPluginMetadata` | missing; add | metadata values, then generated APISIX HTML/content-type defaults |
| A1 | `graphql_limit_count` | **`limit-count`** | `base.LoadPluginMetadata` | owned by `limit_count`; do not add alias schema | exact `limit-count` header metadata only |
| A2 | `file_logger` | `file-logger` | `base.LoadPluginMetadata` | present | route path/format, then metadata, then existing defaults/error |
| A2 | `loki_logger` | `loki-logger` | `base.LoadPluginMetadata` | present | route log format, then metadata format/extra |
| A2 | `skywalking_logger` | `skywalking-logger` | `base.LoadPluginMetadata` | missing; add | route log format, then metadata; route/default queue settings unchanged |
| A2 | `syslog` | `syslog` | `base.LoadPluginMetadata` | present | existing `selectLogFormats` route/metadata/default precedence |
| A2 | `tcp_logger` | `tcp-logger` | `base.LoadPluginMetadata` | present | route log format, then metadata |
| A2 | `udp_logger` | `udp-logger` | `base.LoadPluginMetadata` | present | route log format, then metadata |
| B | `clickhouse_logger` | `clickhouse-logger` | `base.LoadPluginMetadata` | missing; add | route string log format, then metadata; empty remains rejected |
| B | `limit_count` | `limit-count` | `base.LoadPluginMetadata` | missing; add | metadata header names, then existing APISIX header defaults |
| B | `oas_validator` | `oas-validator` | `base.LoadPluginMetadata` | present | metadata `spec_url_ttl`, then existing refresh behavior |
| C | `elasticsearch_logger` | `elasticsearch-logger` | `base.LoadPluginMetadata` | missing; add | route string log format, then metadata; empty remains rejected |
| C | `google_cloud_logging` | `google-cloud-logging` | `base.LoadPluginMetadata` | present | route log format, then metadata |
| C | `http_logger` | `http-logger` | `base.LoadPluginMetadata` | present | route nested log format, then metadata, with depth-five truncation |
| C | `kafka_logger` | `kafka-logger` | `base.LoadPluginMetadata` | missing; add | route log format, then metadata |
| D | `loggly` | `loggly` | `base.LoadPluginMetadata` | missing; add | route field, then metadata field, then built-in default |
| D | `rocketmq_logger` | `rocketmq-logger` | `base.LoadPluginMetadata` | missing; add | route log format, then metadata |
| D | `sls_logger` | `sls-logger` | `base.LoadPluginMetadata` | present | route log format, then metadata |
| D | `splunk_hec_logging` | `splunk-hec-logging` | `base.LoadPluginMetadata` | present | explicit route format, metadata format, then format-extra selection |
| D | `tencent_cloud_cls` | `tencent-cloud-cls` | `base.LoadPluginMetadata` | missing; add | route string log format, then metadata; empty remains rejected |

The ten new schema owners are `azure_functions`, `clickhouse_logger`, `elasticsearch_logger`, `error_page`, `kafka_logger`, `limit_count`, `loggly`, `rocketmq_logger`, `skywalking_logger`, and `tencent_cloud_cls`. `graphql_limit_count` is not an eleventh owner: it consumes the existing `limit-count` document and schema.

### M2 exclusions

| Package | Baseline access | Why excluded from M1 |
| --- | --- | --- |
| `authz_casbin` | `store.GetPluginMetadata` | constructs/reloads an enforcer; M2 must freeze one per generation |
| `batch_requests` | `store.GetValidatedPluginMetadata` | owns a Store last-good cache that C6.5 must replace |
| `chaitin_waf` | `store.GetPluginMetadataRaw` and `store.GetPluginMetadata` | dynamic polling/cache and metadata/config node selection |
| `otel` | `store.GetPluginMetadata` through `safeGetPluginMetadata` | two registry keys and `plugin_metadata > plugin_attr > defaults` precedence |
| `error_log_logger` | no direct getter at `40c04a26` | global observer/task lifetime and explicit metadata schema ownership are M2-only |

`pkg/plugin/base/metadata.go` is shared legacy infrastructure, not a 28th consumer. Delete it only after all 23 M1 packages have migrated and a call-site scan proves no production caller remains.

## Interfaces and Ordering

M1 consumes these already-landed interfaces:

```go
type Dependencies struct {
    Config         *config.EffectiveConfig
    DataEncryption data_encryption.Resolver
    Secrets        secret.GenerationCapability
    Metadata       runtime.MetadataView
    Consumers      ConsumerLookup
    Tasks          *runtime.TaskRegistry
}

func (p *BasePlugin) MetadataView() runtime.MetadataView
func (view runtime.MetadataView) Decode(factory string, target any) (bool, error)
```

The concrete metadata producer is not owned by this lane. Before Task 7 integration, its implementation must satisfy the frozen C6.5 hook:

```go
type MetadataPreparer interface {
    PrepareMetadata(context.Context, PreparationAttempt) (runtime.MetadataView, error)
}
```

It validates/refines raw schemas first, registers the final attempt, materializes only declared `plugin_metadata` fields through the attempt capability, and then calls `runtime.NewMetadataView`. M1 tests inject already-finalized views directly and must not invent a second materializer.

For every leaf, use this exact decode shape inside `PostInit` or its existing preparation helper:

```go
var metadata pluginMetadata
if _, err := p.MetadataView().Decode(metadataFactoryKey, &metadata); err != nil {
    return fmt.Errorf("%s metadata decode failed: %w", name, err)
}
```

Use `Metadata` instead of `pluginMetadata` where that is the package's existing type. `metadataFactoryKey` is `name` everywhere except `graphql_limit_count`, where it is the literal `"limit-count"`. Decode even when later precedence selects route config; this makes one preparation snapshot authoritative and removes conditional access to mutable global state.

## Exclusive Ownership and Parallel Groups

| Worktree | May start from | Exclusive package ownership | Conflict rule | Integration-owner checkpoint |
| --- | --- | --- | --- | --- |
| `m1-a1-core` | `e5b6a73e` | `cors`, `datadog`, `error_page`, `graphql_limit_count` | no secret-lane overlap | `refactor(plugin): bind core metadata consumers` |
| `m1-a1-azure` | integration HEAD **after S3 `azure_functions` and M2-C0 metadata preparer merged** | `azure_functions` | same package is S3-owned before this branch exists; M2-C0 owns generic metadata materialization | `refactor(azure-functions): bind immutable metadata view` |
| `m1-a2-loggers` | `e5b6a73e` | `file_logger`, `loki_logger`, `skywalking_logger`, `syslog`, `tcp_logger`, `udp_logger` | no S1/S2 overlap | `refactor(logger): bind immutable metadata views` |
| `m1-b-s1-overlap` | integration HEAD **after S1 merged** | `clickhouse_logger`, `limit_count`, `oas_validator` | same files are S1-owned before this branch exists | `refactor(plugin): bind s1 metadata consumers` |
| `m1-c-s2-overlap` | integration HEAD **after S2 merged** | `elasticsearch_logger`, `google_cloud_logging`, `http_logger`, `kafka_logger` | same files are S2-owned before this branch exists | `refactor(logger): bind first s2 metadata group` |
| `m1-d-s2-overlap` | integration HEAD **after S2 merged** | `loggly`, `rocketmq_logger`, `sls_logger`, `splunk_hec_logging`, `tencent_cloud_cls` | same files are S2-owned before this branch exists | `refactor(logger): bind second s2 metadata group` |
| `m1-integration` | after all six leaf checkpoints | `pkg/plugin/base/metadata.go`, `pkg/plugin/base/metadata_test.go`, `pkg/plugin/metadata_dependency_guard_test.go` | no leaf worker edits shared files | `refactor(plugin): remove global metadata fallback` |

A1-core and A2 may run in parallel immediately. A1-azure waits for S3's
`azure_functions` checkpoint and M2-C0. B waits for S1. C and D may run in parallel with
each other only after S2 is merged. Do not transplant overlap diffs onto an
older baseline and resolve conflicts mechanically; branch from the merged
secret-lane HEAD so metadata decoding is added to the final scoped-secret
implementation.

---

### Task 1: A1 core metadata consumers

Split this task across `m1-a1-core` and `m1-a1-azure`. The four-package core
worktree may start immediately; the Azure worktree must start after the S3
`azure_functions` secret checkpoint and the generic M2-C0 metadata preparer.
They share no files and the integration owner creates separate accepted
checkpoints.

**Files:**
- Modify: `pkg/plugin/azure_functions/plugin.go`
- Modify: `pkg/plugin/azure_functions/plugin_test.go`
- Modify: `pkg/plugin/cors/plugin.go`
- Modify: `pkg/plugin/cors/plugin_test.go`
- Modify: `pkg/plugin/datadog/plugin.go`
- Modify: `pkg/plugin/datadog/plugin_test.go`
- Modify: `pkg/plugin/error_page/plugin.go`
- Modify: `pkg/plugin/error_page/plugin_test.go`
- Modify: `pkg/plugin/graphql_limit_count/plugin.go`
- Modify: `pkg/plugin/graphql_limit_count/plugin_test.go`

**Interfaces:**
- Consumes: `base.Dependencies.Metadata`, `(*base.BasePlugin).MetadataView`, `runtime.NewMetadataView`, `runtime.MetadataView.Decode`.
- Produces: five generation-local plugin instances with no Store metadata reads; metadata schemas for `azure-functions` and `error-page`.

- [ ] **Step 1: Add red schema-witness tests for the two missing owners**

Add `TestMetadataSchemaAcceptsAzureAuthorization` and `TestMetadataSchemaAcceptsErrorPages`. Valid documents are:

```json
{"master_apikey":"key-a","master_clientid":"client-a"}
```

```json
{"enable":true,"error_404":{"body":"n","content_type":"text/plain"}}
```

Use `util.Validate(valid, p.GetMetadataSchema())` and prove wrong types (`master_apikey: 1`, `enable: "true"`, `error_404.body: 1`) fail. Run:

```bash
source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/azure_functions ./pkg/plugin/error_page -run '^TestMetadataSchema' -count=1
```

Expected: FAIL because both metadata schemas are empty.

- [ ] **Step 2: Add the minimum schema witnesses**

In `azure_functions`, add an object schema whose properties are optional strings `master_apikey` and `master_clientid`; assign it to `p.MetadataSchema` in `Init`. In `error_page`, add an object schema with optional boolean `enable` and optional `error_404`, `error_500`, `error_502`, `error_503` objects, each accepting string `body` and `content_type`; assign it in `Init`. Do not add `required` or `additionalProperties: false`, because empty/additive metadata is currently accepted.

- [ ] **Step 3: Add red generation N/N+1 tests to all five packages**

Each package test builds two independent views using this local helper:

```go
func mustMetadataView(t *testing.T, documents map[string][]byte) runtime.MetadataView {
    t.Helper()
    view, err := runtime.NewMetadataView(documents)
    if err != nil {
        t.Fatalf("NewMetadataView() error = %v", err)
    }
    return view
}
```

Add these exact tests and assertions:

| Test | N document | N+1 document | Assertion after N+1 PostInit |
| --- | --- | --- | --- |
| `azure_functions/TestPreparedGenerationsRetainMetadataAuthorization` | `azure-functions.master_apikey=n-key` | `azure-functions.master_apikey=n1-key` | N still emits `X-Functions-Key: n-key`; N+1 emits `n1-key` |
| `cors/TestPreparedGenerationsRetainMetadataOrigins` | key `tenant` allows `https://n.example` | key `tenant` allows `https://n1.example` | N still accepts only N origin; N+1 accepts only N+1 |
| `datadog/TestPreparedGenerationsRetainMetadataNamespace` | namespace `n` | namespace `n1` | N metric prefix remains `n.` and N+1 is `n1.` |
| `error_page/TestPreparedGenerationsRetainMetadataPages` | enabled 404 body `n` | enabled 404 body `n1` | N body/content-length remain derived from `n` |
| `graphql_limit_count/TestPreparedGenerationsRetainLimitCountMetadataAlias` | `limit-count.limit_header=X-N-Limit` plus conflicting `graphql-limit-count.limit_header=X-Wrong` | `limit-count.limit_header=X-N1-Limit` | N uses `X-N-Limit`; N+1 uses `X-N1-Limit`; neither uses `X-Wrong` |

Run the five exact tests. Expected: FAIL because production still reads the global Store and cannot observe the injected view.

- [ ] **Step 4: Decode once and preserve current precedence**

Replace every `base.LoadPluginMetadata` call with `p.MetadataView().Decode`. Change helper signatures that cannot return errors today (`azure_functions.loadMetadata`, `error_page.loadMetadata`, `datadog.loadMetadata`) to return `(Metadata, error)` or `error`, and propagate failure from `PostInit`. Remove conditional Store loads in `cors` and `error_page`; decode once, then apply the existing condition/default logic to the decoded value. In GraphQL, decode exactly `"limit-count"`.

- [ ] **Step 5: Run focused green and package gates**

```bash
source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/azure_functions ./pkg/plugin/cors ./pkg/plugin/datadog ./pkg/plugin/error_page ./pkg/plugin/graphql_limit_count -count=1
source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/cors ./pkg/plugin/datadog ./pkg/plugin/graphql_limit_count -run 'Metadata|PreparedGenerations' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the A1 checkpoint**

```bash
git add pkg/plugin/azure_functions pkg/plugin/cors pkg/plugin/datadog pkg/plugin/error_page pkg/plugin/graphql_limit_count
git commit -m "refactor(plugin): bind core metadata consumers"
```

---

### Task 2: A2 ordinary logger metadata consumers

**Files:**
- Modify: `pkg/plugin/file_logger/plugin.go`
- Modify: `pkg/plugin/file_logger/plugin_test.go`
- Modify: `pkg/plugin/loki_logger/plugin.go`
- Modify: `pkg/plugin/loki_logger/plugin_test.go`
- Modify: `pkg/plugin/skywalking_logger/plugin.go`
- Modify: `pkg/plugin/skywalking_logger/plugin_test.go`
- Modify: `pkg/plugin/syslog/plugin.go`
- Modify: `pkg/plugin/syslog/plugin_test.go`
- Modify: `pkg/plugin/tcp_logger/plugin.go`
- Modify: `pkg/plugin/tcp_logger/plugin_test.go`
- Modify: `pkg/plugin/udp_logger/plugin.go`
- Modify: `pkg/plugin/udp_logger/plugin_test.go`

**Interfaces:**
- Consumes: finalized `runtime.MetadataView` and existing logger PostInit/Stop lifecycles.
- Produces: six loggers whose path/log format/queue settings are fixed for one generation; a `skywalking-logger` metadata schema witness.

- [ ] **Step 1: Add the missing SkyWalking metadata schema red test**

Add `TestMetadataSchemaAcceptsLogFormatAndPendingLimit`; accept object `log_format` string values and integer `max_pending_entries >= 1`, and reject a string `log_format` and zero `max_pending_entries`. Run the exact test and observe FAIL because `GetMetadataSchema()` is empty.

- [ ] **Step 2: Add the SkyWalking schema and Init assignment**

Use the same field contract already used by `udp_logger`: optional object `log_format` with string values and optional integer `max_pending_entries` with minimum 1. Set `p.MetadataSchema = metadataSchema` in `Init`.

- [ ] **Step 3: Add red N/N+1 tests for all six loggers**

Use local `mustMetadataView` helpers and these exact assertions:

| Package test | N/N+1 metadata | Assertion |
| --- | --- | --- |
| `file_logger/TestPreparedGenerationsRetainMetadataPathAndFormat` | distinct temp paths and `generation: n/n1` formats | N keeps its original path/format after N+1 prepares |
| `loki_logger/TestPreparedGenerationsRetainMetadataFormat` | `log_format.generation=n/n1`, distinct extras | N `LogFormat` and extra map do not change |
| `skywalking_logger/TestPreparedGenerationsRetainMetadataFormat` | string format and pending limit 11/12 | N format and pending limit remain 11 |
| `syslog/TestPreparedGenerationsRetainMetadataFormat` | nested format/extra values n/n1 and pending 11/12 | N snapshot maps and custom-format decision remain N |
| `tcp_logger/TestPreparedGenerationsRetainMetadataFormat` | nested format n/n1 and pending 11/12 | N retains n/11 |
| `udp_logger/TestPreparedGenerationsRetainMetadataFormat` | string format n/n1 and pending 11/12 | N retains n/11 |

For `file_logger`, close both instances and use separate paths so the writer registry does not make the test about shared leases. For async loggers, do not start network delivery; assert prepared fields and close each instance.

- [ ] **Step 4: Replace global loads with one PostInit decode**

Decode before selecting formats or creating batch processors/clients. Preserve `file_logger`'s path error and rich-format precedence, Loki extras, Syslog's `selectLogFormats`, truncation, and the existing max-pending fallback. A decode error must occur before acquiring a writer/client/processor.

- [ ] **Step 5: Run focused package and race gates**

```bash
source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/file_logger ./pkg/plugin/loki_logger ./pkg/plugin/skywalking_logger ./pkg/plugin/syslog ./pkg/plugin/tcp_logger ./pkg/plugin/udp_logger -count=1
source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/file_logger ./pkg/plugin/loki_logger ./pkg/plugin/skywalking_logger ./pkg/plugin/syslog ./pkg/plugin/tcp_logger ./pkg/plugin/udp_logger -run 'Metadata|PreparedGenerations' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the A2 checkpoint**

```bash
git add pkg/plugin/file_logger pkg/plugin/loki_logger pkg/plugin/skywalking_logger pkg/plugin/syslog pkg/plugin/tcp_logger pkg/plugin/udp_logger
git commit -m "refactor(logger): bind immutable metadata views"
```

---

### Task 3: B metadata consumers overlapping S1

**Depends on:** S1 merged into integration. Create this worktree from that merged HEAD, not the earlier `e5b6a73e` leaf baseline.

**Files:**
- Modify: `pkg/plugin/clickhouse_logger/plugin.go`
- Modify: `pkg/plugin/clickhouse_logger/plugin_test.go`
- Modify: `pkg/plugin/limit_count/plugin.go`
- Modify: `pkg/plugin/limit_count/plugin_test.go`
- Modify: `pkg/plugin/oas_validator/plugin.go`
- Modify: `pkg/plugin/oas_validator/plugin_test.go`

**Interfaces:**
- Consumes: S1 scoped secret preparation and the finalized metadata view.
- Produces: metadata decode after scoped secret preparation but before external clients/groups; schema witnesses for `clickhouse-logger` and `limit-count`.

- [ ] **Step 1: Add red schema tests**

For ClickHouse accept optional object `log_format` with string values and optional integer `max_pending_entries >= 1`; reject string log format and zero pending entries. For limit-count accept optional string `limit_header`, `remaining_header`, and `reset_header`; reject non-strings. Run the two exact metadata-schema tests and observe FAIL.

- [ ] **Step 2: Add schema constants and `Init` assignments**

Add only the properties above. Do not add required fields. `graphql-limit-count` depends on this exact `limit-count` schema ownership.

- [ ] **Step 3: Convert Store-backed test setup to immutable views**

In `clickhouse_logger/plugin_test.go`, replace `putPluginMetadata` and its Store/event/global cleanup with a helper returning `runtime.MetadataView`. Update `newRawTestPlugin` to accept a view and include it in the same `base.Dependencies` value as the final S1 dependencies; do not overwrite dependencies later and accidentally discard `Metadata`.

- [ ] **Step 4: Add red N/N+1 tests**

Add:

- `clickhouse_logger/TestPreparedGenerationsRetainMetadataFormat`: N/N+1 formats and pending limits differ; N remains unchanged and empty-format rejection still occurs before client/batch allocation.
- `limit_count/TestPreparedGenerationsRetainMetadataHeaders`: N/N+1 header names differ; overlapping handlers emit their own generation's headers.
- `oas_validator/TestPreparedGenerationsRetainMetadataTTL`: N/N+1 TTLs differ; N refresh decision retains N TTL after N+1 prepares.

Expected: FAIL until each PostInit uses `MetadataView`.

- [ ] **Step 5: Decode after S1 preparation and before side effects**

Replace the three loads with one decode each. Preserve limit-count root/nested config alias provenance from S1; metadata has no role in that normalization. Preserve OAS secret preparation ordering: config secrets are already prepared by S1, then metadata is decoded, then inline/spec URL validation occurs. Do not reintroduce `store.GetPluginMetadata` while `oas_validator` and `limit_count` still legitimately import Store for other responsibilities.

- [ ] **Step 6: Run focused gates**

```bash
source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/clickhouse_logger ./pkg/plugin/limit_count ./pkg/plugin/oas_validator -count=1
source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/limit_count ./pkg/plugin/oas_validator -run 'Metadata|PreparedGenerations' -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit the B checkpoint**

```bash
git add pkg/plugin/clickhouse_logger pkg/plugin/limit_count pkg/plugin/oas_validator
git commit -m "refactor(plugin): bind s1 metadata consumers"
```

---

### Task 4: C metadata consumers overlapping S2

**Depends on:** S2 merged into integration. Create this worktree from that merged HEAD.

**Files:**
- Modify: `pkg/plugin/elasticsearch_logger/plugin.go`
- Modify: `pkg/plugin/elasticsearch_logger/plugin_test.go`
- Modify: `pkg/plugin/google_cloud_logging/plugin.go`
- Modify: `pkg/plugin/google_cloud_logging/plugin_test.go`
- Modify: `pkg/plugin/http_logger/plugin.go`
- Modify: `pkg/plugin/http_logger/plugin_test.go`
- Modify: `pkg/plugin/kafka_logger/plugin.go`
- Modify: `pkg/plugin/kafka_logger/plugin_test.go`

**Interfaces:**
- Consumes: S2 scoped secret access and already materialized metadata.
- Produces: four generation-local logger configurations; new metadata schemas for Elasticsearch and Kafka.

- [ ] **Step 1: Add red schema tests and minimal schemas**

For both missing owners accept optional object `log_format` with string values and optional integer `max_pending_entries >= 1`; reject string format and zero pending entries. Run red, add `metadataSchema`, assign it in `Init`, then rerun green.

- [ ] **Step 2: Remove Store fixtures from Elasticsearch tests**

Replace `putPluginMetadata` with a `runtime.NewMetadataView` helper and pass the view through the final S2 `base.Dependencies`. Delete now-unused Store/event imports. Preserve the tests proving route format wins, metadata fallback is cloned, and empty format fails before side effects.

- [ ] **Step 3: Add red N/N+1 tests**

Add `TestPreparedGenerationsRetainMetadataFormat` in each package. N and N+1 use distinct format values and pending limits; HTTP uses nested `map[string]any`, the other three use string maps. After N+1 PostInit, assert N still owns its original `LogFormat`/private format and pending limit. Also mutate the source document byte slice after `NewMetadataView` and prove neither generation changes.

- [ ] **Step 4: Decode before clients, writers, and batch processors**

Replace all four global loads. Merge `Metadata` into the same dependency bundle that S2 uses for scoped secrets; do not call `SetDependencies` twice. Preserve HTTP depth-five truncation, Elasticsearch empty-format rejection before client allocation, and existing queue defaults.

- [ ] **Step 5: Run package and async race gates**

```bash
source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/elasticsearch_logger ./pkg/plugin/google_cloud_logging ./pkg/plugin/http_logger ./pkg/plugin/kafka_logger -count=1
source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/elasticsearch_logger ./pkg/plugin/google_cloud_logging ./pkg/plugin/http_logger ./pkg/plugin/kafka_logger -run 'Metadata|PreparedGenerations' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the C checkpoint**

```bash
git add pkg/plugin/elasticsearch_logger pkg/plugin/google_cloud_logging pkg/plugin/http_logger pkg/plugin/kafka_logger
git commit -m "refactor(logger): bind first s2 metadata group"
```

---

### Task 5: D metadata consumers overlapping S2

**Depends on:** S2 merged into integration. May run in parallel with Task 4 from the same merged HEAD.

**Files:**
- Modify: `pkg/plugin/loggly/plugin.go`
- Modify: `pkg/plugin/loggly/plugin_test.go`
- Modify: `pkg/plugin/rocketmq_logger/plugin.go`
- Modify: `pkg/plugin/rocketmq_logger/plugin_test.go`
- Modify: `pkg/plugin/sls_logger/plugin.go`
- Modify: `pkg/plugin/sls_logger/plugin_test.go`
- Modify: `pkg/plugin/splunk_hec_logging/plugin.go`
- Modify: `pkg/plugin/splunk_hec_logging/plugin_test.go`
- Modify: `pkg/plugin/tencent_cloud_cls/plugin.go`
- Modify: `pkg/plugin/tencent_cloud_cls/plugin_test.go`

**Interfaces:**
- Consumes: S2 scoped secret preparation and finalized metadata.
- Produces: five generation-local loggers; metadata schemas for Loggly, RocketMQ, and Tencent CLS.

- [ ] **Step 1: Add red schema tests**

Use these exact contracts:

- Loggly: optional string `host`, integer `port`, string `protocol`, integer `timeout`, and object `log_format` with string values. Keep numeric acceptance compatible by not inventing new minimum/maximum constraints.
- RocketMQ and Tencent CLS: optional object `log_format` with string values and optional integer `max_pending_entries >= 1`.

Reject wrong field types. Run the three exact schema tests and observe FAIL, then add schema constants and `Init` assignments.

- [ ] **Step 2: Replace Tencent's Store fixture with an immutable view**

Delete `putPluginMetadata`, Store events, and global Store replacement from `tencent_cloud_cls/plugin_test.go`. Pass a view to `newRawTestPlugin` through the final S2 dependency bundle. Keep route-wins, metadata-fallback cloning, and pre-side-effect empty rejection tests.

- [ ] **Step 3: Add red N/N+1 tests**

Add these tests:

| Package test | N/N+1 assertion |
| --- | --- |
| `loggly/TestPreparedGenerationsRetainMetadataEndpointAndFormat` | N host/port/protocol/timeout/format remain N after N+1 prepares; route fields still win |
| `rocketmq_logger/TestPreparedGenerationsRetainMetadataFormat` | N format/pending remain N |
| `sls_logger/TestPreparedGenerationsRetainMetadataFormat` | N format remains N |
| `splunk_hec_logging/TestPreparedGenerationsRetainMetadataFormat` | N format/extra/pending remain N and explicit route format still suppresses extras |
| `tencent_cloud_cls/TestPreparedGenerationsRetainMetadataFormat` | N format/pending remain N and empty rejection precedes client/batch allocation |

Expected: FAIL before production loads the injected view.

- [ ] **Step 4: Decode once after scoped secrets and before side effects**

Replace all five loads, preserving route-over-metadata-over-default selection exactly. Keep S2's secret capability and metadata view in one `base.Dependencies`; do not create a second dependency assignment that drops either capability.

- [ ] **Step 5: Run package and async race gates**

```bash
source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/loggly ./pkg/plugin/rocketmq_logger ./pkg/plugin/sls_logger ./pkg/plugin/splunk_hec_logging ./pkg/plugin/tencent_cloud_cls -count=1
source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/loggly ./pkg/plugin/rocketmq_logger ./pkg/plugin/sls_logger ./pkg/plugin/splunk_hec_logging ./pkg/plugin/tencent_cloud_cls -run 'Metadata|PreparedGenerations' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the D checkpoint**

```bash
git add pkg/plugin/loggly pkg/plugin/rocketmq_logger pkg/plugin/sls_logger pkg/plugin/splunk_hec_logging pkg/plugin/tencent_cloud_cls
git commit -m "refactor(logger): bind second s2 metadata group"
```

---

### Task 6: Integrate leaf checkpoints and remove the fallback

**Depends on:** Tasks 1–5 reviewed and committed; S1/S2 are already in the integration history.

**Files:**
- Delete: `pkg/plugin/base/metadata.go`
- Delete: `pkg/plugin/base/metadata_test.go`
- Create: `pkg/plugin/metadata_dependency_guard_test.go`

**Interfaces:**
- Consumes: all 23 migrated packages.
- Produces: an import-aware enforcement boundary allowing only named M2 owners and no ordinary global metadata access.

- [ ] **Step 1: Integrate in dependency order**

After S1, S2, the required S3 Azure checkpoint, and M2-C0 are merged, record
`M1_BASE=$(git rev-parse HEAD)` and serialize all M1 integration until the M1
checkpoint is complete. For each returned worktree, the integration owner
reviews its owned-path diff, creates the leaf checkpoint, and integrates only
that checkpoint. After each integration, inspect
`git diff <previous-head>..HEAD -- <owned paths>` and reject changes outside
the group's ownership.

- [ ] **Step 2: Prove the legacy base helper has no production callers**

```bash
rg -n --glob '*.go' --glob '!**/*_test.go' 'LoadPluginMetadata' pkg/plugin
```

Expected: only the declaration in `pkg/plugin/base/metadata.go`. Delete that file and its obsolete nil-Store unit test.

- [ ] **Step 3: Write red AST analyzer fixture tests**

In `metadata_dependency_guard_test.go`, implement tests for a helper that parses production Go source and resolves imports by path. Fixtures must prove detection for:

```go
import base "github.com/wklken/apisix-go/pkg/plugin/base"
func f() { _ = base.LoadPluginMetadata[map[string]any]("x") }
```

```go
import renamed "github.com/wklken/apisix-go/pkg/store"
func f() { _ = renamed.GetPluginMetadata("x", &target) }
```

```go
import . "github.com/wklken/apisix-go/pkg/store"
func f() { _ = GetPluginMetadataRaw("x") }
```

The forbidden selectors are `LoadPluginMetadata`, `GetPluginMetadata`, `GetPluginMetadataRaw`, and `GetValidatedPluginMetadata`. Expected: FAIL before the analyzer helper exists.

- [ ] **Step 4: Implement the import-aware production guard**

Walk `pkg/plugin` production `.go` files, parse imports and call expressions, and reject a forbidden call when its receiver resolves to the exact base/store import path. Dot imports use direct identifier matching. Allow calls only under these exact M2 directories:

```go
var specialMetadataOwners = map[string]struct{}{
    "authz_casbin": {},
    "batch_requests": {},
    "chaitin_waf": {},
    "error_log_logger": {},
    "otel": {},
}
```

The allowlist is directory ownership, not a factory alias mechanism. The test must fail on any new ordinary package, any renamed alias, and any dot import. It should pass if an M2 package removes its legacy call later.

- [ ] **Step 5: Run the guard and source scans**

```bash
source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin -run 'MetadataDependencyGuard' -count=1
rg -n --glob '*.go' --glob '!**/*_test.go' 'LoadPluginMetadata|GetPluginMetadataRaw|GetValidatedPluginMetadata|GetPluginMetadata\(' pkg/plugin
```

Expected: the test passes; text matches are confined to M2 production packages plus guard/test declarations. No M1 package appears.

- [ ] **Step 6: Commit the integration fallback removal**

```bash
git add pkg/plugin/base/metadata.go pkg/plugin/base/metadata_test.go pkg/plugin/metadata_dependency_guard_test.go
git commit -m "refactor(plugin): remove global metadata fallback"
```

---

### Task 7: Integrated acceptance and handoff

**Files:**
- Verify only; no new production files.

**Interfaces:**
- Consumes: all M1 checkpoints plus the C6.5 metadata preparer implementation.
- Produces: evidence that ordinary consumers are immutable, schemas are visible to the compiler, aliases/precedence are intact, and M2 ownership remains untouched.

- [ ] **Step 1: Run all 23 ordinary package tests**

```bash
source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/azure_functions ./pkg/plugin/clickhouse_logger ./pkg/plugin/cors ./pkg/plugin/datadog ./pkg/plugin/elasticsearch_logger ./pkg/plugin/error_page ./pkg/plugin/file_logger ./pkg/plugin/google_cloud_logging ./pkg/plugin/graphql_limit_count ./pkg/plugin/http_logger ./pkg/plugin/kafka_logger ./pkg/plugin/limit_count ./pkg/plugin/loggly ./pkg/plugin/loki_logger ./pkg/plugin/oas_validator ./pkg/plugin/rocketmq_logger ./pkg/plugin/skywalking_logger ./pkg/plugin/sls_logger ./pkg/plugin/splunk_hec_logging ./pkg/plugin/syslog ./pkg/plugin/tcp_logger ./pkg/plugin/tencent_cloud_cls ./pkg/plugin/udp_logger -count=1
```

Expected: PASS.

- [ ] **Step 2: Run the N/N+1 overlap corpus under race**

```bash
source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/azure_functions ./pkg/plugin/clickhouse_logger ./pkg/plugin/cors ./pkg/plugin/datadog ./pkg/plugin/elasticsearch_logger ./pkg/plugin/error_page ./pkg/plugin/file_logger ./pkg/plugin/google_cloud_logging ./pkg/plugin/graphql_limit_count ./pkg/plugin/http_logger ./pkg/plugin/kafka_logger ./pkg/plugin/limit_count ./pkg/plugin/loggly ./pkg/plugin/loki_logger ./pkg/plugin/oas_validator ./pkg/plugin/rocketmq_logger ./pkg/plugin/skywalking_logger ./pkg/plugin/sls_logger ./pkg/plugin/splunk_hec_logging ./pkg/plugin/syslog ./pkg/plugin/tcp_logger ./pkg/plugin/tencent_cloud_cls ./pkg/plugin/udp_logger -run '^TestPreparedGenerationsRetain' -count=1
```

Expected: PASS. Every one of the 23 packages must contribute at least one matching test; verify the run count rather than accepting a package with “no tests to run.”

- [ ] **Step 3: Verify compiler schema visibility and generator parity**

```bash
source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin -run 'SchemaWitness|MetadataDependencyGuard' -count=1
source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/compiler -run 'Schema|PluginMetadata' -count=1
source .envrc && export GOFLAGS=-mod=readonly && go run ./cmd/capability-gen -repo-root . -check
```

Expected: PASS. The ten newly non-empty metadata schemas compile through the same registry witness used by the compiler; the GraphQL alias is still validated by the `limit-count` owner.

- [ ] **Step 4: Run scoped lint, build, and diff checks**

```bash
source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run ./pkg/plugin/azure_functions/... ./pkg/plugin/clickhouse_logger/... ./pkg/plugin/cors/... ./pkg/plugin/datadog/... ./pkg/plugin/elasticsearch_logger/... ./pkg/plugin/error_page/... ./pkg/plugin/file_logger/... ./pkg/plugin/google_cloud_logging/... ./pkg/plugin/graphql_limit_count/... ./pkg/plugin/http_logger/... ./pkg/plugin/kafka_logger/... ./pkg/plugin/limit_count/... ./pkg/plugin/loggly/... ./pkg/plugin/loki_logger/... ./pkg/plugin/oas_validator/... ./pkg/plugin/rocketmq_logger/... ./pkg/plugin/skywalking_logger/... ./pkg/plugin/sls_logger/... ./pkg/plugin/splunk_hec_logging/... ./pkg/plugin/syslog/... ./pkg/plugin/tcp_logger/... ./pkg/plugin/tencent_cloud_cls/... ./pkg/plugin/udp_logger/... ./pkg/plugin
source .envrc && export GOFLAGS=-mod=readonly && make build
git diff --check "$M1_BASE"..HEAD
```

Expected: PASS.

- [ ] **Step 5: Perform ownership and dead-fallback review**

Run:

```bash
git diff --name-only "$M1_BASE"..HEAD
rg -n --glob '*.go' --glob '!**/*_test.go' 'LoadPluginMetadata|GetPluginMetadataRaw|GetValidatedPluginMetadata|GetPluginMetadata\(' pkg/plugin
rg -n '"graphql-limit-count"|"limit-count"|"opentelemetry"|"otel"' pkg/plugin/registry_gen.go pkg/plugin/graphql_limit_count pkg/plugin/otel
```

Confirm:

- only the 23 M1 package directories and the three shared integration paths changed for this lane;
- M1 has no global metadata call;
- M2 files are byte-for-byte unchanged by M1;
- GraphQL reads only `limit-count`;
- no generic alias fallback was added;
- every decode occurs before the package's first external client, writer, processor, group, or goroutine acquisition;
- no handler or log/body-filter callback calls `MetadataView.Decode`.

- [ ] **Step 6: Merge-level review checkpoint**

Request one independent read-only review of `M1_BASE..HEAD` scoped to M1. The
reviewer must check schema compatibility, dependency overwrites after
S1/S2/S3, alias/precedence, error redaction, cleanup on decode failure, and
N/N+1 immutability. Address findings in package-owned follow-up commits; do not
fold M2 remediation into this lane.

## Completion Criteria

Lane M1 is complete only when all of the following are true:

- all 23 frozen ordinary consumers decode from the generation's `runtime.MetadataView` exactly once;
- the ten missing schema witnesses exist and compile; GraphQL retains `limit-count` schema ownership;
- `base.LoadPluginMetadata` and its tests are deleted;
- the import-aware guard prevents new ordinary global metadata access and recognizes default, renamed, and dot imports;
- every ordinary package has a passing N/N+1 overlap test, with the overlap corpus passing under race;
- route/metadata/default precedence tests pass, including GraphQL alias behavior;
- M2's five package directories are unchanged;
- focused package tests, schema/compiler tests, generator check, scoped lint, build, and `git diff --check` pass;
- the independent review has no unresolved M1 finding.
