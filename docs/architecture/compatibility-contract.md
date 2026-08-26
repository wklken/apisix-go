# Compatibility contract

This document governs how apisix-go describes compatibility. The
[capability manifest](../../pkg/capability/manifest.yaml) is the machine-readable
specification and sole editable source for target, capability, behavior,
evidence, qualification, platform, gap, and divergence facts. The generated
[plugin capability status](../plugins.md) is a projection of that manifest, not
an independent source of truth.

## Namespaces

The `apisix` namespace is reserved for the pinned Apache APISIX contract. Its
entries are evaluated against the exact target recorded in the manifest, and
known gaps remain explicit `partial` or `deferred` behavior. Go-native
implementation mechanisms do not by themselves create a divergence.

The `apisix-go/v1` namespace contains versioned project extensions. Extensions
cannot fill an APISIX parity count, hide an APISIX gap, or silently move into
the `apisix` namespace. [ADR-0001](adr/0001-compatibility-governance.md)
records the approved boundary.

## Independent profile axes

The runtime selects three independent axes:

- `compatibility_target` selects the pinned observable APISIX contract;
- `security_profile` selects `compat` or the versioned `strict` controls;
- `qualification_profile` selects an evidence-backed operating contract, or is
  empty and makes no qualification claim.

No axis implies either of the others. In particular, strict security does not
assert qualification, and qualification does not change the compatibility
target. [ADR-0002](adr/0002-strict-security-profile.md) defines the intentional
security-profile divergence.

## Behavior and evidence

Behavior status and evidence maturity are separate dimensions. Behavior is
`full`, `partial`, `deferred`, or `not_applicable`. Evidence claims cover
schema, unit, converted upstream, differential, real dependency, failure, and
recovery, with states `verified`, `missing`, `stale`, `flaky`, `deferred`, or
`not_applicable`. `full` behavior is not qualification evidence.

A qualification profile fails closed unless every required plugin is in the
APISIX namespace, has `full` behavior, supports the selected domain, and every
required evidence dimension is `verified` or concretely `not_applicable`.
Missing, stale, flaky, or deferred evidence blocks selection.

## Target and corpus pins

Compatibility evidence is meaningful only for the manifest's exact target
name, version, source commit, and image. The converted-upstream corpus has its
own explicit source commit in
[`t/plugin/corpus_scope.yaml`](../../t/plugin/corpus_scope.yaml). If that corpus
pin differs from the compatibility target, its evidence is stale; it cannot be
silently relabeled or used as current qualification evidence.

## Divergence lifecycle and ownership

An intentional observable departure is proposed with the
[ADR template](adr/0000-template.md), exact divergence IDs, target, owner, and
approval reference. Only an `accepted` manifest divergence with a matching
accepted ADR owned by `wklken` is active. The manifest and ADR approval
references must match exactly. The generator rejects missing, mismatched,
symlinked, unknown-field, or unreferenced accepted ADRs before checking or
writing generated output. Retirement changes both records to `retired` and
supplies the evidence named by the ADR; deleting history is not retirement.

Owner review freezes the extension namespace and accepted divergence ledger.
Adding, renaming, promoting, or removing an extension, or changing an active
divergence, requires an explicit manifest/ADR change and owner approval. A
partial or deferred feature gap is not converted into a divergence to bypass
qualification.

## Platform and readiness boundary

[ADR-0003](adr/0003-platform-support.md) defines Linux production artifacts,
native-smoked macOS artifacts, and experimental Windows source builds without
a published Windows artifact. Artifact tier, registration, manifest row count,
and generated status count are inventory facts. None proves operational
qualification or production readiness. The repository remains under
development; production-readiness claims require the separately defined,
current, digest-bound release and operations evidence.
