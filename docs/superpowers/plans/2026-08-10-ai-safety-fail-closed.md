# AI Safety Fail-Closed Implementation Plan

> **For agentic workers:** Execute only the bounded work unit in the implementation brief. Use regression-first implementation and do not commit, push, create a PR, or delegate recursively.

**Goal:** Close PR-007 by giving AI safety plugins an explicit fail-mode contract whose production default rejects uninspectable requests and operational moderation failures, without claiming that already-committed realtime streams can be converted to HTTP errors.

**Architecture:** Add pure fail-mode parsing/decision types in `ai_common`; request and buffered-response owners apply the decision in their own control flow. Add a bounded Prometheus counter through the existing metrics lifecycle. Prompt guard and Aliyun moderation share the contract, but keep protocol-aware response construction local. Error-mode realtime response moderation is rejected at configuration time; Plan 19 owns any future buffered-window/terminal-event implementation.

**Validated base:** `origin/master` at `ef771b90f55d0ad41479142e9a41557b8fbe6e78`.

## Global Constraints

- Default fail mode is `error`; `warn` and `skip` require explicit configuration.
- The shared decision helper is pure. It never writes an HTTP response or calls `next`.
- Request invalid JSON, unknown protocol/Passthrough, and recognized requests with no inspectable content map to 400 in error mode and never call upstream.
- Moderation transport errors, timeouts, non-2xx responses, malformed JSON, missing risk fields, and unknown risk levels map to 503 in error mode.
- An invalid buffered upstream AI response maps to 502, because the client is not responsible for the upstream representation.
- Policy matches continue to use plugin-specific deny code/message and are not operational failures.
- `warn` and `skip` have the same data-plane behavior (continue or pass through) but different log levels; both record `degraded`.
- Valid syntax with no text, such as a pure tool-call response, is not automatically an invalid upstream response.
- `fail_mode=error`, `check_response=true`, and `stream_check_mode=realtime` is rejected by `PostInit`; do not pretend a committed stream can become HTTP 503.
- Logs include only plugin, phase, fixed reason, route ID, and outcome. Never log prompt/response bodies, credentials, backend raw bodies, or signing material.
- Metrics labels are fixed and bounded: `plugin`, `phase`, `outcome`, `reason`. Route ID is never a metric label.
- Metrics recording before `metrics.Init()` is a no-op, never a panic.
- Run all Go commands with `bash -lc 'source .envrc && ...'`.

## Files

- Create `pkg/plugin/ai_common/safety.go` and `safety_test.go`.
- Create `pkg/observability/metrics/ai_safety.go` and `ai_safety_test.go`.
- Modify `pkg/observability/metrics/prometheus.go` and, only if required, `prometheus_test.go`.
- Modify `pkg/plugin/ai_prompt_guard/plugin.go` and `plugin_test.go`.
- Modify `pkg/plugin/ai_aliyun_content_moderation/plugin.go` and `plugin_test.go`.

## Task 1: Define the pure safety decision contract

**Owner files:** `pkg/plugin/ai_common/safety.go`, `safety_test.go`.

- [ ] Add `SafetyFailMode` constants `error`, `warn`, `skip`; empty parses as `error`, invalid values return an error.
- [ ] Add fixed failure classes: `invalid_payload`, `unknown_protocol`, `empty_content`, `backend_unavailable`, `backend_invalid_response`, `upstream_invalid_response`.
- [ ] Add fixed phases `request`, `response`; outcomes `allow`, `deny`, `degraded`, `error`; and actions `reject`, `continue`.
- [ ] Add a pure `SafetyDecision{Action, Status, Outcome}` and `DecideSafetyFailure(mode, class)`:
  - warn/skip -> continue, status 0, degraded;
  - error + request-content class -> reject 400/error;
  - error + backend class -> reject 503/error;
  - error + invalid upstream response -> reject 502/error.
- [ ] Add a bounded degradation logger that accepts only fixed enums and emits no content/body values.
- [ ] Run:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/ai_common -run "^(TestParseSafetyFailMode|TestDecideSafetyFailure|TestLogSafetyDegradation)$" -count=1'
```

## Task 2: Add bounded metrics through the existing lifecycle

**Owner files:** `pkg/observability/metrics/ai_safety.go`, `ai_safety_test.go`, `prometheus.go`, optional `prometheus_test.go`.

- [ ] Declare/create/register `AISafetyOutcomes` in the same `metrics.Init()` lifecycle and prefix handling as existing vectors. Default exposed name is `apisix_ai_safety_outcomes_total`.
- [ ] Add `RecordAISafetyOutcome(plugin, phase, outcome, reason string)` that is a no-op before init and rejects unknown label values rather than creating unbounded series.
- [ ] Valid plugins are only `ai-prompt-guard` and `ai-aliyun-content-moderation`; valid phases/outcomes/reasons are the fixed contract values, including `clean`, `allow_pattern_miss`, `deny_pattern_match`, and `risk_threshold`.
- [ ] Follow the private-registry observer pattern used by `proxy_runtime.go` so tests do not pollute the default registry.
- [ ] Do not add AI safety to `HTTPRequestMetricsEnabled()` and do not add route ID/body values as labels.
- [ ] Run:

```bash
bash -lc 'source .envrc && go test ./pkg/observability/metrics -run "^Test(AISafety|RecordAISafety)" -count=1'
```

## Task 3: Make prompt guard default fail closed

**Owner files:** `pkg/plugin/ai_prompt_guard/plugin.go`, `plugin_test.go`.

- [ ] Change schema and `PostInit` default to `error`; parse through `ai_common.ParseSafetyFailMode` and store the typed mode internally.
- [ ] Preserve empty/read-failed request body as unconditional 400.
- [ ] Route invalid JSON, unknown/Passthrough protocol, and zero inspectable messages after configured role/history filtering through the shared decision.
- [ ] In error mode, write a stable public 400 message and never call `next`. In warn/skip, log only bounded fields, record degraded, and call `next` exactly once with the restored body.
- [ ] Record `allow/clean` for clean inspected content, and bounded deny reasons for allow-pattern misses and deny-pattern matches.
- [ ] Update obsolete default-skip/passthrough tests; add an exact table for invalid JSON, unknown protocol, empty extracted messages, explicit warn, and explicit skip.
- [ ] Run:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/ai_prompt_guard -run "^(TestPostInitDefaultsFailModeToError|TestHandlerAppliesFailModeToUninspectableRequest|TestHandlerRecordsPromptGuardAllowAndDenyOutcomes)$" -count=1'
bash -lc 'source .envrc && go test ./pkg/plugin/ai_prompt_guard -count=1'
```

## Task 4: Make Aliyun moderation propagate operational failures

**Owner files:** `pkg/plugin/ai_aliyun_content_moderation/plugin.go`, `plugin_test.go`.

- [ ] Add `fail_mode` to schema/config, default `error`, parse through the shared contract.
- [ ] Reject error-mode realtime response checking in `PostInit`; explicit warn/skip may retain current realtime behavior and record degradation when an operational failure occurs.
- [ ] Replace `(code, message, riskLevel)` with a structured moderation result plus a classified error. Do not translate backend errors into clean content.
- [ ] Treat unknown risk-level strings as backend-invalid responses; do not let `riskLevelToInt == -1` become clean.
- [ ] At request call sites, apply 400 for extraction failures and 503 for moderation failures in error mode without calling upstream. Warn/skip logs and continues once.
- [ ] For non-streaming/final-packet buffered responses, discard the captured response and return 502 for an invalid upstream representation or 503 for moderation failure in error mode; warn/skip passes the captured response through.
- [ ] Preserve pure tool-call/valid no-text response behavior, existing protocol deny wire formats, configured deny code/message, session/risk request vars, body restoration, signing, response headers, and successful allow/deny behavior.
- [ ] Use a deterministic failing `RoundTripper` for timeout/transport tests; do not use sleeps.
- [ ] Add tables covering timeout, backend 500, malformed JSON, missing/unknown risk level, unknown/empty request content, invalid non-streaming upstream response, buffered response moderation failure, warn/skip, clean and policy deny.
- [ ] Run:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/ai_aliyun_content_moderation -run "^(TestPostInitDefaultsFailModeToError|TestPostInitRejectsRealtimeResponseFailClosedMode|TestHandlerRejectsUnknownOrEmptyRequestContent|TestHandlerMapsRequestModerationFailures|TestHandlerDegradesExplicitlyOnRequestModerationFailure|TestHandlerRejectsInvalidNonStreamingUpstreamResponseWith502|TestHandlerFailsClosedOnBufferedResponseModerationFailure|TestModerateContentPreservesFailureClass|TestHandlerRecordsAliyunAllowDenyDegradedAndErrorOutcomes)$" -count=1'
bash -lc 'source .envrc && go test ./pkg/plugin/ai_aliyun_content_moderation -count=1'
```

## Task 5: Combined acceptance and independent delivery

- [ ] Run:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/ai_common ./pkg/plugin/ai_prompt_guard ./pkg/plugin/ai_aliyun_content_moderation ./pkg/observability/metrics -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/plugin/ai_common/... ./pkg/plugin/ai_prompt_guard/... ./pkg/plugin/ai_aliyun_content_moderation/... ./pkg/observability/metrics/...'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

- [ ] Run an independent merge-level review. Treat realtime error-mode rejection and Plan 19 handoff as an explicit boundary, not a silent skip.
- [ ] Stage only this plan and the listed source/test files.
- [ ] Commit with `git commit -m "fix(ai): fail closed when safety checks cannot run"`.
