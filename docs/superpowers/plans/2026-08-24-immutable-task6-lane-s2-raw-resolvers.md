# Immutable Task 6 Lane S2 Raw Resolver Migration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** replace every Lane S2 plugin's package-global `BasePlugin.DataEncryption()` resolution with attempt-scoped `base.ScopedSecretAccess`, while preserving APISIX 3.17 field semantics, keeping raw credentials out of public config and reusable clients, and leaving each async owner safely retireable with its prepared generation.

**Architecture:** the already-merged CP2.1 checkpoint `e5b6a73e` provides the immutable `secret.Descriptor` API, then fifteen independent leaf-package worktrees migrate the raw-resolver plugins from that exact checkpoint. Secret admission remains manifest-owned; leaf code names only its declared fields, derives descriptors with `secret.Value.Descriptor(capability.SecretPluginConfig)`, retains `secret.Value` or constructs an attempt-owned provider inside `Value.Use`, and keeps the transitional legacy materializer separate until the joint Task 9 cutover deletes it after Task 7/8 replacement compilation exists. `clickhouse_logger` is part of the historical sixteen-package raw-resolver inventory but is exclusively implemented by Lane S1 because it also owns Store-backed `$ENV` materialization.

**Tech Stack:** Go 1.26, `pkg/capability` manifest declarations, `pkg/secret.Value` / `secret.Descriptor`, `pkg/plugin/base.ScopedSecretAccess`, generation-scoped plugin lifetime, `logger_batch`, focused unit and race tests.

**Spec:** [`docs/superpowers/plans/2026-08-24-immutable-task6-c6.4-plugin-runtime.md`](2026-08-24-immutable-task6-c6.4-plugin-runtime.md), especially invariants 1-8 and Task 5; the broader behavior contract is [`docs/superpowers/plans/2026-08-23-immutable-compiler-plugin-runtime.md`](2026-08-23-immutable-compiler-plugin-runtime.md).

## Global Constraints

- Architectural prerequisite CP3.4 is `40c04a26` (`feat(compiler): register refined generation attempts`). The actual Lane S2 implementation baseline and descriptor dependency is `e5b6a73e` (`feat(secret): add redacted value descriptors`). Every leaf worktree starts from `e5b6a73e`, never from `master`, bare `40c04a26`, or an older CP3 branch.
- Run every Go command as `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && ...'` from the active worktree root.
- Leaf workers own only their named `pkg/plugin/<package>/**` directory. They do not edit `pkg/plugin/base`, `pkg/plugin/scoped_preparation.go`, `pkg/compiler`, `pkg/capability/manifest.yaml`, shared logger code, route/server code, or another lane's package.
- The manifest is the only editable declaration catalog. Do not introduce a plugin-private declaration table, wildcard expansion table, strictness flag, or alternate field alias.
- Schema validation and structural pre-materialization validation precede secret access. Secret materialization precedes `PostInit`; `PostInit` performs defaults, client/task construction and behavior validation but never calls `DataEncryption()`.
- The scoped method must be exactly `MaterializeScopedSecrets(context.Context, base.ScopedSecretAccess) error`. It must not call `MaterializeSecrets`, `BasePlugin.DataEncryption`, Store, an environment lookup, Vault, or any process-global resolver.
- During the additive window, keep `MaterializeSecrets() error` as a separate legacy implementation for the current Builder. It may share pure installation helpers, but never shares its resolver backend with the scoped method.
- Optional absent/empty fields do not call `ScopedSecretAccess.Materialize`. Present non-strict fields do call it so `$ENV`, `$secret`, encrypted and literal inputs share one admitted path.
- Never copy scoped plaintext back into public `Config`. Replace the raw field with `secret.Descriptor.String()` derived from `Value.Descriptor(capability.SecretPluginConfig)` and retain `secret.Value` privately, or construct the final attempt-owned client/sender inside `Value.Use` and then drop the value.
- A reusable/shared client must be credential-neutral. Authorization headers, Basic Auth, signatures, SASL passwords, tokens and private keys are attached per attempt/request inside `Value.Use`, or the credential-bearing client/sender is plugin-instance-owned and closed in `Stop`.
- Error messages expose factory, canonical field and descriptor/digest only. They never expose raw references, environment names, Vault paths, ciphertext or plaintext.
- Preserve unrelated metadata migration for Lane M1 and async goroutine ownership for Task 11. This lane changes only what is required to move secret resolution and to make the existing secret-bearing client/task lifetime safe.
- No push, PR or merge to `master` is part of this plan. Workers return only
  owned-path diffs and verification evidence. The integration owner inspects
  each worktree and creates the local checkpoint commit after acceptance.
- Every `Commit` command below is therefore an integration-owner step. A leaf
  worker stops after GREEN evidence and the owned-path diff; it never runs that
  command itself.

---

## Dependency and merge graph

```text
CP3.4 architecture @ 40c04a26
  -> CP2.1 descriptor dependency @ e5b6a73e (actual leaf baseline)
      -> S2-N1 ai_rate_limiting ---------+
      -> S2-N2 csrf ---------------------+
      -> S2-N3 kafka_proxy --------------+
      -> S2-N4 response_rewrite ---------+
      -> S2-L1 elasticsearch_logger -----+
      -> S2-L2 google_cloud_logging -----+
      -> S2-L3 http_logger --------------+
      -> S2-L4 kafka_logger -------------+--> S2 integration review/gates
      -> S2-L5 lago ---------------------+
      -> S2-L6 loggly -------------------+
      -> S2-L7 rocketmq_logger ----------+
      -> S2-L8 sls_logger ---------------+
      -> S2-L9 splunk_hec_logging -------+
      -> S2-L10 tencent_cloud_cls -------+
      -> S2-E1 error_log_logger ---------+

S1 clickhouse_logger ----------------------> CP4 leaf merge (not an S2 edit)
S2-E1 merged ------------------------------> M2 error_log_logger metadata/observer work
```

The fifteen leaf branches are independent after `e5b6a73e`. Dispatch them up to the runtime's concurrency limit. Do not begin M2's `error_log_logger` work until S2-E1 is accepted and merged, because both phases necessarily edit `plugin.go` and `plugin_test.go`.

## Sixteen-package current-state inventory

| Package | Manifest-owned fields | Current resolution/use at `40c04a26` | Lane ownership and race class |
|---|---|---|---|
| `ai_rate_limiting` | strict `redis_password`, `sentinel_password` | `PostInit -> resolveSecret -> DataEncryption`; plaintext passed to Redis/Failover clients | S2-N1; focused race because Redis client and quota state are concurrent |
| `clickhouse_logger` | strict `password`; optional `user` | Store `ResolvedSecret` for `$ENV` plus `PostInit` raw password resolver; shared ClickHouse client and batch processor | **Lane S1 exclusive**; S2 only verifies final zero-call inventory |
| `csrf` | strict `key` | `PostInit` resolves into public config; request signing reads it | S2-N2; ordinary unit tests, then focused handler race smoke |
| `kafka_proxy` | strict `sasl.password` | `PostInit` resolves into public config; request context forwards plaintext to protocol owner | S2-N3 owns only `plugin.go`/`plugin_test.go`; Task 11 owns `transport.go` and `websocket.go` goroutines |
| `response_rewrite` | optional `body`; strict `body_secret` | only `body_secret` is currently resolved in `PostInit`; result is copied into public `Body` | S2-N4; ordinary unit tests; preserve body/body_secret/filter validation order |
| `elasticsearch_logger` | strict `auth.password`; optional `headers.Authorization` | only password is resolved; shared ES clients retain Basic Auth/custom headers | S2-L1; async logger race group A |
| `error_log_logger` | plugin-config and plugin-metadata strict `clickhouse.password`, `kafka.brokers.*.sasl_config.password` | one `resolveSecrets` handles both config origins; plugin owns observer, batch processor and Kafka writer | S2-E1 owns plugin-config materialization only; M2 owns metadata materialization, observer/task startup and retirement; isolated race gate |
| `google_cloud_logging` | strict `auth_config.private_key` | `PostInit` writes PEM plaintext into public config, then creates private auth/cache state | S2-L2; async logger race group A |
| `http_logger` | strict `auth_header` | `PostInit` writes plaintext into config and a shared Resty client's default header | S2-L3; async logger race group A |
| `kafka_logger` | strict `brokers.*.sasl_config.password` | `PostInit` mutates every broker and writer retains SASL password | S2-L4; async broker race group B |
| `lago` | strict `token` | `PostInit` stores plaintext in config; every delivery builds Bearer header | S2-L5; async logger race group A |
| `loggly` | strict `customer_token` | `PostInit` stores plaintext in config; URL and RFC5424 structured data reuse it | S2-L6; async transport race group B |
| `rocketmq_logger` | strict `secret_key` | `PostInit` stores plaintext; RocketMQ producer retains it | S2-L7; async broker race group B; Task 11 owns unrelated stop goroutine conversion |
| `sls_logger` | strict `access_key_secret` | `PostInit` validates then clears plaintext; transport does not use it | S2-L8; async transport race group B; retain no secret after validation |
| `splunk_hec_logging` | strict `endpoint.token` | `PostInit` stores plaintext in config and shared Resty default header | S2-L9; async logger race group A |
| `tencent_cloud_cls` | strict `secret_key` | `PostInit` stores plaintext; every send signs with it; `secret_id` is deliberately not declared | S2-L10; async logger race group A |

This table resolves the apparent “16 versus 15” discrepancy: the historical audit has sixteen packages, but the current C6.4 worktree topology moved `clickhouse_logger` to Lane S1. An S2 worker must not edit it.

## Async logger target retention matrix

The batch queue and detached log snapshots never receive a raw reference, ciphertext or plaintext credential. Public config and cache identity retain only `secret.Descriptor.String()` / `Descriptor.Digest()`. Where delivery genuinely needs a credential, the plugin keeps the opaque `secret.Value` in its attempt-owned instance and opens it only inside `Value.Use`; a provider object that must retain credential state is created inside that callback and is closed before the value reference is dropped.

| Logger | Public config / reusable cache | Attempt-owned delivery state |
|---|---|---|
| `elasticsearch_logger` | password/Authorization descriptors; neutral transport identity | opaque values plus plugin-owned authenticated ES client; no authenticated client in the global shared pool |
| `google_cloud_logging` | private-key descriptor; neutral Resty client | private parsed auth/token cache created inside `Value.Use`, cleared on `Stop` |
| `http_logger` | auth-header descriptor; neutral Resty client | opaque value opened only while setting the per-request header |
| `kafka_logger` | broker password descriptors and digest-only identity | plugin-owned writer built from an admitted private broker clone, closed before values are dropped |
| `lago` | token descriptor; neutral Resty client | opaque value opened only while setting the batch request Bearer header |
| `loggly` | token descriptor; neutral HTTP/transport config | opaque value opened only while building one HTTP URL or RFC5424 frame |
| `rocketmq_logger` | secret-key descriptor and digest-only identity | plugin-owned producer created inside `Value.Use`, then stopped before value drop |
| `sls_logger` | access-key-secret descriptor | no credential state after availability validation |
| `splunk_hec_logging` | token descriptor; neutral Resty client | opaque value opened only while setting the per-request HEC header |
| `tencent_cloud_cls` | secret-key descriptor; neutral Resty client | opaque value opened only during per-request signing |
| `error_log_logger` | ClickHouse/Kafka password descriptors | private values plus attempt-owned Kafka writer/request auth; M2 later binds observer/task retirement |
| `clickhouse_logger` | S1-owned user/password descriptors | S1-owned private values/client lifetime; S2 does not implement it |

## Required package-local test harness

Every S2 leaf test file must exercise the public wrapper, not forge `ScopedSecretAccess`. The worker adds a small package-local fake `secret.ScopedAttemptBroker`, loads the real embedded manifest with `capability.Load`, creates its catalog, registers a one-route HTTP publication containing the tested factory, and calls `base.MaterializeScopedPluginSecrets`. The broker records exact `secret.Scope` values and returns test plaintext for ciphertext, `$ENV` and `$secret` inputs. The helper must close the registration with `t.Cleanup`.

The exact call shape is:

```go
p := &Plugin{}
if err := p.Init(); err != nil { t.Fatal(err) }
// Decode config exactly as production does before materialization.
if err := util.Parse(rawConfig, p.Config()); err != nil { t.Fatal(err) }

access := newScopedPluginSecretFixture(t, factory, rawConfig, resolve)
if err := base.MaterializeScopedPluginSecrets(
	context.Background(), access.scope, access.capability, p,
); err != nil { t.Fatal(err) }
if err := p.PostInit(); err != nil { t.Fatal(err) }
```

`newScopedPluginSecretFixture` must use `generation.NewSnapshot`, a valid `PublicationCandidate`, `secret.NewScopedMaterializer`, and `secret.NewGenerationCapability`; it must not expose a constructor for `secret.Value`. This proves declaration lookup, closure authority, exact field paths and redaction through the scoped attempt boundary. It does **not** prove strict at-rest ciphertext policy: `scopedMaterializer` delegates admitted raw values to its broker and does not run the in-process encryption policy.

For each strict field, the test matrix is:

```text
rotated/contextual ciphertext -> scoped broker returns plaintext -> behavior uses plaintext
$ENV://... -> exact scope captured, public config contains descriptor only
$secret://... -> exact scope captured, public config contains descriptor only
empty optional field -> zero broker/materializer calls
resolver failure containing raw input -> wrapper returns redacted credential-unavailable error
```

For optional fields (`response-rewrite.body`, `elasticsearch-logger.headers.Authorization`), add a literal case that succeeds unchanged in behavior but still replaces the public value with a descriptor after admission.

Strict plaintext/ciphertext fail-closed evidence is a serial shared gate owned
by the Task 6 integration owner. It uses the real configured
`data_encryption.Service` with `secret.NewMaterializer`, not the fake scoped
broker, and proves policy failure occurs before any plugin `PostInit` side
effect. Leaf tests only prove exact scoped admission and final behavior.

---

### Task 1: Verify the merged CP2.1 descriptor dependency

**Ownership:** read-only gate for the Lane S2 integration owner. The contract is already implemented and committed as `e5b6a73e`; leaf workers must not modify it.

**Files consumed:**
- `pkg/secret/descriptor.go`
- `pkg/secret/descriptor_test.go`
- `docs/superpowers/plans/2026-08-24-immutable-task6-cp2.1-secret-descriptor.md`

**Interfaces:**
- Scoped path: `func (value secret.Value) Descriptor(capability.SecretDeclarationSource) (secret.Descriptor, error)`.
- Legacy path: `func secret.NewDescriptor(capability.SecretDeclarationSource, [32]byte) (secret.Descriptor, error)` after the leaf computes a digest locally; the constructor never accepts raw text/bytes.
- Descriptor access: `Source()`, `Digest()` and deterministic `String()` in the form `<source>#sha256:<64-lowercase-hex>`.

- [ ] **Step 1: Verify the implementation baseline**

Run:

```bash
git merge-base --is-ancestor e5b6a73e HEAD
git show --no-patch --oneline e5b6a73e
```

Expected: both commands succeed and identify `feat(secret): add redacted value descriptors`.

- [ ] **Step 2: Run the frozen descriptor gates**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/secret -run "^TestDescriptor" -count=1'
```

Expected: PASS. `secret.Descriptor` has no exported state and its errors/format never include raw input.

- [ ] **Step 3: Freeze the leaf usage pattern**

Scoped leaf code derives the descriptor only after successful materialization:

```go
value, err := access.Materialize(ctx, field, raw)
if err != nil { return err }
descriptor, err := value.Descriptor(capability.SecretPluginConfig)
if err != nil { return err }
```

The transitional legacy method hashes its already-resolved private string, then creates the same descriptor without passing raw text to the API:

```go
digest := sha256.Sum256([]byte(resolved))
descriptor, err := secret.NewDescriptor(capability.SecretPluginConfig, digest)
if err != nil { return err }
```

Every leaf branch below starts at exact commit `e5b6a73e`. There is no S2-P0 implementation commit to create.

---

### Task 2: S2-N1 — Migrate `ai_rate_limiting`

**Files:**
- Modify: `pkg/plugin/ai_rate_limiting/plugin.go`
- Modify: `pkg/plugin/ai_rate_limiting/plugin_test.go`
- Modify only if compilation requires lifecycle setup: `pkg/plugin/ai_rate_limiting/benchmark_test.go`

**Interfaces:**
- Consumes: strict fields `redis_password`, `sentinel_password`; `base.ScopedSecretAccess`, `secret.Value` and `secret.Descriptor`.
- Produces: both secret-materializer interfaces; Redis/Failover clients constructed from private admitted values, never public config.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsRedisAndSentinelPasswords` and `TestMaterializeScopedSecretsSkipsEmptyLocalCredentials`. Assert exact canonical scopes, descriptor-only config, no call for policy `local`, and no Redis client construction before `PostInit`.
- [ ] Run red: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/ai_rate_limiting -run "^TestMaterializeScopedSecrets(OwnsRedisAndSentinelPasswords|SkipsEmptyLocalCredentials)$" -count=1'`. Expected: FAIL because the scoped interface is absent.
- [ ] Add private `*secret.Value` fields for Redis and Sentinel passwords plus separate legacy strings. Implement both materializers. In `PostInit`, nest `Value.Use` only around `redis.NewClient`/`redis.NewFailoverClient`; keep rule/quota validation before client construction and clear private values in `Stop` after the client closes.

```go
func (p *Plugin) MaterializeScopedSecrets(ctx context.Context, access base.ScopedSecretAccess) error {
	if p.config.RedisPassword != "" {
		value, err := access.Materialize(ctx, "redis_password", p.config.RedisPassword)
		if err != nil { return err }
		descriptor, err := value.Descriptor(capability.SecretPluginConfig)
		if err != nil { return err }
		p.redisPassword = &value
		p.config.RedisPassword = descriptor.String()
	}
	if p.config.SentinelPassword != "" {
		value, err := access.Materialize(ctx, "sentinel_password", p.config.SentinelPassword)
		if err != nil { return err }
		descriptor, err := value.Descriptor(capability.SecretPluginConfig)
		if err != nil { return err }
		p.sentinelPassword = &value
		p.config.SentinelPassword = descriptor.String()
	}
	return nil
}
```

- [ ] Convert current encrypted-password tests to call the legacy wrapper before `PostInit`, rename the missing-resolver assertion to target `MaterializeSecrets`, and add `$ENV`/`$secret` redaction cases through the scoped wrapper.
- [ ] Run green: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/ai_rate_limiting -run "(MaterializeScopedSecrets|RedisPassword|SentinelPassword|PostInitRequiresRedisEndpoint)" -count=1 && go test -race ./pkg/plugin/ai_rate_limiting -run "(MaterializeScopedSecrets|Concurrent|Redis)" -count=1'`.
- [ ] Commit: `git commit -am "refactor(ai-rate-limiting): scope Redis credentials to attempts"`.

---

### Task 3: S2-N2 — Migrate `csrf`

**Files:**
- Modify: `pkg/plugin/csrf/plugin.go`
- Modify: `pkg/plugin/csrf/plugin_test.go`

**Interfaces:** strict `key`; request signing/verification consumes a private `secret.Value` through `Use`.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsCSRFKey` covering contextual ciphertext, `$ENV`, `$secret`, exact `key` scope and descriptor-only config; add `TestPostInitWithoutMaterializationCannotUseKey` so a direct lifecycle call cannot silently use ciphertext/reference.
- [ ] Run red: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/csrf -run "^(TestMaterializeScopedSecretsOwnsCSRFKey|TestPostInitWithoutMaterializationCannotUseKey)$" -count=1'`.
- [ ] Implement separate scoped/legacy materializers, retain the scoped key privately, and wrap each request's complete verify-or-generate operation in `Value.Use`. Validate trimmed admitted plaintext inside `Value.Use` before installing defaults; do not validate the descriptor string as though it were the key. `PostInit` removes every `DataEncryption()` call.

```go
func (p *Plugin) MaterializeScopedSecrets(ctx context.Context, access base.ScopedSecretAccess) error {
	value, err := access.Materialize(ctx, "key", p.config.Key)
	if err != nil { return err }
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil { return err }
	p.key = &value
	p.config.Key = descriptor.String()
	return nil
}
```

- [ ] Update the rotated/invalid ciphertext tests to target the correct materialization phase. Preserve constant-time comparison and zero-expiry behavior tests unchanged.
- [ ] Run green: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/csrf -count=1 && go test -race ./pkg/plugin/csrf -run "(MaterializeScopedSecrets|Handler|Token)" -count=1'`.
- [ ] Commit: `git commit -am "refactor(csrf): bind signing keys to generation attempts"`.

---

### Task 4: S2-N3 — Migrate `kafka_proxy`

**Files:**
- Modify: `pkg/plugin/kafka_proxy/plugin.go`
- Modify: `pkg/plugin/kafka_proxy/plugin_test.go`
- Do not modify: `consumer.go`, `transport.go`, `websocket.go`, `pubsub.go` or their tests unless a direct compile error proves the secret API changed their signature.

**Interfaces:** strict `sasl.password`; the route/plugin occurrence owns the private value, while the existing request context remains the compatibility handoff to the protocol terminal.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsKafkaProxySASLPassword` for contextual ciphertext, `$ENV`, `$secret`, nil SASL and exact `sasl.password` scope. Assert public config contains only the descriptor and handler behavior still reaches `SASLPassword`.
- [ ] Run red: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/kafka_proxy -run "^TestMaterializeScopedSecretsOwnsKafkaProxySASLPassword$" -count=1'`.
- [ ] Implement scoped/legacy materializers. Keep a private `*secret.Value`; in `prepareRequest`, call `Use` and create the request-local context value inside the callback. Do not change WebSocket/transport goroutine ownership in this lane.

```go
if p.config.SASL != nil {
	value, err := access.Materialize(ctx, "sasl.password", p.config.SASL.Password)
	if err != nil { return err }
	descriptor, err := value.Descriptor(capability.SecretPluginConfig)
	if err != nil { return err }
	p.saslPassword = &value
	p.config.SASL.Password = descriptor.String()
}
```

- [ ] Convert the missing resolver, invalid ciphertext and rotated ciphertext tests to the materialization phase; keep TLS/SASL consumer behavior tests green.
- [ ] Run green: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/kafka_proxy -run "(MaterializeScopedSecrets|SASL|KafkaConsumer)" -count=1 && go test -race ./pkg/plugin/kafka_proxy -run "(MaterializeScopedSecrets|HandlerStoresSASL)" -count=1'`.
- [ ] Commit: `git commit -am "refactor(kafka-proxy): scope SASL passwords to attempts"`.

---

### Task 5: S2-N4 — Migrate `response_rewrite`

**Files:**
- Modify: `pkg/plugin/response_rewrite/plugin.go`
- Modify: `pkg/plugin/response_rewrite/plugin_test.go`
- Modify only if direct setup requires it: `pkg/plugin/response_rewrite/benchmark_test.go`

**Interfaces:** optional `body`, strict `body_secret`; produces one private effective-body value while retaining structural provenance until validation completes.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsResponseBodies` with subcases: literal `body`, `$ENV` body, `$secret` body, strict ciphertext `body_secret`, empty optional body, and both fields configured. Assert `body` uses field `body`, `body_secret` uses field `body_secret`, and both-field conflict rejects before any resolver call.
- [ ] Run red: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/response_rewrite -run "^TestMaterializeScopedSecretsOwnsResponseBodies$" -count=1'`.
- [ ] Extract the existing body/body_secret/filter conflict checks into `ValidatePreMaterialization() error`. Both Builder and compiler call it before either materializer. Materialize the selected field, retain `*secret.Value`, replace only the selected public field with its descriptor, and make the buffered response body assignment occur inside `Value.Use`; do not copy scoped plaintext into `Config.Body`.

```go
func (p *Plugin) ValidatePreMaterialization() error {
	if p.config.Body != nil && p.config.BodySecret != nil {
		return fmt.Errorf("response-rewrite body and body_secret cannot be configured together")
	}
	if p.config.BodySecret != nil && len(p.config.Filters) > 0 {
		return fmt.Errorf("response-rewrite body_secret and filters cannot be configured together")
	}
	if p.config.Body != nil && len(p.config.Filters) > 0 {
		return fmt.Errorf("response-rewrite body and filters cannot be configured together")
	}
	return nil
}
```

- [ ] Preserve base64 validation after materialization by validating the admitted effective body inside `Use`. Preserve filters/vars tests unchanged.
- [ ] Run green: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/response_rewrite -count=1 && go test -race ./pkg/plugin/response_rewrite -run "(MaterializeScopedSecrets|BufferedBody|BodySecret)" -count=1'`.
- [ ] Commit: `git commit -am "refactor(response-rewrite): materialize response bodies by attempt"`.

---

### Task 6: S2-L1 — Migrate `elasticsearch_logger`

**Files:**
- Modify: `pkg/plugin/elasticsearch_logger/plugin.go`
- Modify: `pkg/plugin/elasticsearch_logger/plugin_test.go`

**Interfaces:** strict `auth.password`; optional exact map path `headers.Authorization`. Produces plugin-owned credential values and credential-neutral reusable transports.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsElasticsearchCredentials` covering Basic Auth, exact-case Authorization, absent optional header, lowercase `authorization` remaining an ordinary header, `$ENV` and `$secret`. Assert the two admitted paths are exactly `auth.password` and `headers.Authorization`, and public config contains descriptors.
- [ ] Run red: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/elasticsearch_logger -run "^TestMaterializeScopedSecretsOwnsElasticsearchCredentials$" -count=1'`.
- [ ] Implement both materializers. Store password and optional Authorization as private values. Refactor `clientForEndpoint` so the globally shared component is only credential-neutral transport configuration; attach Basic Auth and Authorization per plugin-owned client/request inside `Value.Use`. Remove plaintext/password/header from `shared.ConfigUID`; use `Value.Digest()` only for plugin-instance-local cache discrimination.
- [ ] Add a regression test proving two generations with different values cannot reuse a credential-bearing ES client, and `Stop` closes/releases each instance once while the batch processor is active.
- [ ] Run green: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/elasticsearch_logger -run "(MaterializeScopedSecrets|EncryptedAuth|Authorization|Client|Stop)" -count=1'`.
- [ ] Run race group A package gate: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/elasticsearch_logger -run "(MaterializeScopedSecrets|Batch|Client|Send|Stop)" -count=1'`.
- [ ] Commit: `git commit -am "refactor(elasticsearch-logger): isolate attempt credentials"`.

---

### Task 7: S2-L2 — Migrate `google_cloud_logging`

**Files:**
- Modify: `pkg/plugin/google_cloud_logging/plugin.go`
- Modify: `pkg/plugin/google_cloud_logging/plugin_test.go`

**Interfaces:** strict `auth_config.private_key`; produces private parsed auth state and a credential-neutral Resty client.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsGooglePrivateKey` covering rotated ciphertext, `$ENV`, `$secret`, `auth_file` without inline key, exact field scope, descriptor-only public config and redacted parse/resolver errors.
- [ ] Run red: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/google_cloud_logging -run "^TestMaterializeScopedSecretsOwnsGooglePrivateKey$" -count=1'`.
- [ ] Implement separate materializers. Parse/copy the inline `AuthConfig` into private `resolvedAuth` inside `Value.Use`; never put the PEM back in `p.config.AuthConfig.PrivateKey`. Keep `auth_file` behavior unchanged and do not treat file contents as a manifest-declared plugin-config field.
- [ ] Ensure token signing and refresh use only private `resolvedAuth`; the shared HTTP client remains free of private key/access-token state. Drop private auth and cached tokens during `Stop` after batch shutdown/client release.
- [ ] Run green/race: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/google_cloud_logging -run "(MaterializeScopedSecrets|PrivateKey|Token|SendBatch|Stop)" -count=1'`.
- [ ] Commit: `git commit -am "refactor(google-cloud-logging): scope service account keys"`.

---

### Task 8: S2-L3 — Migrate `http_logger`

**Files:**
- Modify: `pkg/plugin/http_logger/plugin.go`
- Modify: `pkg/plugin/http_logger/plugin_test.go`

**Interfaces:** strict optional pointer `auth_header`; produces a neutral shared Resty client plus attempt-owned authorization value.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsHTTPAuthorization` covering nil header, ciphertext, `$ENV`, `$secret`, exact `auth_header` scope and descriptor-only config.
- [ ] Run red: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/http_logger -run "^TestMaterializeScopedSecretsOwnsHTTPAuthorization$" -count=1'`.
- [ ] Implement both materializers. Remove the Authorization default header and secret from `ConfigUID`; in `SendBatch`, create the request and set `Authorization` inside `Value.Use`. The shared Resty client owns TLS/timeouts/content type only.
- [ ] Add an overlap test with two plugin instances sharing the neutral client but sending different Authorization headers, then stop one and prove the other remains correct.
- [ ] Run green/race: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/http_logger -run "(MaterializeScopedSecrets|Authorization|Send|Batch|Stop)" -count=1'`.
- [ ] Commit: `git commit -am "refactor(http-logger): bind authorization to attempts"`.

---

### Task 9: S2-L4 — Migrate `kafka_logger`

**Files:**
- Modify: `pkg/plugin/kafka_logger/plugin.go`
- Modify only if the resolved clone requires an explicit parameter: `pkg/plugin/kafka_logger/sender.go`
- Modify: `pkg/plugin/kafka_logger/plugin_test.go`
- Modify: `pkg/plugin/kafka_logger/security_test.go` only if writer construction signatures change

**Interfaces:** strict wildcard `brokers.*.sasl_config.password`; all elements use that one canonical field while retaining distinct broker indexes for diagnostics only.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsEveryKafkaBrokerPassword`: include two SASL brokers plus one broker without SASL; assert exactly two materializations, both scopes use `brokers.*.sasl_config.password`, public broker passwords are descriptors, and mixed identities still fail before writer side effects.
- [ ] Run red: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/kafka_logger -run "^TestMaterializeScopedSecretsOwnsEveryKafkaBrokerPassword$" -count=1'`.
- [ ] Store a private `[]secret.Value` aligned with SASL-bearing broker indexes. Build an immutable private broker clone inside nested `Value.Use` calls and pass that clone to writer construction; never mutate public broker passwords to plaintext. Keep `validateSharedSASLIdentity` on non-secret identity fields.
- [ ] Ensure sender/writer is plugin-owned, `Stop` closes it after batch shutdown, then clears the private clone/value slice. Do not share a SASL-bearing writer across attempts.
- [ ] Run green/race: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/kafka_logger -run "(MaterializeScopedSecrets|SASL|Writer|Batch|Stop)" -count=1'`.
- [ ] Commit: `git commit -am "refactor(kafka-logger): bind broker credentials to attempts"`.

---

### Task 10: S2-L5 — Migrate `lago`

**Files:**
- Modify: `pkg/plugin/lago/plugin.go`
- Modify: `pkg/plugin/lago/plugin_test.go`

**Interfaces:** strict `token`; shared HTTP client remains credential-neutral and each request reads the attempt-owned value.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsLagoToken` for ciphertext, `$ENV`, `$secret`, exact scope, descriptor-only config and redacted errors.
- [ ] Run red: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/lago -run "^TestMaterializeScopedSecretsOwnsLagoToken$" -count=1'`.
- [ ] Implement both materializers and retain a private value. Keep token out of client keys/default headers; set `Authorization: Bearer ...` inside `Value.Use` for each batch request. Clear the value after batch/client retirement.
- [ ] Run green/race: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/lago -run "(MaterializeScopedSecrets|Authorization|SendBatch|Batch|Stop)" -count=1'`.
- [ ] Commit: `git commit -am "refactor(lago): scope delivery tokens to attempts"`.

---

### Task 11: S2-L6 — Migrate `loggly`

**Files:**
- Modify: `pkg/plugin/loggly/plugin.go`
- Modify: `pkg/plugin/loggly/plugin_test.go`

**Interfaces:** strict `customer_token`; both HTTP bulk URL and RFC5424 structured data read the private value without storing plaintext in config.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsLogglyToken` for ciphertext, `$ENV`, `$secret`, HTTP and syslog formatting, descriptor-only config and redacted errors.
- [ ] Run red: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/loggly -run "^TestMaterializeScopedSecretsOwnsLogglyToken$" -count=1'`.
- [ ] Implement both materializers. Refactor `bulkEndpoint` and `structuredData` to accept the plaintext argument and invoke them only inside `Value.Use`; the `http.Client`, TCP/TLS/UDP connection setup and config identity remain credential-neutral.
- [ ] Keep connection-cancellation watcher ownership unchanged here; Task 11 converts its goroutine. `Stop` first retires batch/transport state, then drops the private value.
- [ ] Run green/race: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/loggly -run "(MaterializeScopedSecrets|CustomerToken|SendBatch|Connection|Stop)" -count=1'`.
- [ ] Commit: `git commit -am "refactor(loggly): scope customer tokens to attempts"`.

---

### Task 12: S2-L7 — Migrate `rocketmq_logger`

**Files:**
- Modify: `pkg/plugin/rocketmq_logger/plugin.go`
- Modify: `pkg/plugin/rocketmq_logger/sender.go`
- Modify: `pkg/plugin/rocketmq_logger/plugin_test.go`

**Interfaces:** strict `secret_key`; produces an attempt-owned RocketMQ sender constructed from a private resolved config clone.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsRocketMQSecretKey` for ciphertext, `$ENV`, `$secret`, exact scope, descriptor-only config and unsupported TLS ordering.
- [ ] Run red: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/rocketmq_logger -run "^TestMaterializeScopedSecretsOwnsRocketMQSecretKey$" -count=1'`.
- [ ] Implement both materializers. Change sender construction to accept the resolved secret argument/private config clone and call it inside `Value.Use`; public `Config.SecretKey` remains a descriptor. Do not share a credential-bearing producer across attempts.
- [ ] Keep the current stop-goroutine behavior unchanged in S2 and record it for Task 11. Preserve the existing sender-close ordering and tests.
- [ ] Run green/race: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/rocketmq_logger -run "(MaterializeScopedSecrets|SecretKey|Sender|Batch|Stop)" -count=1'`.
- [ ] Commit: `git commit -am "refactor(rocketmq-logger): scope producer credentials"`.

---

### Task 13: S2-L8 — Migrate `sls_logger`

**Files:**
- Modify: `pkg/plugin/sls_logger/plugin.go`
- Modify: `pkg/plugin/sls_logger/plugin_test.go`

**Interfaces:** strict `access_key_secret`; validation-only value is not retained because the current TLS syslog transport does not authenticate with it.

- [ ] Add failing `TestMaterializeScopedSecretsValidatesAndDropsSLSSecret` for ciphertext, `$ENV`, `$secret`, exact scope, descriptor-only config and proof that `PostInit`/delivery cannot read the plaintext.
- [ ] Run red: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/sls_logger -run "^TestMaterializeScopedSecretsValidatesAndDropsSLSSecret$" -count=1'`.
- [ ] Implement both materializers. Scoped materialization calls `Value.Use` only to prove availability, stores the descriptor in public config, then immediately drops the value. `PostInit` retains the documented transport behavior and contains no resolver call.
- [ ] Keep cancellation watcher/WaitGroup ownership unchanged for Task 11; this lane does not introduce any goroutine.
- [ ] Run green/race: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/sls_logger -run "(MaterializeScopedSecrets|AccessKeySecret|SendBatch|Connection|Stop)" -count=1'`.
- [ ] Commit: `git commit -am "refactor(sls-logger): validate secrets within attempts"`.

---

### Task 14: S2-L9 — Migrate `splunk_hec_logging`

**Files:**
- Modify: `pkg/plugin/splunk_hec_logging/plugin.go`
- Modify: `pkg/plugin/splunk_hec_logging/plugin_test.go`

**Interfaces:** strict `endpoint.token`; shared Resty client is neutral and each batch request receives the attempt-owned header.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsSplunkToken` for ciphertext, `$ENV`, `$secret`, exact scope, descriptor-only config and channel header preservation.
- [ ] Run red: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/splunk_hec_logging -run "^TestMaterializeScopedSecretsOwnsSplunkToken$" -count=1'`.
- [ ] Implement both materializers. Remove token from `ConfigUID` and client default headers; add `Authorization: Splunk ...` to the batch request inside `Value.Use`. Keep URI/channel/timeout/TLS on the neutral shared client.
- [ ] Add an N/N+1 overlap test proving a shared neutral client cannot cross-contaminate Authorization headers.
- [ ] Run green/race: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/splunk_hec_logging -run "(MaterializeScopedSecrets|Authorization|SendBatch|Batch|Stop)" -count=1'`.
- [ ] Commit: `git commit -am "refactor(splunk-hec-logging): scope HEC tokens to attempts"`.

---

### Task 15: S2-L10 — Migrate `tencent_cloud_cls`

**Files:**
- Modify: `pkg/plugin/tencent_cloud_cls/plugin.go`
- Modify: `pkg/plugin/tencent_cloud_cls/plugin_test.go`
- Do not modify: `pkg/plugin/tencent_cloud_cls/manifest_test.go`

**Interfaces:** strict `secret_key` only. `secret_id` remains ordinary config because the current manifest deliberately does not declare it.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsTencentCLSSecretKey` for ciphertext, `$ENV`, `$secret`, exact scope, descriptor-only config and authorization signature behavior. Add an assertion that `secret_id` causes zero secret calls.
- [ ] Run red: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/tencent_cloud_cls -run "^TestMaterializeScopedSecretsOwnsTencentCLSSecretKey$" -count=1'`.
- [ ] Implement both materializers. Change `authorization` to accept/use the private secret inside `Value.Use`; never copy it to public config or the neutral Resty client. Preserve one-timestamp signing and `SecretID` behavior.
- [ ] Run green/race: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race ./pkg/plugin/tencent_cloud_cls -run "(MaterializeScopedSecrets|Authorization|SecretKey|SendBatch|Batch|Stop)" -count=1'`.
- [ ] Commit: `git commit -am "refactor(tencent-cloud-cls): scope signing keys to attempts"`.

---

### Task 16: S2-E1 — Migrate only `error_log_logger` plugin-config secrets

**Files:**
- Modify: `pkg/plugin/error_log_logger/plugin.go`
- Modify: `pkg/plugin/error_log_logger/plugin_test.go`

**Exclusive ownership split with M2:**
- S2-E1 owns `Config.Clickhouse.Password`, `Config.Kafka.Brokers[*].SASLConfig.Password`, scoped/legacy plugin-config materializers, private plugin-config values, writer/HTTP request consumption, and secret-focused tests.
- M2 owns plugin-metadata schema/decoding, `plugin_metadata` materialization, metadata-versus-route selection, global `StartObserving`, `observerStop`, task registry binding, `Stop` retirement semantics, N/N+1 metadata overlap and invalid-desired/last-good tests.
- S2-E1 must not add metadata reads, call `PreparationAttempt.MaterializeSecret` for metadata, start the global observer, change observer replacement semantics, or claim task ownership complete.
- M2 branches only after S2-E1 is merged and preserves S2's private secret-installation helpers.

**Interfaces:** strict plugin-config `clickhouse.password` and wildcard `kafka.brokers.*.sasl_config.password`; both metadata declarations remain untouched for M2.

- [ ] Add failing `TestMaterializeScopedSecretsOwnsErrorLoggerPluginConfig` with ClickHouse plus two SASL brokers. Assert exact plugin-config source, wildcard canonical field, descriptor-only config, no observer startup and no metadata access.
- [ ] Run red: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/error_log_logger -run "^TestMaterializeScopedSecretsOwnsErrorLoggerPluginConfig$" -count=1'`.
- [ ] Split current `resolveSecrets` into pure field enumeration/installation plus two backends. Scoped code uses only access; legacy code uses only `DataEncryption`. Construct the Kafka writer from a private resolved broker clone and make ClickHouse delivery accept the private password rather than reading public config.

```go
func (p *Plugin) MaterializeScopedSecrets(ctx context.Context, access base.ScopedSecretAccess) error {
	if p.config.Clickhouse != nil {
		value, err := access.Materialize(ctx, "clickhouse.password", p.config.Clickhouse.Password)
		if err != nil { return err }
		descriptor, err := value.Descriptor(capability.SecretPluginConfig)
		if err != nil { return err }
		p.clickhousePassword = &value
		p.config.Clickhouse.Password = descriptor.String()
	}
	for i := range p.config.Kafka.Brokers {
		sasl := p.config.Kafka.Brokers[i].SASLConfig
		if sasl == nil { continue }
		value, err := access.Materialize(ctx, "kafka.brokers.*.sasl_config.password", sasl.Password)
		if err != nil { return err }
		descriptor, err := value.Descriptor(capability.SecretPluginConfig)
		if err != nil { return err }
		p.kafkaPasswords = append(p.kafkaPasswords, indexedSecret{index: i, value: value})
		sasl.Password = descriptor.String()
	}
	return nil
}
```

- [ ] Update the current encrypted tests to target materialization, preserve Kafka mechanism and sender tests, and add a guard proving metadata declarations are not accessed by this method.
- [ ] Run green: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test ./pkg/plugin/error_log_logger -run "(MaterializeScopedSecrets|ClickHousePassword|KafkaPassword|SASL|SendLogs)" -count=1'`.
- [ ] Run the package alone under race because it replaces a process-global logger observer: `bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race -p=1 ./pkg/plugin/error_log_logger -run "(MaterializeScopedSecrets|Observer|Batch|Kafka|Stop)" -count=1'`.
- [ ] Commit: `git commit -am "refactor(error-log-logger): scope plugin credentials to attempts"`.

---

### Task 17: Accept leaf worktrees and run Lane S2 integration

**Ownership:** Task 6 integration owner. Leaf workers do not perform this task.

**Files:**
- Review only the fifteen S2 package worktrees, then create and integrate one
  accepted checkpoint per owned package onto `codex/apisix-go-immutable-task6`.
- Reject any `clickhouse_logger` edit from S2; accept that package only from
  Lane S1.

- [ ] **Step 1: Review every leaf diff before the integration owner commits it**

For each commit, verify its path set is restricted to its declared package, its scoped method contains no `MaterializeSecrets` or `DataEncryption` call, secret fields match the manifest spelling exactly, and no unrelated formatting/refactor entered the diff.

- [ ] **Step 2: Prove all manifest-declared S2 factories advertise scoped support**

Add or extend the existing plugin support test with this exact set:

```go
for _, factory := range []string{
	"ai-rate-limiting", "csrf", "kafka-proxy", "response-rewrite",
	"elasticsearch-logger", "error-log-logger", "google-cloud-logging",
	"http-logger", "kafka-logger", "lago", "loggly", "rocketmq-logger",
	"sls-logger", "splunk-hec-logging", "tencent-cloud-cls",
} {
	supported, err := plugin.SupportsScopedSecretMaterialization(factory)
	if err != nil || !supported { t.Errorf("factory %s scoped support = %v/%v", factory, supported, err) }
}
```

This common test is owned by the integration owner because leaf workers may not edit `pkg/plugin/scoped_preparation_test.go`.

- [ ] **Step 3: Run non-logger focused tests**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test \
  ./pkg/plugin/ai_rate_limiting ./pkg/plugin/csrf ./pkg/plugin/kafka_proxy ./pkg/plugin/response_rewrite \
  -run "(MaterializeScopedSecrets|Secret|Encrypted|SASL|Body|Redis|Token)" -count=1'
```

- [ ] **Step 4: Run async logger race group A**

HTTP/client/signature packages can run together because each package is a separate test process:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race \
  ./pkg/plugin/elasticsearch_logger ./pkg/plugin/google_cloud_logging ./pkg/plugin/http_logger \
  ./pkg/plugin/lago ./pkg/plugin/splunk_hec_logging ./pkg/plugin/tencent_cloud_cls \
  -run "(MaterializeScopedSecrets|Secret|Authorization|PrivateKey|Send|Batch|Stop|Client)" -count=1'
```

- [ ] **Step 5: Run async logger race group B**

Broker/transport packages get a separate command so slow close/cancellation failures are attributable:

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race \
  ./pkg/plugin/kafka_logger ./pkg/plugin/loggly ./pkg/plugin/rocketmq_logger ./pkg/plugin/sls_logger \
  -run "(MaterializeScopedSecrets|Secret|SASL|Sender|Connection|Send|Batch|Stop)" -count=1'
```

- [ ] **Step 6: Run `error_log_logger` in isolation**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test -race -p=1 \
  ./pkg/plugin/error_log_logger \
  -run "(MaterializeScopedSecrets|Secret|SASL|Observer|Send|Batch|Stop)" -count=1'
```

Do not include `clickhouse_logger` here; Lane S1 supplies its focused/race evidence. At CP4, run it alongside the complete sixteen-package inventory.

- [ ] **Step 7: Run full affected package tests once**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && go test \
  ./pkg/plugin/ai_rate_limiting ./pkg/plugin/csrf ./pkg/plugin/kafka_proxy ./pkg/plugin/response_rewrite \
  ./pkg/plugin/elasticsearch_logger ./pkg/plugin/error_log_logger ./pkg/plugin/google_cloud_logging \
  ./pkg/plugin/http_logger ./pkg/plugin/kafka_logger ./pkg/plugin/lago ./pkg/plugin/loggly \
  ./pkg/plugin/rocketmq_logger ./pkg/plugin/sls_logger ./pkg/plugin/splunk_hec_logging \
  ./pkg/plugin/tencent_cloud_cls -count=1'
```

- [ ] **Step 8: Run boundary and source-of-truth gates**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && \
  go test ./pkg/plugin -run "(ScopedSecret|SecretMaterializationGuard|FactoryInstance)" -count=1 && \
  go run ./cmd/capability-gen -repo-root . -check && \
  git diff --check'
```

Expected: every S2 `MaterializeScopedSecrets` method is mechanically proven not
to call Store, `DataEncryption`, a legacy materializer, or a raw resolver; all
factories with manifest plugin-config declarations report scoped support; and
generated registry/manifest artifacts are unchanged. The current Builder still
calls `MaterializePluginSecrets`, so each package's separate
`MaterializeSecrets` compatibility method and its file-level
`pkg/data_encryption` import remain allowed until the C6.6 live-caller ledger
assigns their deletion to the joint Task 9 cutover. A package-wide zero-match
scan here would contradict the leaf tasks' required legacy backend and the
pre-Task-9 production-owner boundary.

- [ ] **Step 9: Run lint and build smoke**

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && golangci-lint run \
  ./pkg/plugin/ai_rate_limiting/... ./pkg/plugin/csrf/... ./pkg/plugin/kafka_proxy/... \
  ./pkg/plugin/response_rewrite/... ./pkg/plugin/elasticsearch_logger/... \
  ./pkg/plugin/error_log_logger/... ./pkg/plugin/google_cloud_logging/... \
  ./pkg/plugin/http_logger/... ./pkg/plugin/kafka_logger/... ./pkg/plugin/lago/... \
  ./pkg/plugin/loggly/... ./pkg/plugin/rocketmq_logger/... ./pkg/plugin/sls_logger/... \
  ./pkg/plugin/splunk_hec_logging/... ./pkg/plugin/tencent_cloud_cls/...'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && make build'
```

- [ ] **Step 10: Commit integration-only support test, if changed**

```bash
git add pkg/plugin/scoped_preparation_test.go
git commit -m "test(plugin): require scoped support for raw resolver factories"
```

**Lane acceptance checkpoint:** record the final integration SHA, the fifteen
integration-owner leaf checkpoint SHAs, S1's `clickhouse_logger` SHA, all
command outputs, and the explicit handoff that M2 now owns the remaining
`error_log_logger` metadata/observer lifecycle work.

## Self-review checklist

- [ ] All fifteen S2 implementation packages and the S1-owned sixteenth audit package are accounted for.
- [ ] Every manifest declaration appears exactly once in a package task, including `response-rewrite.body` and `elasticsearch-logger.headers.Authorization`, which the old raw-resolver code did not handle.
- [ ] `tencent-cloud-cls.secret_id` is not silently added; the manifest remains authoritative.
- [ ] No scoped implementation calls the legacy backend and no `PostInit` resolves secrets.
- [ ] Secret-bearing shared clients were made neutral or replaced by attempt-owned instances; client cache keys contain digest/identity only.
- [ ] `error_log_logger` plugin-config ownership is complete while metadata/observer/task ownership remains explicitly assigned to M2.
- [ ] `clickhouse_logger` is not modified by S2 and remains assigned to S1.
- [ ] Async race commands are split into client/signature, broker/transport, and isolated global-observer groups.
- [ ] Each leaf task has a red test, a green test, an exact ownership boundary
  and an integration-owner checkpoint.
- [ ] No placeholder text or unspecified error-handling step remains.
