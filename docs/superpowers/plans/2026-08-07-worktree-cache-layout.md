# Shared Go Cache With Isolated Worktree Outputs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Subagents are not authorized for this repository task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop duplicating Go toolchains, modules, build cache, and installed tools in every Git worktree while keeping temporary files, benchmark evidence, telemetry, and built application binaries worktree-local.

**Architecture:** `.envrc` derives one repository-wide shared cache from Git's absolute common directory and one local root from the active worktree. Content-addressed/download caches and installed development tools use the shared root; mutable task outputs stay below the active worktree's `.cache/`. Make targets write the server binary to the local cache and provide explicit inspection and local legacy-cache cleanup commands.

**Tech Stack:** Bash, GNU Make, Git worktrees, Go 1.26 environment variables.

## Global Constraints

- Preserve all tracked and untracked worktree source files.
- Do not delete pre-existing worktree caches during implementation; broad cleanup must be an explicit user action. Task-generated cache data may be removed by the target under test.
- Do not share `GOTMPDIR`, `TMPDIR`, `TEST_TELEMETRY_DIR`, benchmark data, coverage data, or built application binaries.
- Do not add dependencies or change Go source behavior.
- Do not commit, push, or create a PR without separate authorization.

---

### Task 1: Lock and implement the cache boundary

**Files:**
- Create: `scripts/cache_layout_test.sh`
- Modify: `.envrc`

**Interfaces:**
- Consumes: `git rev-parse --show-toplevel` and `git rev-parse --path-format=absolute --git-common-dir`.
- Produces: exported `APISIX_GO_ROOT`, `APISIX_GO_SHARED_CACHE`, `GOPATH`, `GOBIN`, `GOCACHE`, `GOMODCACHE`, `GOLANGCI_LINT_CACHE`, `GOTMPDIR`, `TMPDIR`, and `TEST_TELEMETRY_DIR`.

- [x] **Step 1: Write the failing layout test**

  Create a temporary Git repository plus linked worktree, source the repository `.envrc` in both, and assert that shared-cache variables are equal while local-root, temp, and telemetry variables differ.

- [x] **Step 2: Verify the old layout fails**

  Run: `bash scripts/cache_layout_test.sh`

  Expected: FAIL because the current `.envrc` places `GOMODCACHE`, `GOCACHE`, and `GOBIN` under each active worktree.

- [x] **Step 3: Implement the split layout**

  Resolve the active worktree from `--show-toplevel`, resolve the main checkout from `--git-common-dir`, place shareable Go/linter state under `<main-checkout>/.cache/shared`, and leave temp/telemetry under `<active-worktree>/.cache`.

- [x] **Step 4: Verify the layout test passes**

  Run: `bash scripts/cache_layout_test.sh`

  Expected: `cache layout test: PASS`.

### Task 2: Keep build products local and add lifecycle targets

**Files:**
- Modify: `Makefile`

**Interfaces:**
- Consumes: `GOBIN` and `APISIX_GO_SHARED_CACHE` exported by `.envrc`.
- Produces: `.cache/out/apisix`, `make cache-status`, `make cache-layout-test`, `make clean`, and `make cache-clean-local`.

- [x] **Step 1: Move Make-managed application builds**

  Change `make build`, `make serve`, and `make live` to use `.cache/out/apisix`; keep `BINARY_PATH` overridable.

- [x] **Step 2: Share installed benchmark tools**

  Resolve `BENCHSTAT` from the sourced shared `GOBIN`, with `.cache/bin` only as a fallback when `.envrc` was not sourced.

- [x] **Step 3: Add explicit inspection and cleanup targets**

  `cache-status` reports resolved cache paths and sizes. `clean` removes only Make-managed and legacy root binaries. `cache-clean-local` removes only the active worktree's local temp/output and pre-migration duplicated Go/linter directories, preserving `.cache/shared`, benchmarks, coverage, fixtures, and source data.

- [x] **Step 4: Verify Make wiring**

  Run: `bash -lc 'source .envrc && make cache-layout-test cache-status && make build && test -x .cache/out/apisix && make clean && test ! -e .cache/out/apisix'`

  Expected: every command succeeds; no root `./apisix` remains.

### Task 3: Synchronize operator documentation

**Files:**
- Modify: `AGENTS.md`
- Modify: `README.md`

**Interfaces:**
- Consumes: the cache contract implemented by `.envrc` and Make targets.
- Produces: one consistent human/agent workflow for new worktrees and migration cleanup.

- [x] **Step 1: Document shared versus isolated paths**

  Explain that Go toolchains/modules/build cache/tools are shared at the main checkout's `.cache/shared`, while temp, telemetry, benchmark evidence, coverage, and application output remain local.

- [x] **Step 2: Document safe cleanup**

  Replace blanket `.cache/` deletion guidance with `make cache-status`, `make clean`, and per-worktree `make cache-clean-local`; require all agents to stop before manually clearing shared Go caches.

- [x] **Step 3: Update build instructions**

  Document `.cache/out/apisix` and remove instructions that assume `make build` leaves `./apisix` behind.

### Task 4: Completion gates

**Files:**
- Verify only: `.envrc`, `Makefile`, `scripts/cache_layout_test.sh`, `AGENTS.md`, `README.md`

**Interfaces:**
- Consumes: completed Tasks 1-3.
- Produces: fresh verification evidence for the final handoff.

- [x] **Step 1: Check shell syntax and focused behavior**

  Run: `bash -n .envrc scripts/cache_layout_test.sh && bash scripts/cache_layout_test.sh && git diff --check`.

- [ ] **Step 2: Run repository gates with the new environment**

  Run: `bash -lc 'source .envrc && go test ./... -count=1 -p=1 && make lint && make build && make clean'`.

  Expected: all gates pass. If a pre-existing or integration failure occurs, report the exact command and failure without calling the full gate successful.

  Observed: the pre-change full gate failed in `t/plugin`; post-change `make lint` failed on two unchanged benchmark files. Focused cache-layout, build, and `make test` gates passed and are reported separately.

- [x] **Step 3: Inspect final scope**

  Run: `git status --short -- .envrc Makefile scripts/cache_layout_test.sh AGENTS.md README.md docs/superpowers/plans/2026-08-07-worktree-cache-layout.md` and `git diff --` for the same paths.

  Expected: only requested cache/build/documentation changes plus this implementation plan.
