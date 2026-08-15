# Request Data Contract Implementation Plan

> **Execution:** implement this plan task by task with regression-first tests. Plan 20 is one independent PR based on `origin/master@e0faff461e816f4b0c16822f2423a8b6ce040227`.

**Goal:** Close PR-020 and PR-021: `data-mask` changes only the detached logging view and never the request sent upstream; configured `request-validation.body_schema` rejects a missing or whitespace-only body before schema decoding.

**Architecture:** Extend the Plan 17 log executor with one exact pre-callback sanitizer capability. The executor builds its canonical detached snapshot, runs all materialized sanitizers in scope/priority order, then gives each logger/finalizer a private policy-bounded clone of the sanitized snapshot. `data-mask` owns that capability and no request phase. `request-validation` remains an access-stage request gate and explicitly distinguishes missing bytes from JSON `null`.

**No new dependency.** Use Go 1.26, the existing detached `base.LogSnapshot`, current JSON/form helpers, and the existing capability/metadata registries.

## Frozen contracts

- The live `*http.Request` passed to downstream/upstream is never changed by `data-mask`: method, `RequestURI`, `URL.RawQuery`, query ordering/encoding, headers, body bytes, `ContentLength`, and body replay remain intact.
- Add `base.LogSnapshotSanitizerPlugin` with `SanitizeLogSnapshot(*LogSnapshot) error`. It receives only a detached snapshot; it must not retain the pointer.
- Add `CapabilityLogSanitizer`. Only exact factory key `data-mask` owns it in Plan 20. It is not `CapabilityLog`, `CapabilityRequestRewrite`, or `CapabilityConditionalTerminal`.
- `requestStageRegistry["data-mask"]` is `RequestStageNone`; its compatibility `Handler` only calls `next`.
- `NewLogExecutorFromBindings` materializes sanitizer bindings even when no logger is present. Capture maxima include sanitizer policies.
- `LogExecutor.runComposite` sorts once, runs sanitizers first, then log callbacks, then snapshot finalizers. Every logger/finalizer still receives a private clone. A sanitizer error stops later snapshot consumers, so raw marked fields cannot fall through.
- Query masking updates the detached `Query`, `URI`, and `URL`; header masking updates only the detached header clone; body masking updates only detached body bytes.
- When a configured body rule cannot safely produce a complete masked body (capture truncated, configured `max_body_size` exceeded, malformed JSON/form), the sanitizer clears the detached body before returning an error. No logger receives a raw fallback.
- A body sanitizer requests `min(max_body_size, base.MAX_REQ_BODY)` bytes. Query/header-only configurations request zero body bytes.
- Metadata wrappers preserve the sanitizer method set and expose a read-only sanitizer selector. The executor evaluates every selector against the same detached pre-sanitized snapshot before running any sanitizer; a false filter leaves the snapshot unchanged and a true filter invokes the sanitizer exactly once.
- With `body_schema`, `len(bytes.TrimSpace(body)) == 0` is rejected with configured `rejected_code`/`rejected_msg` before content-type parsing or schema evaluation.
- JSON literal `null` is not missing. It is decoded and accepted/rejected by the configured schema. Existing downstream body replay/JSON normalization remains unchanged for accepted bodies.

## Task 1: Freeze regression tests before production edits

- [x] Add core executor tests proving sanitizer-before-logger/finalizer order, private clone isolation, policy-driven body capture, and fail-closed sanitizer errors.
- [x] Add exact registry tests proving `data-mask` is Plan 20 `log_sanitizer`, `RequestStageNone`, not request rewrite/terminal/log.
- [x] Add metadata-wrapper tests proving `_meta.filter` preserves the sanitizer interface and exact invocation behavior.
- [x] Add `data-mask` tests proving upstream request/query/header/body identity and detached query/header/JSON/form masking.
- [x] Add `data-mask` tests proving truncated/oversized/malformed body never reaches a logger as raw bytes.
- [x] Add `request-validation` matrix tests for nil/zero/whitespace body, JSON `null`, `{}`, valid/invalid form, custom rejection code/message, and downstream replay.

Required red gate before production changes:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/base ./pkg/plugin ./pkg/plugin/data_mask ./pkg/plugin/request_validation ./pkg/route -run "(LogSnapshotSanitizer|DataMaskLogSnapshot|DataMaskRoute|BodySchemaRequiresBody|RequestValidationBodyMatrix|MetadataLogSanitizer)" -count=1'
```

## Task 2: Add the log-sanitizer owner

Files:

- `pkg/plugin/base/log_phase.go`
- `pkg/plugin/log_executor.go`
- `pkg/plugin/log_executor_test.go`
- `pkg/plugin/capability_registry.go`
- `pkg/plugin/capability_registry_test.go`
- `pkg/plugin/request_stage_registry.go`
- `pkg/plugin/request_stage_registry_test.go`

- [x] Add the bounded plugin-facing sanitizer interface.
- [x] Materialize the exact capability and capture policy from ordinary/global/consumer bindings.
- [x] Apply sanitizers to the canonical detached snapshot before cloning for any log/finalizer callback.
- [x] Remove `data-mask` from rewrite/conditional-terminal sets and assign it to Plan 20.
- [x] Keep registry completeness at 115 factory keys / 114 identities.

## Task 3: Move data-mask off the live request

Files:

- `pkg/plugin/data_mask/plugin.go`
- `pkg/plugin/data_mask/plugin_test.go`

- [x] Make `Handler` a no-op compatibility adapter.
- [x] Implement `LogCapturePolicy` and `SanitizeLogSnapshot` using the existing rule compiler/masking helpers.
- [x] Keep query/header/body mutations confined to snapshot-owned maps/slices.
- [x] Update detached URI/URL consistently after query masking without touching upstream encoding/order.
- [x] Fail closed for incomplete or invalid masked body data; never retain raw body on the error path.
- [x] Delete request-mutation-only helpers/tests or adapt them to the detached contract; do not keep proxy-only dead seams.

## Task 4: Preserve metadata and route integration

Files:

- `pkg/route/builder.go`
- `pkg/route/log_phase_test.go`
- `pkg/route/plugin_parity_test.go`

- [x] Add an owner-specific metadata sanitizer wrapper without giving it log/finalizer/response interfaces.
- [x] Replace route parity assertions that expect masked upstream traffic with exact original-request assertions.
- [x] Add a real route closure with `data-mask` plus a logger proving upstream sees originals and the detached logger sees only masked query/header/body.
- [x] Cover JSON numeric formatting, form/query ordering, repeated query values, and header/body masking.

## Task 5: Tighten request-validation missing-body semantics

Files:

- `pkg/plugin/request_validation/plugin.go`
- `pkg/plugin/request_validation/plugin_test.go`

- [x] Reject empty/whitespace bytes immediately after the safe body read and before `parseRequestBody`.
- [x] Preserve configured rejection code/message.
- [x] Preserve JSON `null` schema semantics and accepted-body replay/normalization.
- [x] Remove the duplicate empty-body branch in `parseRequestBody` once the caller owns the missing-body boundary.

## Task 6: Align manifests and documentation

Files:

- `docs/superpowers/plans/2026-08-10-plugin-capability-manifest.md`
- `docs/plugins.md`
- `t/plugin/data-mask.yaml`
- `t/plugin/request-validation.yaml`

- [x] Move `data-mask` from Plan 13 request rewrite to Plan 20 log sanitizer in the canonical manifest and capability vocabulary.
- [x] Document upstream-preserving/log-only masking and missing-body validation.
- [x] Change data-mask integration expectations so upstream receives original query/header/body; retain an access-log case that proves masked output contains no raw secret.
- [x] Add an exact request-validation integration step for missing/whitespace body rejection and JSON `null` schema evaluation.

## Task 7: Verification and refactor audit

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/base ./pkg/plugin ./pkg/plugin/data_mask ./pkg/plugin/request_validation ./pkg/route -run "(LogSnapshotSanitizer|DataMask|BodySchema|RequestValidation|MetadataLogSanitizer|Capability|RequestStage)" -count=1'
bash -lc 'source .envrc && go test ./pkg/plugin/base ./pkg/plugin ./pkg/plugin/data_mask ./pkg/plugin/request_validation ./pkg/route -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin ./pkg/plugin/data_mask ./pkg/plugin/request_validation ./pkg/route -run "(LogSnapshotSanitizer|DataMask|BodySchema|RequestValidation|MetadataLogSanitizer)" -count=3 -timeout=5m'
bash -lc 'source .envrc && go test ./t/plugin -run "^TestPluginIntegration/data-mask/" -count=1 -timeout=10m'
bash -lc 'source .envrc && go test ./t/plugin -run "^TestPluginIntegration/request-validation/body-schema-requires-bytes-and-accepts-present-values$" -count=1 -timeout=10m'
bash -lc 'source .envrc && golangci-lint run ./pkg/plugin/base/... ./pkg/plugin/... ./pkg/route/...'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

- [x] Inspect the complete diff and exact changed-path inventory.
- [x] Search production/tests for deleted request-masking owners and stale Plan 13 `data-mask` classification.
- [x] Confirm no proxy-only helper remains solely for old tests.
- [x] Run independent merge-level code review and remediate every introduced High/Medium finding.
- [x] Record commands not run and residual risks honestly.

## Completion criteria

- [x] Upstream identity and sanitized logger output are proven in the same route execution.
- [x] No configured masked field can fall back to raw detached data.
- [x] Missing/whitespace body rejects before schema; JSON `null` follows schema.
- [x] Capability, request-stage, metadata, docs, and integration manifests agree.
- [x] Impact-scoped normal/race/lint/build/integration gates pass.
- [x] Independent review is APPROVE with no introduced High/Medium finding.

## Verification evidence

- Impact-scoped normal tests, focused race tests (`-count=3`), scoped `golangci-lint`, and `make build && make clean` passed on the final production/test diff.
- The complete `data-mask` real-process manifest and the exact new `request-validation/body-schema-requires-bytes-and-accepts-present-values` case passed.
- The full `request-validation` manifest was not used as a green gate because its existing configuration-rejection cases intentionally exit before readiness and the integration runner reports those cases as process failures.
- Independent review found one global/route metadata-filter ordering blocker. A regression reproduced it before the fix; selector preselection against one pre-sanitized snapshot closed it, and the focused re-review returned APPROVE.
- `go test ./...`, `make test`, and unrelated `t/plugin` manifests were not run; repository verification is impact-scoped.
