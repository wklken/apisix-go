# Immutable Task 6 CP2.1 Secret Descriptor Correction Plan

> **Execution rule:** land this shared-contract correction on
> `codex/apisix-go-immutable-task6` before S1/S2/A1/M1 leaf implementation
> worktrees branch. It is a serial owner checkpoint. Push and PR publication are
> outside current authority.

**Goal:** complete the CP2 redaction contract with one immutable descriptor that
can identify a materialized secret by declaration source and digest without
accepting or retaining raw input, references, ciphertext or plaintext.

**Baseline:** `40c04a26` (`feat(compiler): register refined generation attempts`).

**Why this correction is required:** CP2 already exposes `secret.Value.Digest`
and scoped materialization, but the parent C6.4 plan also requires a shared
`SecretDescriptor`. No such type exists at the baseline. Leaf packages must not
invent private descriptor formats.

## Ownership and API

**Single owner files:**

- Create: `pkg/secret/descriptor.go`
- Create: `pkg/secret/descriptor_test.go`
- Modify: `pkg/secret/materializer.go` only to add `Value.Descriptor`
- Modify focused docs/plan checkboxes only after verification
- Do not modify plugin leaf packages in this checkpoint.

The low-level `pkg/secret` package owns the type so compiler, metadata,
consumer and plugin preparation can share it without a `plugin/base` reverse
dependency.

```go
type Descriptor struct {
    source capability.SecretDeclarationSource
    digest [32]byte
}

func NewDescriptor(
    capability.SecretDeclarationSource,
    [32]byte,
) (Descriptor, error)

func (value Value) Descriptor(
    capability.SecretDeclarationSource,
) (Descriptor, error)

func (descriptor Descriptor) Source() capability.SecretDeclarationSource
func (descriptor Descriptor) Digest() [32]byte
func (descriptor Descriptor) String() string
```

`NewDescriptor` accepts only an already-computed digest, never bytes or text.
`Value.Descriptor` uses the value's existing internal digest. Both reject an
unknown source and a zero digest. The three legal sources are
`plugin_config`, `plugin_metadata`, and `consumer_config`.

`String` is deterministic and fixed to:

```text
<source>#sha256:<64-lowercase-hex>
```

The struct has no exported fields. It exposes no JSON/YAML representation that
could later grow a raw field, and it does not implement custom error wrapping.

## Step 1 — Write failing redaction and shape tests

Add tests proving:

- all three legal declaration sources succeed;
- unknown/empty sources and zero digests fail with fixed errors that do not
  echo caller input;
- `Value.Descriptor` returns the exact `Value.Digest`;
- `String` is stable lowercase source plus SHA-256 hex;
- reflection finds no exported fields;
- constructor signatures accept neither `string` nor `[]byte` raw input;
- a sensitive sample containing an environment name, Vault path, ciphertext
  and plaintext never appears in descriptor formatting or errors;
- returned digest arrays are values and cannot mutate descriptor state.

Run RED:

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/secret -run '^TestDescriptor' -count=1
```

Expected: compilation fails because `secret.Descriptor` does not exist.

## Step 2 — Implement the minimum immutable value

Implement only the API above. Reuse the capability source constants; do not
copy their string set into plugin packages. Use fixed package errors and
`encoding/hex` for formatting. Do not add raw-input constructors, mutable
setters, logging helpers, global registries or serialization tags.

Run GREEN:

```bash
source .envrc
export GOFLAGS=-mod=readonly
go test ./pkg/secret -run '^(TestDescriptor|TestValue)' -count=1
go test -race ./pkg/secret -run '^(TestDescriptor|TestValue)' -count=1
golangci-lint run --new-from-rev=HEAD ./pkg/secret/...
git diff --check
make build
```

## Step 3 — Freeze the leaf handoff

Add compile-time or focused tests in each subsequent lane only when it actually
stores a descriptor. Leaves obtain descriptors from the `secret.Value` returned
by their scoped access. Legacy compatibility methods may compute a digest and
call `NewDescriptor`, but they must never pass raw text into a shared helper or
format it themselves.

No leaf may redefine `Descriptor`, keep a parallel source/digest struct, expose
the digest as a credential, or claim the digest permits plaintext recovery.

**Checkpoint commit:** `feat(secret): add redacted value descriptors`

## Acceptance

| Check | Required evidence |
| --- | --- |
| Ownership | exactly one descriptor type in `pkg/secret` |
| Input | constructors accept source plus digest or `Value`, never raw text/bytes |
| State | private source and digest fields only |
| Output | fixed source and lowercase SHA-256; no caller text |
| Compatibility | all three catalog sources accepted |
| Gates | focused tests/race, new-diff lint, build and diff check pass |

## Non-goals

- No secret resolution or catalog lookup changes.
- No plugin leaf migration in this checkpoint.
- No attempt to erase Go strings or reverse a digest.
- No generic observability/log schema work.
