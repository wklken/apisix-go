# Configuration, Container, and Release Gates Implementation Plan

> Execution owner: the primary agent using `superpowers:executing-plans` in
> the isolated `codex/prod-ready-config-container-release-gates` worktree. No
> implementation delegation is authorized for this plan.

**Goal:** Close PR-024 and P1 5.6 with deterministic default-plus-override
configuration, a non-root runnable image, and repeatable security/release
evidence.

**Pinned base:** `origin/master@503d9737f2978fdc82a360edfcc76f6e49ec182d`.

## Current gaps

- `cmd/root.go` selects one Viper file and retains a merge FIXME. Local default
  execution reads `conf/config-default.yaml`, while the image explicitly reads
  the incomplete `conf/config.yaml` override.
- `pkg/config.Load` publishes `GlobalConfig` before all current validation and
  does not require a plugin allowlist, an explicit valid listener, positive
  proxy limits, or etcd endpoints/prefix for the etcd provider.
- The runtime image is Alpine 3.19, runs as root, lacks CA installation and a
  healthcheck, and has no executable container smoke.
- CI has lint/build/unit/integration/package jobs, but no focused race,
  `govulncheck`, SBOM, image scan, non-root smoke, or rollback metadata gate.

## Frozen decisions

- Load order is `conf/config-default.yaml`, selected `-c` override, then
  `APISIXGO_*` environment values. The config flag selects the override; there
  are no independent configuration-value CLI flags today.
- Keep the existing `-c conf/config-default.yaml` CLI default for compatibility.
  Passing that same path performs one base read; the image continues to pass
  `-c /usr/local/apisix/conf/config.yaml`, now as a true override.
- Lists replace the base list; nested maps merge. Empty replacement lists remain
  visible to strict validation and fail closed where required.
- Runtime validation occurs before `GlobalConfig` publication and before data
  encryption configuration. It requires: at least one HTTP plugin, at least one
  valid explicit HTTP listener, all four proxy resource limits greater than
  zero, and non-empty etcd host/prefix when the selected provider is `etcd`.
- `CapabilitySummary` exposes only booleans, counts, role/provider, and listener
  modes. It never exposes addresses, credentials, certificates, keyrings,
  tokens, or secret references.
- Alpine 3.24.1 is the current supported stable release. Use it explicitly for
  the runtime, create UID/GID 10001, and install `ca-certificates` plus `curl`
  for TLS and the image healthcheck.
- The container smoke creates an isolated Docker network, a tiny upstream
  container, and a standalone APISIX route. It must prove healthy startup, one
  proxied response, UID 10001, TERM handling, and process exit 0. It owns a
  concurrency lock so real-process invocations serialize.
- Docker is absent on the local execution host. Syntax/static shell checks run
  locally; the real container command is reported BLOCKED locally and must run
  in CI.
- Release evidence consists of CycloneDX JSON SBOM, Trivy SARIF/JSON scan, and
  `rollback-metadata.json` containing source ref/commit, immutable image ID,
  image tag, and artifact checksums. These are uploaded artifacts, not a new
  deployment or rollback protocol.
- No production dependency changes are expected.

## Task 1: Deterministic configuration and safe summary

- [ ] Add regression-first tests for nested map merge, list replacement,
  environment precedence, missing etcd hosts/prefix, empty plugins, invalid or
  empty listeners, non-positive proxy limits, pre-publication failure, and
  summary redaction.
- [ ] Implement `Load(overridePath)` using a new Viper instance: read base,
  optionally `MergeInConfig` the distinct override, enable `APISIXGO_*`, then
  decode and validate once.
- [ ] Move all publication side effects after validation.
- [ ] Add `CapabilitySummary(*Config) map[string]any` in `pkg/config`; make the
  command log that canonical summary rather than a second implementation.
- [ ] Run focused and full `pkg/config`/`cmd` tests and scoped lint.

## Task 2: Non-root runnable image and smoke

- [ ] Add a Dockerfile contract test before editing the Dockerfile.
- [ ] Pin Alpine 3.24.1, install CA/curl, create and select UID/GID 10001, own
  runtime config/log/data paths, and add an HTTP listener healthcheck.
- [ ] Add `scripts/container_smoke.sh` with bounded waits, exact cleanup, an
  isolated network, standalone configuration, real upstream proxying, UID
  assertion, TERM, and exit-code assertion.
- [ ] Add a shell contract test covering fail-closed missing prerequisites,
  source syntax, and cleanup/serialization hooks without pretending to replace
  the real Docker run.

## Task 3: Release/security gates and documentation

- [ ] Add a dedicated workflow for focused race packages, pinned
  `govulncheck`, serialized container smoke, CycloneDX SBOM, Trivy image scan,
  and uploaded rollback metadata.
- [ ] Keep third-party actions/tools on explicit version tags consistent with
  the repository's existing action policy; do not use floating `latest`.
- [ ] Add a deterministic metadata script/test rather than embedding opaque
  shell in YAML.
- [ ] Document local and CI commands, effective-config precedence, redacted
  summary fields, image ownership/health behaviour, evidence artifacts, and
  rollback use in `docs/design.md`.
- [ ] Run workflow/shell syntax checks, impact-scoped Go tests/race/lint,
  `govulncheck`, build, diff check, and the real container smoke when Docker is
  available.
- [ ] Obtain an independent read-only final review and remediate every
  introduced High/Medium finding before delivery.

## Verification commands

```bash
bash -lc 'source .envrc && go test ./pkg/config ./cmd -count=1'
bash -lc 'source .envrc && go test -race ./pkg/config ./cmd ./pkg/server ./pkg/route ./pkg/proxy ./pkg/store ./pkg/etcd -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/config/... ./cmd/...'
bash -lc 'source .envrc && go run golang.org/x/vuln/cmd/govulncheck@v1.7.0 ./...'
bash -lc 'source .envrc && make build && make clean'
bash scripts/container_smoke_test.sh
bash scripts/release_metadata_test.sh
bash scripts/container_smoke.sh
git diff --check
```
