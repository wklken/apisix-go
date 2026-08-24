# Immutable Task 6 Lane X1 Composite Child Ownership Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prepare every `workflow` and `multi-auth` child as an immutable,
attempt-owned plugin instance that keeps the outer resource provenance, uses
the final generation metadata and consumer views, and retires exactly once in
reverse acquisition order.

**Architecture:** A serial integration owner first adds one cycle-free child
preparation boundary between `pkg/plugin/base` and `pkg/plugin`. It derives
child secret authority only through the already-landed
`ScopedSecretAccess.Child`, constructs a child from its exact registry key,
passes the same immutable dependency bundle, and retains a real
attempt-qualified `plugin.InstanceKey`. Source occurrences remain
attempt-authorized secret coordinates only; they are never runtime bindings.
CP5 later defines one compiler-private effective-plugin materializer, and
Immutable Compiler Tasks 7/8 call it only after computing the winning outer
config, final scope/provenance, and HTTP/stream resource context. File-exclusive
workers migrate `workflow` and `multi-auth` to two-phase validate/prepare flows;
the integration owner reviews both diffs, runs cross-package gates, and creates
the only commits.

**Tech Stack:** Go 1.26, `pkg/compiler.PreparationAttempt`,
`pkg/plugin.FactoryInstance`, `pkg/plugin.InstanceKey`,
`pkg/plugin/base.ScopedSecretAccess`, `runtime.MetadataView`,
`base.ConsumerLookup`, focused unit/race tests.

**Spec:**
[`docs/superpowers/plans/2026-08-24-immutable-task6-c6.4-plugin-runtime.md`](2026-08-24-immutable-task6-c6.4-plugin-runtime.md), Task 8;
[`docs/superpowers/plans/2026-08-24-immutable-task6-total-plan.md`](2026-08-24-immutable-task6-total-plan.md), Task 7;
[`docs/superpowers/plans/2026-08-23-immutable-compiler-plugin-runtime.md`](2026-08-23-immutable-compiler-plugin-runtime.md), Task 6 ownership transaction.

## Global Constraints

- Bind execution to the exact integration `HEAD` containing accepted S1
  `limit-count`, accepted S3-0 compiler-discard, every accepted A1 auth leaf,
  and later accepted corrections. Planning-time `HEAD` is `9ebcd2b5`, but it is
  not the execution baseline; workers branch only after the owner records the
  newer exact SHA.
- X1-W (`pkg/plugin/workflow/**`) and X1-A
  (`pkg/plugin/multi_auth/**`) may run in parallel only after X1-I, the shared
  seam, is reviewed and committed. They never edit shared files or each
  other's package.
- Run Go commands from the active worktree after `source .envrc` with
  `GOFLAGS=-mod=readonly`. Do not run broad `go test ./...` or `make test`.
- Current interfaces are facts, not proposals:
  `ScopedSecretAccess.Child(factory string)`,
  `PreparationAttempt.PrepareScopedPluginSecrets`,
  `plugin.NewFactoryInstance(factory, deps)`,
  `plugin.BindAttemptResolvedPlugin`, `runtime.MetadataView`, and
  `base.ConsumerLookup`. New interfaces below are explicitly produced by
  X1-I; workers must not assume differently named helpers exist.
- Child secret authority preserves outer attempt, generation, domain, source,
  and `generation.ResourceKey`; only `Scope.Plugin` changes to the child
  factory. A composite never constructs `secret.Scope`, reads capability
  internals, registers an attempt, or accepts a resolver/Store.
- Keep source authority and effective runtime identity separate. A bound
  `FactoryOccurrence` authorizes the source resource used by the existing
  attempt-scoped secret hook; it does not choose the runtime config, binding
  scope, provenance, route/service/stream context, or `InstanceKey`. Immutable
  Compiler Tasks 7/8 compute those effective values after precedence/merge and
  pass them to CP5's compiler-private materializer.
- Validate raw schemas for all children before the first materialization call.
  Partial failure installs no incomplete child and closes every acquired child
  exactly once in reverse order.
- The scoped path never calls `base.MaterializePluginSecrets`, a child's
  `MaterializeSecrets`, Store, `BasePlugin.DataEncryption`, or a global
  resolver. Transitional legacy preparation remains a separately named,
  nil-immutable-dependency compatibility path only until the joint Task 9 cutover; scoped code
  never calls or falls through to it.
- Every child receives the same generation-local `MetadataView`,
  `ConsumerLookup`, `TaskRegistry`, effective config, and secret capability as
  its outer plugin. A non-nil consumer lookup is authoritative and a miss never
  falls back to Store.
- Child identity includes exact registry factory, attempt ID, the effective
  outer scope/provenance supplied by Immutable Compiler Task 7/8, canonical
  child config, and stable structural position. Equal children in different
  positions and two generations cannot collide. Raw source occurrence identity
  never substitutes for effective outer scope/provenance.
- Public config and errors never expose a raw reference, environment name,
  Vault path, ciphertext, lookup credential, or plaintext. Tests use poison
  strings and assert their absence from errors, logs, and public config.
- Preserve workflow rule/action order, first matching rule and first action,
  consumer/group override semantics, multi-auth fallback order, body replay,
  winning authentication state, status/body/header behavior, and documented
  compatibility gaps.
- Workers return owned-path diffs and evidence only. They do not commit, push,
  open a PR, merge, cherry-pick, or edit `master`. Only the Task 6 integration
  owner reviews and commits. Push/PR remains outside current authority.

---

## Dependency and ownership graph

```text
accepted S1 limit-count --------------------+
accepted S3-0 + A1 auth leaves ------------+--> exact X1 integration HEAD
accepted relevant leaf corrections --------+              |
                                                          v
                                              X1-I shared child seam
                                                 /               \
                                                v                 v
                                      X1-W workflow       X1-A multi-auth
                                                 \               /
                                                  v             v
                                               X1-G integration gate
                                                        |
                                                        v
                                  CP5 private effective-plugin materializer
                                              /                 \
                                             v                   v
                           Immutable Task 7 HTTP       Immutable Task 8 stream
                           effective outer specs       effective outer specs
                                             \                   /
                                              v                 v
                                             Task 9 joint cutover
```

| Owner | Exclusive production paths | Exclusive test paths |
| --- | --- | --- |
| X1-I integration owner | `pkg/compiler/composite_children.go`, minimum target-aware edits in `occurrence.go`, `hooks.go`, and accepted S3-0 `discarded_secret_preparer.go`; `pkg/plugin/base/composite.go`, `pkg/plugin/base/types.go`, `pkg/plugin/composite_preparer.go`, `pkg/plugin/instance.go`, minimum helper in `pkg/plugin/base/secrets.go` | corresponding compiler/plugin/base tests; `pkg/plugin/schema_witness_test.go` only if its inventory requires the accessor |
| X1-W workflow worker | `pkg/plugin/workflow/**` | same package only |
| X1-A multi-auth worker | `pkg/plugin/multi_auth/**` | same package only |
| X1-G integration owner | one shared AST contract test under `pkg/plugin` only if package-local scans cannot prove the invariant | no leaf production files |

X1-I is serial. X1-W and X1-A branch from its exact commit into separate
worktrees. X1-G applies one diff at a time, reviews the full result, and resolves
conflicts semantically; it never takes whole-file ours/theirs for convenience.

## Current-source facts

- `workflow.MaterializeSecrets` directly creates `limit-req`, `limit-conn`, and
  `limit-count` and calls `base.MaterializePluginSecrets`.
- `workflow.PostInit` calls child `PostInit`; `Stop` already clears action
  pointers and stops recorded owners in reverse order.
- `workflow.withConsumerActionOverride` reads consumer groups from
  `store.GetConsumerGroup` during requests.
- `multi-auth.PostInit` validates, constructs, parses, legacy-materializes, and
  initializes every auth child. It has no composite-level `Stop` owner.
- `ScopedSecretAccess.Child` preserves all private authority fields and only
  replaces `scope.Plugin`.
- `FactoryInstance` preserves the exact private registry key. X1 never infers
  identity from Go type or `GetName`.
- CP3 publication occurrences do not represent nested `workflow`/`multi-auth`
  factories. Without X1's target-aware nested inventory, S3-0 cannot consume
  compiler-discard auth fields inside `multi-auth`, and pure scoped-support
  validation cannot prove the nested `limit-count` owner before registration.
- A CP3/Task 2A occurrence proves only source admission/secret authority. The
  effective HTTP/stream merge has not happened at that point, so neither the
  raw occurrence nor its config is a runtime `plugin.Binding`.
- `InstanceIdentityInput` currently hashes plugin config, filter, and error
  response. X1 adds an optional position; zero-value non-composite identities
  remain unchanged.

---

### Task 1: Bind the exact execution baseline

**Files:** Read-only inspection of accepted commits and the files named above.

**Interfaces:**
- Consumes: accepted S1/A1/S3 interfaces at one exact revision.
- Produces: an evidence note containing `X1_BASE`, clean status, dependency
  proofs, and passing package baselines; no source change.

- [ ] **Step 1: Record and validate the exact baseline**

```bash
source .envrc
export GOFLAGS=-mod=readonly
X1_BASE=$(git rev-parse HEAD)
git status --short
git show -s --format='%H %s' "$X1_BASE"
rg -n 'func \(access ScopedSecretAccess\) Child|type FactoryInstance|func NewFactoryInstance|func BindAttemptResolvedPlugin' \
  pkg/plugin/base/secrets.go pkg/plugin/scoped_preparation.go pkg/plugin/executor.go
rg -n 'func \(.*\) MaterializeScopedSecrets' pkg/plugin/limit_count \
  pkg/plugin/basic_auth pkg/plugin/key_auth pkg/plugin/jwt_auth \
  pkg/plugin/hmac_auth pkg/plugin/ldap_auth pkg/plugin/jwe_decrypt \
  pkg/plugin/wolf_rbac
```

Expected: no unexpected status; exact SHA recorded; shared APIs present;
accepted `limit-count` and auth package states agree with the integration
ledger. If a dependency is missing or a signature differs, stop and refresh
this plan before branching.

- [ ] **Step 2: Run package baselines**

```bash
go test ./pkg/plugin/workflow ./pkg/plugin/multi_auth -count=1
go test -race ./pkg/plugin/workflow ./pkg/plugin/multi_auth \
  -run '(Workflow|MultiAuth|Handler|Stop|Consumer)' -count=1
```

Expected: PASS, or record an exact pre-existing failure before mutation. Do not
call a narrowed selector a full package pass.

- [ ] **Step 3: Record legacy edges as architecture RED**

```bash
rg -n 'MaterializePluginSecrets|store\.GetConsumerGroup|func \(p \*Plugin\) PostInit|func \(p \*Plugin\) Stop' \
  pkg/plugin/workflow pkg/plugin/multi_auth
```

Expected: workflow legacy materialization/Store lookup are visible; multi-auth
constructs in `PostInit` and has no outer `Stop`.

---

### Task 2: Add the cycle-free attempt-owned child seam

**Files:**
- Create: `pkg/compiler/composite_children.go`
- Create: `pkg/compiler/composite_children_test.go`
- Modify: `pkg/compiler/occurrence.go`
- Modify: `pkg/compiler/hooks.go`
- Modify after accepted S3-0: `pkg/compiler/discarded_secret_preparer.go`
- Modify after accepted S3-0: `pkg/compiler/discarded_secret_preparer_test.go`
- Create: `pkg/plugin/base/composite.go`
- Modify: `pkg/plugin/base/types.go`
- Modify: `pkg/plugin/base/secrets.go`
- Create: `pkg/plugin/composite_preparer.go`
- Create: `pkg/plugin/composite_preparer_test.go`
- Modify: `pkg/plugin/instance.go`
- Modify only if required: `pkg/plugin/schema_witness_test.go`

**Interfaces:**
- Consumes: current `ScopedSecretAccess.Child`, `FactoryInstance`,
  `ResolveDescriptorForFactory`, `BindAttemptResolvedPlugin`, and immutable
  `base.Dependencies` fields.
- Produces:

```go
// pkg/compiler/composite_children.go; pure pre-registration inventory
type compositeChildOccurrenceSpec struct {
    outer    factoryOccurrenceSpec
    factory  string
    position string
    config   map[string]any
}

func compositeChildOccurrenceSpecsFromCandidates(
    context.Context,
    map[generation.Domain]generation.PublicationCandidate,
) ([]compositeChildOccurrenceSpec, error)

// Bound only after newPreparationAttempt creates package-private authority.
type compositeChildOccurrence struct {
    outer    FactoryOccurrence
    factory  string
    position string
    config   map[string]any
}

func bindCompositeChildOccurrences(
    PreparationAttempt,
    []compositeChildOccurrenceSpec,
) ([]compositeChildOccurrence, error)

func validateCompositeScopedSecretSupport(
    []compositeChildOccurrenceSpec,
    *capability.SecretDeclarationCatalog,
) error
```

```go
// pkg/compiler/hooks.go; package-private, usable only by compiler-owned
// target materialization after registration
func (attempt PreparationAttempt) materializeCompositeSecret(
    context.Context,
    compositeChildOccurrence,
    string, // canonical manifest field
    string, // raw leaf
) (secret.Value, error)
```

```go
// pkg/plugin/base/composite.go
type CompositeChildSpec struct {
    Factory  string
    Config   map[string]any
    Position string
}

type PreparedCompositeChild interface {
    Factory() string
    Instance() any
    Close()
}

type CompositeChildPreparer interface {
    Prepare(context.Context, ScopedSecretAccess, CompositeChildSpec) (
        PreparedCompositeChild, error,
    )
}
```

```go
// additive field/accessor in pkg/plugin/base/types.go
type Dependencies struct {
    // existing fields remain unchanged
    CompositeChildren CompositeChildPreparer
}

func (p *BasePlugin) CompositeChildPreparer() CompositeChildPreparer
```

```go
// pkg/plugin/composite_preparer.go
func NewCompositeChildPreparer(
    deps base.Dependencies,
    attempt secret.AttemptID,
    scope Scope, // caller-supplied effective outer scope
    provenance ResourceProvenance, // caller-supplied effective outer provenance
) (base.CompositeChildPreparer, error)
```

The constructor treats `scope` and `provenance` as already-effective caller
inputs. It has no occurrence parameter and cannot derive them from source
inventory.

```go
// additive optional field in pkg/plugin/instance.go
type InstanceIdentityInput struct {
    PluginConfig      any    `json:"plugin_config"`
    Filter            any    `json:"filter,omitempty"`
    ErrorResponse     any    `json:"error_response,omitempty"`
    CompositePosition string `json:"composite_position,omitempty"`
}
```

#### Task 2A: Close the nested compiler-discard/support gap

“Final candidate” in this subtask means the normalized, owned publication
candidate selected before registration. It does not mean a final HTTP/stream
runtime plugin binding; precedence/merge, runtime scope/provenance, and resource
context are still owned by Immutable Compiler Tasks 7/8.

- [ ] **Step 1: Write RED final-candidate child inventory tests**

Add candidate and recovery cases for a route/service containing workflow
`limit-count` children and all seven multi-auth child factories. Assert
canonical outer resource order, workflow rule/action order, multi-auth array
order plus sorted keys within one object, stable structural positions, and
defensive config copies. Tombstones and disabled/non-final resources produce
no child occurrence. Cover outer `_meta.disable=true`, strip valid outer
`_meta` before reading composite fields, and reject an invalid `_meta.disable`
type through the existing integrity/schema path before registration.

Add `TestPrepareCompilerDiscardSecretsOwnsNestedMultiAuthCompatibilityFields`:
put poison values in nested basic/key/JWT/HMAC/LDAP/JWE compatibility fields;
assert S3-0 materializes/drops each through the child factory plus the outer
route occurrence, retains no value/descriptor, and does not mutate the final
candidate clone. Add a nested `limit-count` support test proving the pure gate
rejects a missing scoped owner with zero candidate/recovery registration calls
and zero resolver calls.

- [ ] **Step 2: Run compiler RED**

```bash
go test ./pkg/compiler \
  -run '^(TestFinalCandidateCompositeChildOccurrences|TestPrepareCompilerDiscardSecretsOwnsNested|TestValidateScopedSecretSupportRejectsNested)' \
  -count=1
```

Expected: compile/test failure because CP3/S3-0 only see top-level factories.

- [ ] **Step 3: Implement pure nested extraction and pre-registration support**

Parse only normalized final candidate clones. Candidate/recovery paths call
`compositeChildOccurrenceSpecsFromCandidates` before registration and pass its
result to `validateCompositeScopedSecretSupport`. After registration and
`newPreparationAttempt`, `bindCompositeChildOccurrences` replaces each pure
outer spec with the exact package-private `FactoryOccurrence` owned by that
attempt; missing, extra, duplicate, or mismatched outer authority fails before
any child materialization. Recognize exactly the two outer factories and their
current schema shapes:

```text
workflow.rules[*].actions[*] = [child-factory, child-config]
multi-auth.auth_plugins[*]   = {child-factory: child-config, ...}
```

Do not import either leaf package and do not create a second secret-field
table. The child factory names come from final raw config; declaration target
and fields come only from the accepted manifest catalog. Before registration,
extend the pure support gate so every effective child with at least one
`plugin`-target `plugin_config` declaration must implement
`base.ScopedSecretMaterializer`. Compiler-discard-only children need no fake
plugin method. Unknown/disabled/schema-invalid outer config remains owned by
the existing compiler validation and composite raw validation; never resolve
it speculatively.

The pure phase receives only already-revalidated owned final
`PublicationSet.Domains`/recovery candidates and final outer
`factoryOccurrenceSpec` values. It receives no `PreparationAttempt`,
`GenerationCapability`, registration, broker, or resolver. Candidate and
recovery tests assert a nested missing owner returns before
`RegisterCandidate`/`RegisterRecovery` and every resolver hook.

Use the same effective top-level plugin metadata semantics already tested by
the current route path: a disabled outer composite contributes no child spec,
and `_meta` is not part of its child config. Do not invent a nested-child
`_meta` contract; nested action/auth config remains governed by each current
composite schema and compatibility tests.

- [ ] **Step 4: Extend S3-0 discard without weakening occurrence authority**

`materializeCompositeSecret` accepts only a bound
`compositeChildOccurrence`, proves its outer occurrence belongs to this attempt
with `Source=plugin_config`, then copies the exact domain/resource and
substitutes only the already-inventoried child factory. It is package-private
and called only by `prepareCompilerDiscardSecrets`. Catalog admission still
validates factory/target/field. Resolve and
immediately drop compiler-discard leaves; retain no plaintext, value,
descriptor, config rewrite, or child occurrence in the public plugin hook list.

Run GREEN:

```bash
go test ./pkg/compiler \
  -run '^(TestFinalCandidateCompositeChildOccurrences|TestPrepareCompilerDiscardSecrets|TestValidateScopedSecretSupport|TestAttemptFactory)' \
  -count=1
go test -race ./pkg/compiler \
  -run '^(TestFinalCandidateCompositeChildOccurrences|TestPrepareCompilerDiscardSecrets|TestAttemptFactory)' \
  -count=1
```

This is the required compiler-discard bridge. A composite plugin must not
materialize, ignore, or retain these phantom fields itself.

#### Task 2B: Add the cycle-free runtime child preparer

- [ ] **Step 1: Write RED seam tests**

Use a package-local sentinel implementing `base.ScopedSecretMaterializer`,
immutable dependency probes, and idempotent `Stop`. Temporarily replace one
existing manifest-declared secret factory's registry constructor for authority
tests and restore it with `t.Cleanup`. For same-Go-type identity, use the
already manifest-declared `serverless-pre-function` and
`serverless-post-function` factory pair (or temporarily replace those two
existing entries with closures returning one sentinel type); never invent an
unmanifested test factory because descriptor resolution must remain real. Add:

```go
func TestCompositeChildPreparerPreservesOuterAuthorityAndDependencies(t *testing.T)
func TestCompositeChildPreparerUsesExactRegistryFactoryForSameType(t *testing.T)
func TestCompositeChildPreparerSeparatesSiblingPositionAndAttemptKeys(t *testing.T)
func TestCompositeChildPreparerFailureStopsConstructedChildOnce(t *testing.T)
func TestCompositeChildPreparerRedactsRawCredentialFailures(t *testing.T)
```

Assert the broker sees the child factory with exact outer source generation,
attempt, HTTP domain, `routes/route-x1`, `plugin_config`, and manifest field.
Pass explicit effective outer scope/provenance separately. Use equal configs
with different positions and a second attempt. Inspect the package-private
returned binding and assert all `InstanceKey` values differ while the supplied
effective outer scope/provenance stay equal. Add a case where source provenance
differs from effective provenance and prove secret authority follows the source
while the binding/child key follows the effective values.

- [ ] **Step 2: Run RED**

```bash
go test ./pkg/plugin/base ./pkg/plugin -run '^TestCompositeChildPreparer' -count=1
```

Expected: compile failure because the new seam and position do not exist.

- [ ] **Step 3: Implement the lower-layer contract only**

Add the types exactly above. Refactor the existing scoped body only enough to
expose:

```go
func MaterializeScopedCompositeChildSecrets(
    ctx context.Context,
    access ScopedSecretAccess,
    p any,
) error
```

It calls the same scoped-only implementation as
`MaterializeScopedPluginSecrets`; it never reconstructs a scope, accepts a
capability, or falls back to legacy materialization. Preserve the unresolved
reference scan and fixed redacted error.

Implement `compositeChildPreparer.Prepare` in this order:

```text
validate context/spec/attempt/effective outer scope/provenance
-> childAccess := access.Child(spec.Factory)
-> clone spec.Config
-> NewFactoryInstance(spec.Factory, childDependencies)
-> child.Init()
-> compile child schema and validate raw cloned config
-> util.Parse(clonedConfig, child.Config())
-> MaterializeScopedCompositeChildSecrets(ctx, childAccess, child)
-> child.PostInit()
-> ResolveDescriptorForFactory(spec.Factory, child)
-> BindAttemptResolvedPlugin(attempt, descriptor, child,
     effectiveOuterScope, effectiveOuterProvenance,
     InstanceIdentityInput{
       PluginConfig: child.Config(), CompositePosition: spec.Position,
     })
-> return preparedCompositeChild{binding: binding}
```

Copy outer dependencies but set `CompositeChildren` nil before constructing the
leaf. Reject nil instance, nil config, and empty factory/position before
construction; an empty non-nil config is valid for existing auth children.
After construction, every failure calls child `Stop` once when supported.
Returned `Close` uses `sync.Once`. Return context cancellation unchanged; map
all other errors to a fixed safe factory/position diagnostic without raw input.

- [ ] **Step 4: Run GREEN and race tests**

```bash
go test ./pkg/plugin/base ./pkg/plugin -run '^TestCompositeChildPreparer' -count=1
go test -race ./pkg/plugin/base ./pkg/plugin \
  -run '^(TestCompositeChildPreparer|TestScopedSecretAccess)' -count=1
golangci-lint run ./pkg/compiler/... ./pkg/plugin/base/... ./pkg/plugin/...
make build
git diff --check
```

Expected: PASS; no unrelated formatting.

- [ ] **Step 5: Integration-owner review and commit**

Verify non-composite `InstanceKey` golden tests are unchanged because the new
field is omitted when empty. Only the integration owner runs:

```bash
git add pkg/plugin/base/composite.go pkg/plugin/base/types.go \
  pkg/plugin/base/secrets.go pkg/plugin/composite_preparer.go \
  pkg/plugin/composite_preparer_test.go pkg/plugin/instance.go \
  pkg/compiler/composite_children.go pkg/compiler/composite_children_test.go \
  pkg/compiler/occurrence.go pkg/compiler/hooks.go \
  pkg/compiler/discarded_secret_preparer.go \
  pkg/compiler/discarded_secret_preparer_test.go
git diff --cached --check
git commit -m 'feat(plugin): bind attempt-owned composite children'
```

Stage `pkg/plugin/schema_witness_test.go` too only when actually changed. Record
the new exact SHA; both package workers branch from it.

---

### Task 3: Migrate `workflow` to scoped child preparation

**Files:**
- Modify: `pkg/plugin/workflow/plugin.go`
- Modify: `pkg/plugin/workflow/plugin_test.go`
- Create: `pkg/plugin/workflow/scoped_children_test.go`
- Modify only if behavior evidence changes: `pkg/plugin/workflow/manifest_test.go`

**Interfaces:**
- Consumes: X1-I `base.CompositeChildPreparer`, accepted scoped
  `limit-count`, immutable `base.ConsumerLookup`, and authority derived through
  `ScopedSecretAccess.Child` inside the shared preparer.
- Produces:

```go
func (p *Plugin) MaterializeScopedSecrets(
    context.Context,
    base.ScopedSecretAccess,
) error

func (p *Plugin) Stop()
```

The existing `MaterializeSecrets() error` remains a separately implemented
legacy Builder seam until Task 9; the scoped method never calls it.

- [ ] **Step 1: Write RED scoped workflow tests**

Use the real manifest/catalog, `secret.NewScopedMaterializer`, an authorized
HTTP route source containing `workflow`, and a real X1-I child preparer
constructed with explicit effective route scope/provenance. This is a seam
test, not raw-occurrence-to-binding integration. Add:

```go
func TestMaterializeScopedSecretsBindsLimitCountToOuterAttemptAndRoute(t *testing.T)
func TestMaterializeScopedSecretsSeparatesEqualLimitCountSiblingPositions(t *testing.T)
func TestMaterializeScopedSecretsFailureStopsEarlierChildrenInReverseOnce(t *testing.T)
func TestMaterializeScopedSecretsRejectsAllInvalidChildrenBeforeResolution(t *testing.T)
func TestScopedWorkflowGenerationOverlapKeepsChildrenAndSecretsIsolated(t *testing.T)
func TestScopedWorkflowConsumerGroupLookupNeverFallsBackToStore(t *testing.T)
func TestScopedWorkflowErrorsAndConfigHideCredentialMaterial(t *testing.T)
```

The broker records child factory `limit-count` with the outer route, domain,
generation, and attempt. The sibling test uses equal `limit-count` actions at
different indexes. The partial-failure fixture uses a scripted
`CompositeChildPreparer` returning real expected child types inside recording
prepared-child handles, then fails the third; assert
`third-self, second, first` exactly once after a repeated outer `Stop`.

Put a valid secret reference in child one and an invalid schema/group in a
later child; assert zero resolver calls and no installed children. For N/N+1,
use the same route key with different attempt IDs/resolved limit keys; retiring
N must not change N+1.

- [ ] **Step 2: Run RED**

```bash
go test ./pkg/plugin/workflow -run 'Scoped|Attempt|Child|Cleanup|Consumer' -count=1
```

Expected: failure because workflow lacks scoped materialization, still creates
legacy children directly, and still uses Store for group lookup.

- [ ] **Step 3: Split validation from acquisition**

Keep action-array parsing unchanged. Extend `ValidatePreMaterialization` to
validate all actions before constructing one child:

```text
structural rule/action position
-> supported action name
-> enabled child factory
-> child raw schema
-> current limit-count group rejection
-> return status 200..599
```

Expression compilation stays in outer `PostInit`. Preserve safe diagnostics
for unsupported actions, disabled children, invalid return codes, invalid
limit-count config, and unsupported group. Validation never resolves a secret
or starts a child.

- [ ] **Step 4: Implement atomic scoped preparation**

Use this stable position:

```go
func workflowChildPosition(rule, action int) string {
    return "workflow/rule/" + strconv.Itoa(rule) + "/action/" + strconv.Itoa(action)
}
```

`MaterializeScopedSecrets` must:

1. call `stopChildren` to retire previous package-local state;
2. validate all actions;
3. require `p.CompositeChildPreparer()` for every limit child;
4. call `Prepare` in canonical rule/action order with factory, config, and
   stable position;
5. append each returned owner immediately;
6. assert the returned instance is the expected concrete child type;
7. stage a descriptor-safe clone of materialized child config without mutating
   the action map;
8. apply existing route/service context;
9. publish all staged config replacements, `children`, action pointers, and
   owners only after complete success;
10. on failure close in-progress/prior owners in reverse and leave every
    action runtime pointer nil.

The shared preparer already runs child `PostInit`; outer `PostInit` never calls
it again. Outer `PostInit` compiles expressions and validates only outer
behavior. `stopChildren` detaches/clears state before reverse closing so
repeated or concurrent `Stop` cannot double-stop a child.

- [ ] **Step 5: Move group lookup to the immutable view**

Make `withConsumerActionOverride` a method and use
`p.ConsumerLookup().ConsumerGroupByID`. A non-nil lookup hit or miss is
authoritative. Keep the old Store call only in a private function named
`legacyConsumerGroupByIDWhenLookupIsNil`, called exclusively when lookup is
nil. Add an explicit C6.6 deletion comment naming that function.

Preserve override union order: group plugins, consumer plugins, action name.
Return the original request when override is disabled or no consumer exists.

- [ ] **Step 6: Keep legacy preparation isolated**

Retain `MaterializeSecrets` for the current Builder. It may call only a helper
whose name begins `legacy`, construct the current concrete children, and call
`base.MaterializePluginSecrets`. It shares only pure validation/iteration,
descriptor-safe config synchronization, and reverse cleanup with the scoped
path. The scoped method contains no call edge to the legacy helper.

- [ ] **Step 7: Run workflow gates**

```bash
go test ./pkg/plugin/workflow -count=1
go test -race ./pkg/plugin/workflow \
  -run '(Scoped|Attempt|Child|Cleanup|Stop|Consumer|Handler)' -count=1
golangci-lint run ./pkg/plugin/workflow/...
git diff --check
```

Expected: all existing behavior tests and new scoped tests PASS. Legacy Store
fixtures prove only the temporary nil-lookup branch, not scoped correctness.

- [ ] **Step 8: Return owned diff without committing**

```bash
git diff -- pkg/plugin/workflow
git status --short pkg/plugin/workflow
```

Return diff and exact results; do not commit.

---

### Task 4: Migrate `multi-auth` to scoped child preparation

**Files:**
- Modify: `pkg/plugin/multi_auth/plugin.go`
- Modify: `pkg/plugin/multi_auth/plugin_test.go`
- Modify: `pkg/plugin/multi_auth/request_phase_test.go`
- Modify: `pkg/plugin/multi_auth/jwe_decrypt_test.go`
- Create: `pkg/plugin/multi_auth/scoped_children_test.go`

**Interfaces:**
- Consumes: X1-I `base.CompositeChildPreparer`, accepted A1 auth leaves,
  generation-local `ConsumerLookup` and `MetadataView`.
- Produces:

```go
func (p *Plugin) ValidatePreMaterialization() error

func (p *Plugin) MaterializeScopedSecrets(
    context.Context,
    base.ScopedSecretAccess,
) error

func (p *Plugin) MaterializeSecrets() error // transitional legacy only
func (p *Plugin) Stop()
```

- [ ] **Step 1: Write RED multi-auth tests**

Use an authorized route source occurrence, real registration, a real X1-I
child preparer constructed with explicit effective route scope/provenance, and
immutable A1 consumer bindings. This does not publish the source occurrence as
an outer binding. Add:

```go
func TestMaterializeScopedSecretsPreparesEveryAuthChildBeforePostInit(t *testing.T)
func TestMaterializeScopedSecretsPassesImmutableConsumerAndMetadataViews(t *testing.T)
func TestMaterializeScopedSecretsPreservesOuterAuthorityForEveryAuthFactory(t *testing.T)
func TestMaterializeScopedSecretsSeparatesEqualSiblingPositionsAndAttempts(t *testing.T)
func TestMaterializeScopedSecretsThirdFailureStopsEarlierChildrenReverseOnce(t *testing.T)
func TestMaterializeScopedSecretsValidatesAllRawChildrenBeforeSecretAccess(t *testing.T)
func TestScopedMultiAuthGenerationOverlapUsesOnlyItsOwnConsumerBindings(t *testing.T)
func TestScopedMultiAuthLookupMissCannotReachLegacyStore(t *testing.T)
func TestScopedMultiAuthErrorsDoNotExposeCredentialMaterial(t *testing.T)
```

Keep current body-isolation assertions and prove `PostInit` neither reconstructs
nor reinitializes children. Cover all seven factories:

```text
basic-auth, key-auth, jwt-auth, hmac-auth,
ldap-auth, jwe-decrypt, wolf-rbac
```

For S3-0 compiler-discard route fields, assert no forged child scope is
created. Consumer credentials remain A1-owned through the injected lookup.

- [ ] **Step 2: Run RED**

```bash
go test ./pkg/plugin/multi_auth -run 'Scoped|Attempt|Child|Cleanup|Consumer' -count=1
```

Expected: failure because construction remains in `PostInit`, there is no
scoped owner/Stop, and tests still use global Store setup.

- [ ] **Step 3: Enumerate deterministically and validate all first**

Preserve outer `auth_plugins` array order. Sort keys lexicographically inside a
multi-factory object, replacing Go map iteration nondeterminism without dropping
children. Use:

```go
func authChildPosition(entry int, factory string) string {
    return "multi-auth/entry/" + strconv.Itoa(entry) + "/factory/" + factory
}
```

`ValidatePreMaterialization` completes before any construction:

```text
at least two effective children
-> supported exact factory
-> enabled check
-> temporary child Init only for schema acquisition
-> compile and validate raw child config
```

It does not parse into a retained child, resolve, start a client/task, or look
up a consumer. Preserve safe unsupported/disabled diagnostics.

- [ ] **Step 4: Acquire children outside `PostInit`**

`MaterializeScopedSecrets` clears prior state, validates all specs, requires the
injected preparer, and prepares in canonical order. Append each owner
immediately, type assert `Instance()` to `authPlugin`, and stage
`configuredAuth` plus descriptor-safe child config maps locally. Publish all
nested config replacements, `p.auths`, and owners only after all succeed. This
atomic rewrite is required so the outer scoped boundary's unresolved-reference
scan sees descriptors rather than original child references.

On failure, close the failing returned child and prior owners in reverse; leave
`p.auths` empty. `Stop` atomically detaches slices before reverse close and is
idempotent under concurrent calls. The shared preparer already runs child
`PostInit`; outer `PostInit` checks only that a fully prepared list exists with
at least two children. It never constructs, parses, materializes, or initializes
a child.

- [ ] **Step 5: Preserve auth behavior with immutable lookups**

Keep request phase, body isolation/replay, diagnostic bounds, winner selection,
authentication-state publication, and final 401 unchanged. New scoped tests
construct `runtime.ConsumerBindings` and pass them through `base.Dependencies`
to outer and children.

Prove each accepted A1 leaf sees the same generation-local lookup. A non-nil
miss follows A1 failure/anonymous behavior and cannot call Store. N/N+1 uses
the same credential key mapped to different consumers; each composite
authenticates only its own generation before and after retiring the other.

- [ ] **Step 6: Add separate legacy Builder preparation**

Add `MaterializeSecrets` using deterministic specs/raw validation and a private
`legacyPrepareAuthChild`; only that helper calls
`base.MaterializePluginSecrets`. It fully prepares children before outer
`PostInit` and owns reverse cleanup. Scoped preparation never selects it.

Update legacy test helpers to call `base.MaterializePluginSecrets(p)` before
`p.PostInit()`, matching current Builder order. `PostInit` invokes neither
materializer; missing preparation fails closed.

- [ ] **Step 7: Run multi-auth gates**

```bash
go test ./pkg/plugin/multi_auth -count=1
go test -race ./pkg/plugin/multi_auth \
  -run '(Scoped|Attempt|Child|Cleanup|Stop|Consumer|Handler|RequestPhase)' -count=1
golangci-lint run ./pkg/plugin/multi_auth/...
git diff --check
```

Expected: current fallback/body/authentication behavior and new ownership tests
PASS. Legacy Store tests are compatibility evidence only.

- [ ] **Step 8: Return owned diff without committing**

```bash
git diff -- pkg/plugin/multi_auth
git status --short pkg/plugin/multi_auth
```

Return diff and exact results; do not commit.

---

### Task 5: Integrate both composites and prove no scoped bypass

**Files:**
- Review: all X1-I, X1-W, and X1-A paths
- Create only when package-local scans cannot prove the edge:
  `pkg/plugin/composite_contract_test.go`
- Modify parent Task 6 plans only after acceptance, by integration owner

**Interfaces:**
- Consumes: both package diffs applied to the X1-I descendant.
- Produces: one reviewed X1 checkpoint plus the input/order contract for CP5's
  compiler-private effective-plugin materializer; no production injection,
  master merge, push, or PR.

- [ ] **Step 1: Apply and review one diff at a time**

After applying X1-W, run its full/focused gates. Then apply X1-A and run its
gates. Inspect:

```bash
git diff --stat
git diff -- pkg/plugin/base pkg/plugin pkg/plugin/workflow pkg/plugin/multi_auth
git status --short
```

Reject changes to Store, compiler, runtime consumer construction, metadata
owners, manifest declarations, route/server activation, or unrelated plugins.

- [ ] **Step 2: Run AST/import-aware legacy-edge gates**

Add or extend a Go AST test that inspects method bodies, not comments. Assert:

```text
workflow.MaterializeScopedSecrets -> no MaterializeSecrets or MaterializePluginSecrets
multi_auth.MaterializeScopedSecrets -> no MaterializeSecrets or MaterializePluginSecrets
workflow.PostInit -> no child PostInit call
multi_auth.PostInit -> no child constructor, util.Parse, or materializer call
scoped methods -> no Store selector/import
legacy helpers -> only remaining direct MaterializePluginSecrets calls
workflow Store group lookup -> reachable only from nil-lookup legacy helper
```

Also run:

```bash
rg -n 'MaterializePluginSecrets|MaterializeSecrets|store\.|GetConsumerGroup|GetConsumerByPluginKey' \
  pkg/plugin/workflow pkg/plugin/multi_auth
```

Expected: each result is a test, explicitly named legacy helper, or Task 9
deletion marker. No scoped or `PostInit` edge remains.

- [ ] **Step 2A: Freeze the effective-materializer handoff without injecting from raw occurrences**

X1 owns the shared `NewCompositeChildPreparer` constructor and tests it with
explicit effective outer scope/provenance. X1 does **not** add a concrete
`PluginPreparer`, enumerate raw `FactoryOccurrence` values into runtime
bindings, or install `CompositeChildren` from the pre-registration inventory.
Those occurrences remain source-authority inputs for the attempt-scoped secret
hook and the post-registration compiler-discard bridge only.

CP5 defines one compiler-private effective-plugin materializer. Its private
request keeps these inputs separate; the final Go type/name is frozen against
the CP5 integration `HEAD`, not invented by X1:

```text
source authority:
  bound FactoryOccurrence used only by PreparationAttempt secret admission

effective runtime specification:
  exact registry factory + cloned winning config
  final plugin.Scope + plugin.ResourceProvenance
  final HTTP route/service context or final stream context
  immutable base.Dependencies
```

Immutable Compiler Task 7 (HTTP) and Task 8 (stream) are the only owners that
derive that effective runtime specification. Each invokes the CP5 primitive
only after precedence/merge and context resolution. The primitive must then
execute this order; the identifiers below are sequence pseudocode, not claimed
landed APIs:

```text
effective = Task7Or8.computeEffectiveOuterSpec(...)
CP5.materializeEffectivePlugin(sourceOccurrence, effective, deps):
  childPreparer = plugin.NewCompositeChildPreparer(
      deps, attempt.AttemptID(), effective.Scope, effective.Provenance)
  outerDeps = deps with CompositeChildren = childPreparer
  outer = plugin.NewFactoryInstance(effective.Factory, outerDeps)
  attempt.PrepareScopedPluginSecrets(sourceOccurrence, outer)
  apply the caller-supplied effective HTTP/stream resource context to outer
  finish admission/PostInit/binding with effective config/scope/provenance
```

Task 7/8 pair the winner with its exact admitted source occurrence. The
primitive rejects a source/effective factory mismatch, and the existing
attempt hook rejects an occurrence not owned by that attempt; the primitive
does not invent a provenance-equivalence check or copy
scope/provenance/config from the occurrence. The child preparer is per
effective outer binding, never global or shared, and is injected before
`NewFactoryInstance` so the outer composite receives it through
`base.Dependencies`. CP5 unit tests prove the primitive's order, factory
mismatch rejection, and attempt-owned occurrence enforcement. Task 7/8
integration tests use two
effective winners backed by distinct source resources and prove neither raw
precedence losers nor source coordinates become executor bindings or exchange
attempt/provenance. X1 tests only the constructor/seam that CP5 consumes.

Before CP5/Task 7/Task 8 implementation starts, the integration owner updates
the parent plans as a status/contract change, not in the X1 code commit:

```text
CP5 / Immutable Task 6:
  define the private effective-plugin materializer and ownership cleanup;
  do not turn the raw occurrence inventory into PreparedGeneration bindings.

Immutable Task 7 HTTP:
  replace "consume pre-materialized bindings" with "compute effective outer
  specs, pair each winner with its admitted source occurrence, then call the
  CP5 materializer".

Immutable Task 8 stream:
  perform the same post-effective-merge call for enabled stream bindings;
  do not reuse HTTP route context or invent stream composites.

C6.6:
  record both deferred call paths and add no prepared production adapter.

Task 9:
  wait for accepted Task 7/8 call paths and their source/effective tests before
  deleting the legacy production seams.
```

- [ ] **Step 3: Run cross-package lifecycle gates**

```bash
go test ./pkg/plugin/base ./pkg/plugin \
  ./pkg/plugin/workflow ./pkg/plugin/multi_auth -count=1
go test -race ./pkg/plugin/base ./pkg/plugin \
  ./pkg/plugin/workflow ./pkg/plugin/multi_auth \
  -run '(Composite|Scoped|Attempt|Child|Cleanup|Stop|Consumer|Metadata|Generation)' \
  -count=1
```

Expected: PASS. Evidence includes third-child failure, reverse order, repeated
Stop, canceled preparation with uncanceled cleanup, and N/N+1 overlap.

- [ ] **Step 4: Run lint, build, and diff gates**

```bash
golangci-lint run ./pkg/plugin/base/... ./pkg/plugin/... \
  ./pkg/plugin/workflow/... ./pkg/plugin/multi_auth/...
make build
git diff --check
```

Expected: PASS. If broad plugin lint reports an unrelated known failure, rerun
with `--new-from-rev="$X1_BASE"` and report both exact results; never describe
the broad lint as passing when it did not.

- [ ] **Step 5: Review compatibility evidence explicitly**

Confirm executable coverage remains for:

```text
workflow: action arrays; first matching rule; first action only; return
200..599; limit req/conn/count; consumer/group override union; unsupported
limit-count group; route context; Stop.

multi-auth: all seven factories; every child inside each array object; ordered
fallback; successful winner; failed child cannot mutate next alternative; body
replay; bounded diagnostics; generic final 401; authentication-state and
downstream request propagation.
```

Add a focused regression only where evidence is absent. Do not expand workflow
expression or limit-count group scope.

- [ ] **Step 6: Integration-owner final review and commit**

Resolve every high/medium finding and rerun invalidated gates. Only the
integration owner runs:

```bash
git add pkg/plugin/workflow pkg/plugin/multi_auth
test ! -e pkg/plugin/composite_contract_test.go || \
  git add pkg/plugin/composite_contract_test.go
git diff --cached --check
git diff --cached --stat
git commit -m 'feat(plugin): own composite child generations'
```

Update/stage the parent CP5 and Immutable Task 7/8 plans with the mandatory
contract changes in Step 2A in a separate plan-status commit; do not sweep
unrelated concurrent plan files into the code commit. Record the new exact X1
SHA for CP5. Do not merge to `master`, push, or open a PR.

---

## CP5 and C6.6 handoff

CP5 defines and tests one compiler-private effective-plugin materializer
primitive; it does not walk raw source occurrences and publish one binding per
occurrence. The private request must carry two deliberately separate inputs:
the bound `FactoryOccurrence` for attempt-owned secret authority, and the
effective outer specification for runtime identity. The latter contains the
winning config, final factory, final `plugin.Scope`, final
`plugin.ResourceProvenance`, and resolved HTTP/stream resource context.

Immutable Compiler Task 7 builds each HTTP effective outer specification only
after plugin-config/service/route/global precedence and route/service context
resolution. Immutable Compiler Task 8 does the equivalent after stream
precedence/context resolution. They then call the CP5 primitive. The primitive
first creates a per-binding `plugin.NewCompositeChildPreparer` with the exact
`PreparationAttempt.AttemptID` plus **effective** scope/provenance, installs it
in a copy of `base.Dependencies`, and only then calls
`plugin.NewFactoryInstance` for the effective factory. It applies the caller's
effective resource context, uses the separate source occurrence with
`PreparationAttempt.PrepareScopedPluginSecrets`, and completes admission and
binding with effective config/identity. Task 7/8 must pair the effective winner
with its exact admitted source occurrence. CP5 rejects a source/effective
factory mismatch, while the existing attempt hook enforces occurrence
ownership; neither layer derives runtime scope/provenance from source
coordinates or invents a provenance-equivalence rule.

The outer remains the executor binding. Nested child bindings are
lifecycle/identity records and are not inserted independently into the route or
stream chain. X1 continues to own only the pure pre-registration nested
inventory/support check, post-registration compiler-discard materialization,
and the shared child-preparer seam. CP5 owns the primitive; Tasks 7/8 own its
effective call sites.

The prepared-generation cleanup ledger closes effective outer plugin leases in
reverse order. Each composite outer `Stop` closes its child owners in reverse
order exactly once. Attempt registration remains sole capability/revocation
owner and closes only after tasks, effective plugin leases, consumers, and
other generation resources.

C6.6 only records the still-live legacy composite and workflow Store callers.
Task 7/8 must first use the CP5 primitive and prove no raw occurrence is
published as a binding. The joint Task 9 cutover then deletes both legacy
composite materializers, the workflow nil-lookup Store helper/import, obsolete
Store fixtures, and current Builder call edges. X1 does not delete those seams
before replacement compilation exists, and does not add a runtime flag or
second activation path.

## Acceptance ledger

| Boundary | Required evidence |
| --- | --- |
| Source authority | nested pre-registration support uses owned final candidates with zero registration/resolver calls; post-registration discard uses the exact bound occurrence and attempt |
| Effective outer binding | only Task 7/8 winners become bindings; scope/provenance/context/config come from the effective specification, never a raw occurrence |
| Outer provenance | child secret scope retains exact authorized outer domain/resource/generation/attempt while child identity receives effective outer scope/provenance |
| Child identity | real `InstanceKey` contains child factory, effective outer scope/provenance, position digest, and non-zero attempt |
| Immutable dependencies | same metadata/consumer/task/config/capability bundle; non-nil miss cannot use Store |
| Atomicity | complete raw validation before materialization; Nth failure publishes no partial list |
| Cleanup | failing child self-stops; earlier children reverse-stop once; outer Stop is idempotent |
| Compatibility | workflow and multi-auth request behavior remains covered and unchanged |
| Isolation | N/N+1 do not share keys, credentials, consumers, clients, or retirement state |
| Redaction | poison raw reference/plaintext absent from errors/logs/public config |
| No bypass | scoped methods/PostInit have no Store, legacy materializer, resolver, raw capability, or registration edge |
| Delivery | integration owner alone commits; no push/PR/master mutation |

## Explicit non-goals

- No new workflow action, expression operator, NGINX variable, or
  `limit-count` group support.
- No auth protocol, response, body, anonymous consumer, JWE, LDAP, Wolf, JWT,
  HMAC, key-auth, or basic-auth behavior change.
- No new secret declaration, Store schema, consumer binding format, metadata
  precedence rule, or dependency.
- No recursive composite graph. X1 supports the two bounded composites and
  clears the child preparer's recursive dependency.
- X1 does not implement CP5 `PreparedGeneration` or its private effective
  materializer, Immutable Compiler Task 7 HTTP snapshot, Immutable Compiler
  Task 8 stream snapshot, Task 9 supervisor/worker, stream composites,
  route/server activation, or C6.6 destructive deletion. This handoff only
  constrains their source/effective boundary and call order.

## Self-review record

- **Spec coverage:** Tasks 2-4 cover authority, exact factory, metadata/consumer
  propagation, position-qualified identity, two-phase validation, reverse
  cleanup, compatibility, N/N+1, and redaction. Task 5 owns AST no-bypass gates,
  focused/race/build verification, review, and integration-only commits.
- **Dependency consistency:** X1-I precedes both workers; workflow waits for S1
  limit-count; multi-auth waits for S3-0 and A1 auth leaves; CP5 waits for the
  reviewed X1 checkpoint; Immutable Compiler Tasks 7/8 wait for the CP5 private
  primitive and invoke it only after their effective merge/context phase;
  C6.6 only records the deferred call sites; Task 9 waits for both accepted
  Task 7/8 implementations.
- **Type consistency:** current consumed signatures were verified at planning
  time. Each new type/signature is defined in Task 2 before consumption.
  `CompositePosition` is optional, preserving non-composite identities.
- **Injection consistency:** X1 tests
  `NewCompositeChildPreparer(deps, attempt, effectiveScope,
  effectiveProvenance)` as a standalone shared seam. CP5 defines/tests the
  compiler-private primitive that injects that seam before outer
  `NewFactoryInstance`. Immutable Compiler Tasks 7/8 are the only concrete call
  sites, after effective merge/context resolution. The bound source occurrence
  is passed separately to the existing attempt-scoped hook and never supplies
  runtime scope, provenance, context, config, or binding enumeration. No such
  private CP5 primitive exists at planning time, so this plan does not name it
  as a landed API or claim X1 production injection.
- **Completeness scan:** no deferred or implement-later step. Dynamic commit
  identity is captured from the accepted branch rather than guessed.

Plan complete. Execute through the parent program's selected subagent-driven
workflow: X1-I serially, X1-W/X1-A in parallel from its exact commit, then X1-G
review and integration.
