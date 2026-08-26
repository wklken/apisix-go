# Stream Snapshot and Runtime Instructions

This file inherits the repository root `AGENTS.md` and applies to `pkg/stream`.

## Snapshot compiler

- `CompileRouter` is detached and pure: clone route/upstream data and do not
  open listeners, call factories/materializers, acquire resources, or start
  tasks.
- Resolve route/service/upstream and plugin precedence before side effects.
  Dynamic `/plugins` absent means static fallback; present-empty disables all.
- Current scope is raw TCP plus at most one `mqtt-proxy` protocol owner. Multiple
  protocol plugins or unsupported stream plugins fail preparation.
- Stream TLS listener/upstream, UDP, PROXY protocol, discovery, and general
  stream-plugin chaining remain unsupported unless explicitly implemented.

## Persistent runtime

- Listener and connection tasks use the process-local `core/stream-runtime`
  owner. Do not introduce raw goroutines or generation-owned listeners.
- Every accepted connection acquires the exact router generation and retains it
  for the complete connection. Activation/rollback only changes later accepts.
- Bridge children use `RequestTaskGroup`; cancellation/hard error closes both
  peers, EOF preserves half-close, and panic rethrows only after peer cleanup
  and join.
- `Close` is retryable: cancel, close listeners/connections, join tasks, return
  exact residuals, and keep authority until a later close completes.

## Focused verification

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./pkg/stream ./pkg/stream/bridge -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test -race ./pkg/stream ./pkg/stream/bridge -count=1'
```
