# Proxy Runtime Acceptance

This contract detects regressions in the local proxy harness. It does not
predict public-network or cross-region production capacity.

## Benchmark scope

The corpus covers weighted upstream selection and loopback requests through
route matching, plugin middleware, retries/timeouts, `ReverseProxy`, and
response copying. Focused hot-path rows measure request-pipeline materialization
and metrics finalization.

Accepted evidence comes only from the repository benchmark runner. Baseline and
candidate runs must use identical corpus fingerprints, settings, hardware, and
the declared Go 1.26 toolchain.

## Comparative gates

| Metric | Rejection threshold |
| --- | --- |
| Primary latency | More than 10% statistically significant slowdown in `routes=100/plugins=none/nodes=10`. |
| Other affected rows | More than 10% statistically significant slowdown. |
| Allocation bytes | More than 512 additional B/op without an accepted retained object. |
| Allocations | More than 2 additional allocs/op without an accepted retained object. |

A metadata or corpus mismatch invalidates the comparison.

## Stability gate

The canonical soak is 30 minutes at concurrency 256. It must report:

- zero unexpected errors;
- no race findings in the focused concurrency paths;
- final goroutines no more than warmup baseline plus 32;
- heap in use after two final GCs no more than 25% above the warmed five-minute
  sample; and
- bounded p50, p95, p99, and p999 latency estimates plus request, allocation,
  GC CPU, and runtime pause deltas.

Fault coverage must preserve the documented 502/504 mapping, retry rules,
committed-body abort behavior, cluster admission recovery, and active-health
quarantine/re-admission.

Runtime allocation and pause metrics cover the whole single-process harness,
including clients, gateway, fixtures, and test machinery. They are comparative
harness evidence, not gateway-only measurements. A five-second run validates
wiring only; it is not stability evidence.

## Evidence

Use the `benchmark-*` Make targets. Raw results, metadata, and profiles remain
under ignored `.cache/bench`. A report records the commit, hardware, Go
version, commands, affected rows, and every regression.
