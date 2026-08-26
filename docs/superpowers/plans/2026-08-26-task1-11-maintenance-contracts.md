# Task 1-11 Architecture Maintenance Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` to execute this plan task-by-task.

**Goal:** Preserve the implemented portions of Tasks 1-11 and their planned-only boundaries, status facts, and maintenance invariants in the canonical design record and directory-scoped agent instructions.

**Architecture:** Current source and tests at `master@fe8ab1ea` define implemented behavior. `docs/design.md` records the cross-package architecture, the root `AGENTS.md` owns repository-wide workflow and source-of-truth routing, and nested `AGENTS.md` files own only directory-specific invariants and focused verification. Historical plans remain implementation history and must not be treated as delivery evidence.

**Tech Stack:** Go 1.26, bbolt generation journal, immutable compiler/runtime snapshots, Markdown instruction files.

---

## Global constraints

- Preserve the four user-owned untracked review reports under `docs/reviews/`.
- Do not create a duplicate root `DESIGN.md`; `docs/design.md` remains canonical.
- Distinguish the implemented single-process generation engine from planned-only supervisor/worker and qualification subsystems.
- Do not hand-edit manifest-generated plugin registry or status projections.
- Keep generic setup, cache, testing, and delivery rules in the root `AGENTS.md`; nested files inherit them.

### Task 1: Establish the documentation source-of-truth ledger

- [x] Confirm the current revision, dirty state, existing instruction hierarchy, and canonical design path.
- [x] Verify Task 1-11 facts against current code/tests and identify planned-only packages that do not exist.
- [x] Assign canonical ownership: capability manifest for machine-readable parity facts, `docs/design.md` for architecture, root `AGENTS.md` for repository rules, nested `AGENTS.md` for local invariants.

### Task 2: Update the canonical design record

**Files:**
- Modify: `docs/design.md`

- [x] Add the implemented bootstrap, desired-to-published transaction, immutable compiler, generation lease, task ownership, panic, secret, and resource cleanup contracts.
- [x] Mark external supervisor/worker and next-generation qualification as planned-only.
- [x] Replace or clearly supersede stale mutable Store/route-builder and old secret-registry descriptions.

### Task 3: Update repository-wide agent instructions

**Files:**
- Modify: `AGENTS.md`

- [x] Correct stale configuration, listener, publication, journal, stream, and plugin-registration facts.
- [x] Add source-of-truth routing and cross-package invariants.
- [x] Route agents to nested instructions without duplicating local contracts in the root.

### Task 4: Add state, configuration, compiler, and runtime instructions

**Files:**
- Create: `pkg/capability/AGENTS.md`
- Create: `pkg/config/AGENTS.md`
- Create: `pkg/generation/AGENTS.md`
- Create: `pkg/store/AGENTS.md`
- Create: `pkg/compiler/AGENTS.md`
- Create: `pkg/runtime/AGENTS.md`
- Create: `pkg/secret/AGENTS.md`

- [x] Record each package's authority boundary, frozen invariants, change synchronization rules, and focused verification.
- [x] Keep generated artifacts, journal state, secret authority, and retryable cleanup contracts unambiguous.

### Task 5: Add serving, protocol, proxy, and plugin instructions

**Files:**
- Create: `pkg/server/AGENTS.md`
- Create: `pkg/route/AGENTS.md`
- Create: `pkg/stream/AGENTS.md`
- Create: `pkg/proxy/AGENTS.md`
- Create: `pkg/plugin/AGENTS.md`
- Create: `pkg/plugin/logger_batch/AGENTS.md`
- Create: `pkg/observability/metrics/AGENTS.md`

- [x] Record atomic HTTP/stream publication and lease rules.
- [x] Record request finalization and panic attribution rules.
- [x] Record stream connection, cluster-resource, plugin-task, and logger-batch ownership rules.
- [x] Record readiness, bounded-label, and overlapping-generation metric ownership rules.

### Task 6: Verify documentation and instruction convergence

- [x] Verify every documented path and focused test selector exists.
- [x] Verify the nested instruction hierarchy does not duplicate root setup/cache policy.
- [x] Verify planned-only packages are not described as implemented.
- [x] Run `git diff --check` and inspect the complete diff and status.
- [x] Confirm the four pre-existing review reports remain untouched.
