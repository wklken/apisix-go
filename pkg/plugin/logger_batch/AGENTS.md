# Logger Batch Instructions

This file inherits `pkg/plugin/AGENTS.md` and applies to the shared network and
broker logger batch processor. It does not govern `file-logger`'s byte-aware
processor.

## Fixed topology and bounds

- Task components are exactly `batch-scheduler`, `batch-worker`, and
  `batch-shutdown` under the compiler-injected plugin owner.
- Default to one delivery worker; trusted constructors may request at most
  eight. Do not expose worker count as a route-schema field without a new
  product decision.
- Default pending-entry capacity is 10,000 across buffered, queued, active, and
  retrying entries. Reject at capacity; detached log phase never blocks.
- Each delivery attempt has a 10-second deadline. Retry delay and all sink I/O
  must observe cancellation. Shutdown budget defaults to 15 seconds.

## Teardown and metrics

- Stop admission, seal the buffer, and join every admitted callback. Timeout or
  residual retains sink/client/sender authority for a later retry.
- Never release resources merely because the caller-facing shutdown deadline
  elapsed; callback return is the ownership fence.
- Keep pending-entry gauges refcounted across overlapping generations. Delete
  them only after the final processor owner closes. Keep event labels bounded.

## Focused verification

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./pkg/plugin/logger_batch -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test -race ./pkg/plugin/logger_batch -count=1'
```
