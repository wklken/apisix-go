# OAS and Secret Resolution Boundaries Implementation Plan

> **Execution:** Follow `superpowers:executing-plans` in one isolated worktree. Do not use subagents for this plan.

**Goal:** Close P1 5.9 and P1 5.18 with bounded OAS retrieval and one explicit pre-`PostInit` secret-materialization phase.

**Current base:** Plan 07 owns acknowledged store apply and Plan 10 owns transactional route publication. Existing consumer secrets and a few plugins call `store.ResolveSecretReference` directly; this plan preserves those compatibility paths while adding the shared phase and migrating the three audited gaps.

## Frozen invariants

- Secret values are held in clone-on-read, destroyable byte handles. Plugin config retained after initialization contains only a reference plus fingerprint descriptor, never resolved plaintext.
- Every plugin factory path invokes secret materialization after schema decode and before `PostInit`. A materialization failure aborts the new route generation; the reload scheduler preserves the last-good handler and readiness failure.
- A `secrets` store event invalidates the Vault value cache and triggers HTTP route rebuild. A replacement generation resolves new values before publication; retiring instances destroy their handles.
- OAS root plus external documents consume at most 4 MiB total, 64 external-reference occurrences, and depth 16. Cycles fail closed.
- OAS fetch permits only HTTP(S), follows at most three redirects, and rejects loopback, private, link-local, multicast, unspecified, CGNAT, and known metadata addresses after DNS resolution and at each redirect unless an exact hostname/IP/CIDR allowlist entry permits them.
- Configured request headers are sent only to the original `spec_url` origin. Cross-origin refs and redirects never receive them.
- The existing OAS last-good background refresh remains: refresh failures do not replace a valid compiled document and shutdown still joins the worker.

## Task 1: Add the secret lifecycle owner

- [ ] Add `store.ResolvedSecret` with cloned `Bytes`, reference, version/fingerprint descriptor, and idempotent zeroing `Destroy`.
- [ ] Add a `plugin.SecretMaterializer` hook and invoke it at every production plugin initialization site before `PostInit`.
- [ ] Treat `secrets` as an HTTP reload bucket, expose one canonical event-bucket parser, and invalidate Vault cache on secret updates.
- [ ] Add regression-first tests for clone/destroy/redaction, missing references, pre-`PostInit` order, cache invalidation, reload classification, and last-good route replacement.

## Task 2: Migrate the audited plugin gaps

- [ ] Materialize Aliyun access key ID/secret and both AI RAG API keys into private handles; use short-lived clones for signing/headers and destroy them on retirement.
- [ ] Materialize inline OAS `spec` and configured spec request-header values. Preserve references/fingerprints in effective plugin config.
- [ ] Add tests proving literal and `$ENV://`/`$secret://` values work, failures expose no plaintext, and rotation creates a distinct generation without mutating the old instance.

## Task 3: Bound the OAS document graph and network

- [ ] Preload the root/external document graph with total-byte, reference-count, depth, and cycle accounting; compile kin-openapi only from the bounded in-memory graph.
- [ ] Add a DNS-pinning dialer and redirect policy. Validate every resolved address and redirect destination; support an explicit `spec_url_allowed_addresses` hostname/IP/CIDR list.
- [ ] Add focused tests for exact/oversized total bytes, 64/65 refs, depth 16/17, cycles, private/metadata addresses, redirect count, cross-origin header stripping, and allowlisted same-origin fetch.

## Task 4: Verification, review, and delivery

```bash
bash -lc 'source .envrc && go test ./pkg/store ./pkg/plugin ./pkg/route ./pkg/server ./pkg/plugin/oas_validator ./pkg/plugin/ai_aliyun_content_moderation ./pkg/plugin/ai_rag -count=1'
bash -lc 'source .envrc && go test -race ./pkg/store ./pkg/route ./pkg/server ./pkg/plugin/oas_validator ./pkg/plugin/ai_aliyun_content_moderation ./pkg/plugin/ai_rag -run "(Secret|Materializ|Rotation|OAS|Spec|Redirect|Address|Header|Reload)" -count=3'
bash -lc 'source .envrc && golangci-lint run ./pkg/store/... ./pkg/plugin/... ./pkg/route/... ./pkg/server/...'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

- [ ] Review the frozen diff, repair confirmed findings regression-first, then commit, push, open a PR, wait for required CI, squash-merge to `master`, and verify the remote merge.

**Commit:** `fix(config): bound OAS and resolve plugin secrets`
