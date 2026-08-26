# Immutable Task 6 Lane A1 Consumer Bindings Implementation Plan

> **Execution rule:** `40c04a26` is the architecture prerequisite,
> `e5b6a73e` provides the shared descriptor contract, and `9ebcd2b5` is the
> accepted A1.1 compiler checkpoint. Auth leaf worktrees start only from an
> integration descendant that also contains the accepted S3-0 compatibility
> adapter; they never branch directly from one of those older commits or from
> `master`. Workers return diffs and verification evidence only. The integration
> owner reviews and commits accepted diffs. Push and PR publication remain
> outside current authority.

**Goal:** construct immutable, attempt-local consumer bindings from the exact
final HTTP publication after registration, materialize only manifest-declared
consumer credential strings, and migrate the seven auth leaf packages away
from request-time global Store lookup on the new compiler path.

**Architecture:** the compiler owns consumer decoding, scoped materialization,
resolved validation, deterministic lookup indexing, and binding lifetime.
Plugins receive only `base.ConsumerLookup`. During the additive window, current
Builder compatibility is an explicit legacy branch; a non-nil lookup never
falls through to Store. C6.6 records that nil-lookup branch as a live legacy
caller; Task 7 compiles immutable consumer bindings and the joint Task 9
cutover deletes the branch after replacing the sole production path.

**Architecture baseline:** `40c04a26` (`feat(compiler): register refined
generation attempts`). **Completed A1.1 checkpoint:** `9ebcd2b5`
(`feat(compiler): prepare immutable consumer bindings`).

**Dependencies:** CP1 catalog declarations, CP2 `runtime.ConsumerBindings` and
`base.ConsumerLookup`, CP3.4 final-attempt authority. The shared CP2.1
`secret.Descriptor` correction required by the secret lanes is present at
`e5b6a73e`. A1 auth package work additionally waits for S3-0 because six auth
factories have compatibility-only `plugin_config` declarations whose raw
values are not represented by the route plugin Config structs. S3-0 owns that
raw-before-decode boundary; A1 continues to own the auth package directories.

## Frozen ownership and compatibility matrix

| Factory | Exact lookup field | Request-time credential use | Anonymous lookup | Leaf files |
| --- | --- | --- | --- | --- |
| `basic-auth` | `username` | constant-time `password` comparison | yes | `pkg/plugin/basic_auth/**` |
| `key-auth` | `key` | index only | yes | `pkg/plugin/key_auth/**` |
| `jwt-auth` | `key` | signing keys, algorithm and grace period | yes | `pkg/plugin/jwt_auth/**` |
| `hmac-auth` | `key_id` | `secret_key` signature validation | yes | `pkg/plugin/hmac_auth/**` |
| `ldap-auth` | `user_dn` | index after LDAP bind | no | `pkg/plugin/ldap_auth/**` |
| `jwe-decrypt` | string `key` | string `secret` and base64 flag | no | `pkg/plugin/jwe_decrypt/**` |
| `wolf-rbac` | `appid` | server/header-prefix/TLS consumer override | no | `pkg/plugin/wolf_rbac/**` |

`wolf-rbac` retains both declared `server` and historical `wolf_url`. This lane
does not choose one spelling or remove either declaration. The plugin continues
to consume `server`; the compatibility divergence remains documented.

## Dependency order

```text
40c04a26 exact registered attempt
  -> e5b6a73e descriptor contract
      -> 9ebcd2b5 A1.1 compiler consumer preparer
      -> S3-0 compatibility-only plugin_config adapter
          -> A1.2 auth leaf groups (parallel, exclusive packages)
          -> A1.3 merged guards and overlap verification
              -> X1 workflow/multi-auth composites
              -> CP5 PreparedGeneration ownership
```

## A1.1 — Build exact attempt-local consumer bindings

**Single owner files:**

- Create: `pkg/compiler/consumer_preparer.go`
- Create: `pkg/compiler/consumer_preparer_test.go`
- Modify only if a focused seam is required:
  `pkg/compiler/hooks.go`, `pkg/compiler/factory_test.go`
- Do not modify auth plugin packages in this checkpoint.

### Step 1: Write failing final-attempt construction tests

Add focused tests proving:

1. only the final HTTP candidate is consumed; a stream-only attempt returns an
   empty binding;
2. every published `consumers/<id>` and `consumer_groups/<id>` record is copied
   from the candidate snapshot, while tombstones create no record;
3. each supported consumer config uses its owned
   `FactoryOccurrence{Source: consumer_config}` and exact consumer resource;
4. declared string leaves are materialized through
   `PreparationAttempt.MaterializeSecret`; missing optional leaves make no call;
5. JWE `key`/`secret` are materialized only when their dynamic value is a
   string, then `consumer.ValidateResolved` preserves the existing rejection of
   unsupported non-string values;
6. resolved configs are validated again after materialization before lookup
   key extraction;
7. duplicate resolved `(factory, lookup-key)` values fail deterministically and
   without raw key/reference/plaintext text;
8. input snapshots and occurrence slices remain unchanged;
9. generation N and N+1 produce independent bindings, closing N does not alter
   N+1, and concurrent reads/close pass the race detector.

Run RED:

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/compiler -run '^TestConsumerBindingPreparer' -count=1
```

Expected: compilation fails because the concrete preparer does not exist.

### Step 2: Implement deterministic decoding and scoped materialization

Implement an unexported preparer owned by `pkg/compiler`:

```go
type consumerBindingPreparer struct {
    catalog *capability.SecretDeclarationCatalog
}

func newConsumerBindingPreparer(
    catalog *capability.SecretDeclarationCatalog,
) (*consumerBindingPreparer, error)

func (p *consumerBindingPreparer) PrepareConsumers(
    context.Context,
    PreparationAttempt,
    runtime.MetadataView,
) (*runtime.ConsumerBindings, error)
```

The constructor requires the exact compiler catalog. It must not accept Store,
`data_encryption.Service`, a resolver callback, or a raw keyring.

For the HTTP candidate:

1. validate/copy the candidate exposed by `PreparationAttempt.Candidate`;
2. normalize its snapshot and iterate canonical published resources only;
3. map attempt-owned `consumer_config` occurrences by
   `(ResourceKey, Factory)` and reject missing/extra/duplicate authority;
4. decode each consumer into an independent generic document;
5. for each supported consumer factory, call
   `catalog.TransformDeclaredFields` on that config;
6. for a string leaf, call `attempt.MaterializeSecret(ctx, occurrence,
   declaration.Field, raw)` and install the returned plaintext only inside
   `secret.Value.Use`;
7. skip non-string leaves rather than coercing them; resolved validation owns
   the final type decision;
8. call `consumer.ValidateResolved`, then `consumer.LookupKey` on the resolved
   config;
9. decode the complete resolved document into `resource.Consumer`, set
   `ConfigDigest = sha256(rawPublishedBytes)`, and append one
   `runtime.ConsumerCredentialBinding` per supported configured factory;
10. decode consumer groups without consumer-credential materialization and set
    their raw publication digest;
11. construct `runtime.NewConsumerBindings` only after all records validate.

No error may contain a raw document, secret reference, environment name,
credential, lookup key, or plaintext. Resource/factory identity may be
represented only by the repository's redacted descriptor/digest contract.

### Step 3: Prove lifecycle and recovery behavior

Add candidate and recovery tests using different HTTP/Stream revisions. Recovery
must consume only the exact verified committed HTTP candidate. It must not read
desired state or recalculate disposition. Add a poison registration proving a
foreign occurrence is rejected before materialization.

Run GREEN:

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/compiler -run '^TestConsumerBindingPreparer' -count=1
go test -race ./pkg/compiler -run '^TestConsumerBindingPreparer' -count=1
golangci-lint run ./pkg/compiler/...
git diff --check
```

**Completed checkpoint:** `9ebcd2b5 feat(compiler): prepare immutable consumer bindings`

## A1.2 — Migrate auth leaf packages

Every worker owns only its listed package directories and tests. Workers do not
modify `pkg/compiler`, `pkg/runtime`, `pkg/plugin/base`, `pkg/plugin/init.go`,
route/server files, Store, shared test helpers, or another leaf package.
Workers do not commit; the integration owner commits only after reviewing the
returned diff and fresh verification evidence. No A1 auth package worker starts
until S3-0 has merged, and S3 must not assign those same six auth packages to a
concurrent leaf worker.

Recommended parallel groups after A1.1 merges:

- Group A: `basic_auth`, `key_auth`
- Group B: `jwt_auth`, `hmac_auth`
- Group C: `ldap_auth`, `jwe_decrypt`, `wolf_rbac`

For every plugin:

1. add tests with an injected immutable `base.ConsumerLookup` before changing
   production code;
2. use `ConsumerByPluginKey(factory, resolvedKey)` and `ConsumerByID` for
   anonymous consumers;
3. treat a non-nil lookup as authoritative: misses fail/anonymous-fallback
   without consulting Store;
4. preserve a separately named legacy Store helper only when lookup is nil so
   the current Builder remains behavior-compatible until the joint Task 9 cutover;
5. never pass Store or a closeable `*runtime.ConsumerBindings` into the plugin;
6. preserve authentication-state publication, credential hiding, diagnostics,
   status/body/header behavior, and constant-time comparisons;
7. add a poison test proving a non-nil lookup miss cannot reach the legacy
   Store branch;
8. keep consumer configs instance/read-only during requests.

### Group A details

`basic_auth` retains username lookup, `basicAuth` parsing, constant-time
password comparison, realm behavior, and anonymous fallback. `key_auth`
retains header/query precedence, credential hiding, anonymous behavior, and its
distinction between invalid credentials and current legacy lookup failure. The
immutable lookup has no runtime backend-error state because preparation already
validated the index.

Run:

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/plugin/basic_auth ./pkg/plugin/key_auth -count=1
go test -race ./pkg/plugin/basic_auth ./pkg/plugin/key_auth -count=1
```

**Integration-owner checkpoint:** `refactor(auth): use immutable basic and key consumers`

### Group B details

`jwt_auth` must continue parsing/verifying against the resolved consumer copy,
including asymmetric/private/public key behavior, `base64_secret`, algorithm,
grace period, claims, and anonymous consumers. `hmac_auth` retains canonical
signature construction, replay/date/digest behavior, secret-key use and
anonymous fallback. Neither plugin may cache across attempt instances.

Run:

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/plugin/jwt_auth ./pkg/plugin/hmac_auth -count=1
go test -race ./pkg/plugin/jwt_auth ./pkg/plugin/hmac_auth -count=1
```

**Integration-owner checkpoint:** `refactor(auth): use immutable jwt and hmac consumers`

### Group C details

`ldap_auth` retains LDAP bind before lookup and uses the resolved `user_dn`
index. `jwe_decrypt` retains string-only key/secret behavior, base64 handling,
authentication-state publication and exact error status. `wolf_rbac` retains
consumer route fallback precedence, public API ownership, per-consumer TLS
selection and the `server`/`wolf_url` compatibility record.

Run:

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/plugin/ldap_auth ./pkg/plugin/jwe_decrypt ./pkg/plugin/wolf_rbac -count=1
go test -race ./pkg/plugin/ldap_auth ./pkg/plugin/jwe_decrypt ./pkg/plugin/wolf_rbac -count=1
```

**Integration-owner checkpoint:** `refactor(auth): use immutable ldap jwe and wolf consumers`

## A1.3 — Merge and verify the consumer lane

After reviewing each worktree, the integration owner creates one leaf commit
and integrates those commits in the fixed group order A, B, C. After each
integration, inspect the exact owned-path diff and run that group's focused
tests. Resolve shared behavior conflicts on the integration branch; do not let
a worker modify another group's package.

Add or extend integration guards proving:

- the new compiler consumer preparer imports neither Store nor
  `pkg/data_encryption`;
- every supported factory from `pkg/consumer.Factories()` creates the exact
  resolved lookup identity;
- a non-nil `base.ConsumerLookup` is authoritative in all seven auth packages;
- remaining Store calls are only the named transitional nil-lookup branches;
- N/N+1 overlapping requests use their own immutable consumers;
- closing N after handler retirement does not invalidate N+1;
- no auth plugin can type-assert the lookup back to
  `*runtime.ConsumerBindings` or call `Close`.

Run:

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/consumer ./pkg/runtime ./pkg/compiler \
  ./pkg/plugin/basic_auth ./pkg/plugin/key_auth ./pkg/plugin/jwt_auth \
  ./pkg/plugin/hmac_auth ./pkg/plugin/ldap_auth ./pkg/plugin/jwe_decrypt \
  ./pkg/plugin/wolf_rbac -count=1
go test -race ./pkg/runtime ./pkg/compiler \
  ./pkg/plugin/basic_auth ./pkg/plugin/key_auth ./pkg/plugin/jwt_auth \
  ./pkg/plugin/hmac_auth ./pkg/plugin/ldap_auth ./pkg/plugin/jwe_decrypt \
  ./pkg/plugin/wolf_rbac -count=1
golangci-lint run ./pkg/consumer/... ./pkg/runtime/... ./pkg/compiler/... \
  ./pkg/plugin/basic_auth/... ./pkg/plugin/key_auth/... ./pkg/plugin/jwt_auth/... \
  ./pkg/plugin/hmac_auth/... ./pkg/plugin/ldap_auth/... \
  ./pkg/plugin/jwe_decrypt/... ./pkg/plugin/wolf_rbac/...
make build
git diff --check
```

## Task 7/9 deletion handoff

C6.6 records the seven named legacy Store helpers and their old-Builder callers;
it does not inject a prepared lookup into production. Task 7 compiles every
effective auth instance with the exact `base.ConsumerLookup`, and the joint
Task 9 cutover deletes the helpers/imports only after import-aware scans show
zero production auth lookup calls to Store. Task 9 also owns rollback and
retirement evidence for the installed generation.

## Acceptance ledger

| Boundary | Required evidence |
| --- | --- |
| Exact input | only the registered final HTTP/recovery candidate is decoded |
| Declaration | only manifest `consumer_config` fields materialize |
| Validation | raw schema before registration, resolved schema before indexing |
| Identity | deterministic resolved `(factory,key)` with duplicate rejection |
| Isolation | N/N+1 bindings and close lifetimes do not overlap incorrectly |
| Plugins | seven auth packages use immutable lookup whenever it is present |
| Compatibility | only explicit nil-lookup legacy branches remain until Task 9 replaces the production Builder |
| Redaction | no raw references, lookup keys, credentials or plaintext in errors |
| Gates | focused tests, race, lint, build and diff checks pass |

## Explicit non-goals

- No workflow or multi-auth composite migration; X1 owns those after A1 merges.
- No choice between `wolf-rbac.server` and `wolf_url`.
- No Task 7 HTTP snapshot, Task 8 stream router or Task 9 supervisor work.
- No generic consumer plugin framework beyond the seven registry-owned schemas.
