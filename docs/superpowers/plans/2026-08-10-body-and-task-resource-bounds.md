# Body and Task Resource Bounds Implementation Plan

**Goal:** Close production-readiness items P1 5.2 and P1 5.13 with shared body limits and a bounded batch-request dispatcher that continues after an individual timeout.

**Architecture:** Use one limit+1 reader contract in `plugin/base`. Request-body consumers receive a 1 MiB default with plugin overrides. The buffered-response executor keeps its existing Plan 15 4 MiB incremental bound and shares the canonical default. Batch requests use an instance-owned dynamic admission gate, retain at most response-limit+1 bytes, and emit one ordered result per item.

## Frozen contracts

- Default request body: 1 MiB.
- Default buffered and batch subresponse body: 4 MiB.
- Default batch pipeline items: 20.
- Default concurrent batch workers: 8.
- Readers retain at most limit+1 bytes and return a typed size error.
- `proxy-mirror` and `traffic-split` replay the exact accepted body to the primary request path.
- A mirror or form-match body above its configured limit fails before the primary upstream with 413.
- An MCP message above its configured limit returns 413.
- A batch subresponse above its configured limit becomes a bodyless 502 for that item.
- An item timeout returns 504 and later items continue in input order; parent cancellation stops the pipeline.
- A cancellation-ignoring inner handler occupies one admission slot until it actually returns, so the number of retained workers is bounded by `max_concurrency`.

## Work units

### 1. Shared bounded body contract

- [x] Add generic request/response limited readers with exact-limit, limit+1, and read-error tests.
- [x] Make before-proxy hooks propagate a stored, repeatable error.
- [x] Map oversized before-proxy bodies to a stable APISIX 413 before terminal dispatch.
- [x] Apply request limits to `proxy-mirror`, form `post_arg_*` reads in `traffic-split`, and MCP message bodies.
- [x] Preserve the existing incremental buffered-response executor and source its default from the shared 4 MiB constant.

### 2. Bounded batch dispatch

- [x] Change the default pipeline item limit to 20.
- [x] Add metadata overrides for `max_concurrency` and `max_response_body_size`.
- [x] Add an instance-owned dynamic admission gate shared across batch requests.
- [x] Continue after item timeout while stopping on parent cancellation.
- [x] Bound subresponse retention and return bodyless 502 on overflow.
- [x] Test that cancellation-ignoring workers never exceed the configured concurrency and drain after release.

### 3. Verification and delivery

- [x] Run focused normal and race tests for the affected packages.
- [x] Run scoped formatting/lint and a build smoke check.
- [x] Perform a final five-axis review and remediate the weakened single-response test helper plus canceled-admission edge.
- [ ] Commit, push, open the PR, wait for CI, and squash-merge to `master`.
