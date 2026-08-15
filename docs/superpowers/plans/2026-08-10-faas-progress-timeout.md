# FaaS Progress Timeout Implementation Plan

**Goal:** Close P1 5.17 by replacing the absolute whole-response timeout with connect, response-header, request-write, and response-read progress bounds, while making post-commit failures observable.

**Architecture:** `function_upstream` owns one shared `http.Client` per immutable transport configuration through `pkg/shared`. The base `http.Transport` owns connect/header/keepalive/TLS policy; `proxy.NewProgressTimeoutTransport` adds per-I/O send/read idle bounds; `http.Client.Timeout` remains zero. A manual response-copy loop distinguishes upstream reads from downstream writes, records a bounded response failure reason and metric, and aborts after a committed response instead of reporting a clean 2xx.

**Owners:** AWS Lambda, Azure Functions, and OpenFunction inherit the same `function_upstream.Plugin` client and `Stop` lifecycle.

## Constraints

- Existing `timeout` maps to connect, response-header, and per-I/O progress timeout; it is not a whole-request deadline.
- A response lasting longer than `timeout` succeeds when each chunk makes progress within the bound.
- An idle upstream body gap records `upstream_idle_timeout`; client cancellation and downstream write failure use distinct bounded reasons.
- Post-commit copy failure aborts the response and is never finalized as a clean completed 2xx.
- Shared clients are released on route generation retirement and close idle connections after the final reference.

### Task 1: Shared progress-timeout client

- [x] Add regression tests for zero `http.Client.Timeout`, progress wrapper composition, cross-owner sharing, final release, and config-key separation.
- [x] Build the client from connect/header/keepalive/TLS transport policy plus send/read progress wrappers.
- [x] Acquire it through `pkg/shared` and expose an idempotent promoted `Stop` lifecycle to all three owners.

### Task 2: Observable response-copy outcomes

- [x] Add a bounded response failure reason to the request outcome capture and a bounded FaaS failure metric facade.
- [x] Add real streaming regressions for total duration greater than timeout with continuous progress, idle gap, cancellation, upstream half-close, and downstream write failure.
- [x] Replace ignored `io.Copy` errors with an outcome-aware loop; record structured failure state and abort post-commit failures.

### Task 3: Verify and deliver

```bash
bash -lc 'source .envrc && go test -race ./pkg/plugin/function_upstream ./pkg/plugin/aws_lambda ./pkg/plugin/azure_functions ./pkg/plugin/openfunction ./pkg/proxy ./pkg/plugin/base ./pkg/observability/metrics -run "(Progress|Timeout|Stream|Cancel|Shared|Copy|FailureReason)" -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/plugin/function_upstream/... ./pkg/plugin/aws_lambda/... ./pkg/plugin/azure_functions/... ./pkg/plugin/openfunction/... ./pkg/proxy/... ./pkg/plugin/base/... ./pkg/observability/metrics/...'
bash -lc 'source .envrc && make build'
```

- [x] Commit with `git commit -m "fix(faas): use progress timeouts for streamed responses"`.
