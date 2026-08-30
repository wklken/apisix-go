# Serving Generation Instructions

This file inherits the repository root `AGENTS.md` and applies to `pkg/server`.

## Generation engine

- Maintain one atomic active bundle with independent HTTP and stream revisions
  and owners. A one-domain update must preserve the other domain exactly.
- Publish prepares detached candidates and swaps the complete active bundle
  atomically. A failed publish leaves the predecessor active.
- Complete the provider's initial synchronization before opening listeners.
- The current runtime is single-process. Do not document or code against an
  assumed external supervisor/worker lifecycle that does not exist.

## Lease and request lifecycle

- Every HTTP request and TLS selection pins the exact HTTP generation. Batch
  children retain a live parent lease; hijack transfers a retained lease to the
  wrapped connection until `Close`.
- Retire an owner only when it owns no active domain and all leases drain.
- Preserve request ordering: sanitize inbound consumer identity, acquire lease,
  establish response/body state, run lifecycle, complete then finalize all
  callbacks, release vars and leases last.
- Plugin panic is bounded attribution; exact `http.ErrAbortHandler` remains a
  sentinel; downstream/core panic is not relabeled and re-panics only after
  cleanup. The standard `net/http.Server` boundary currently still recovers it.
- Preserve shutdown phases: stop/join the config producer -> reject listeners
  and new route leases while initiating stream close -> drain HTTP, generation
  leases, and stream runtime -> close generation engine -> secret resolver ->
  metrics/exporter/tracing. An incomplete authority-owning phase,
  including residual/deadline, cannot advance; terminal errors are recorded by
  their phase and completed phases are not repeated.

## Focused verification

```bash
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test ./pkg/server -run "^(TestGenerationEngine|TestGenerationOwner|TestRouteHandler|TestFrontendTLSSelector|TestServerShutdown|TestServerStartsProvider)" -count=1'
bash -lc 'source .envrc && export GOFLAGS=-mod=readonly && scripts/go_cache.sh run -- go test -race ./pkg/server -run "^(TestGenerationEngine|TestGenerationOwner|TestRouteHandler|TestServerShutdown)" -count=1'
```
