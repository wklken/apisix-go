# Plugin Phase and Panic Boundary Decomposition

**Status:** Superseded as a single implementation PR. This file is the coordination umbrella for seven independently reviewed and merged plans.

The original plan combined panic recovery, request lifecycle, global/route/consumer ordering, authentication handoff, buffered transforms, streaming/hijack, and log/finalizer migration. Current-code and pinned APISIX 3.17 review proved those concerns cannot share one safe writer or one-plugin/one-phase model. In particular, response phases run in explicit scope/priority order rather than legacy middleware unwind; consumer resolution must occur between route rewrite and access; post-commit panic must abort; and streaming/hijack cannot silently enter a full-body recorder.

## Ordered PR chain

| Order | Plan | Incremental acceptance |
| --- | --- | --- |
| 11 | `2026-08-10-plugin-panic-outcome-foundation.md` | Request lifecycle, LIFO infrastructure finalizers, response outcome capture, stable pre-commit panic and post-commit abort |
| 12 | `2026-08-10-plugin-request-phase-bridge.md` | Mixed explicit/legacy request executor; request-context, request-id and limit-conn bridge |
| 13 | `2026-08-10-plugin-scoped-rewrite-executor.md` | System/global/route rewrite scope ordering for an audited request-only set |
| 14 | `2026-08-10-plugin-consumer-access-executor.md` | Authentication handoff, consumer/group merge, consumer rewrite, global/route access |
| 15 | `2026-08-10-plugin-buffered-response-phases.md` | Explicit header/body phases for bounded transforms and cache storage |
| 16 | `2026-08-10-plugin-streaming-terminal-phases.md` | Streaming/compression/protocol/hijack compatibility and explicit terminal ownership |
| 17 | `2026-08-10-plugin-log-finalizer-phases.md` | Ordered log/finalizer execution and complete plugin capability registry |

Each row is one branch, one plan document, one independent review, and one PR. A later row starts only from master after its predecessor merges; interfaces are refreshed against that merge before dispatch.

## Finding closure

- Plan 11 supplies the panic/outcome foundation and partially addresses P1 5.5, but does not claim phase parity.
- Plans 12–16 incrementally replace legacy middleware behavior; none individually claims PR-014 closure.
- Plan 17 closes PR-014 and the remaining P1 5.5 only after its all-registered-plugin capability test, legacy post-next scan, focused race gates, and independent merge-level review pass.

## Shared invariants

- Pinned order is system setup → global rewrite → route rewrite/auth → consumer merge/rewrite → global access → merged route access → before-proxy → exclusive protocol owner or upstream → global then merged header/body/log phases → infrastructure finalizers. Conditional terminals execute exactly once inside their declared rewrite/access/before-proxy stage rather than in a second terminal pass.
- Scope and phase order win over priority; priority is descending only within one scope/phase.
- A plugin may implement multiple capabilities.
- Stable pre-commit panic response bypasses configurable status/body transforms. Committed, flushed, or hijacked panic writes no fallback and aborts after finalization.
- Lifecycle LIFO is only for defer-style infrastructure ownership. APISIX log order is explicit inside one composite finalizer.
- Unsupported buffered/streaming/hijack combinations fail strict route build and preserve the last-good generation.

This umbrella is not dispatched and must not be included as the sole plan document in any implementation PR.
