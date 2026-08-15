# gRPC Transport and Transcode Contract Implementation Plan

> **Execution:** Implement sequentially in one isolated worktree. Add each regression before its production fix and keep verification impact-scoped.

**Goal:** Close P1 5.11 and P1 5.16 by routing h2c/grpcs through cluster admission/progress/retry ownership and preserving gRPC status/detail/trailer semantics.

**Architecture:** Add cleartext HTTP/2 as an immutable cluster transport mode so `grpc://` no longer replaces the cluster's RoundTripper. Keep `grpcs://` on the existing TLS HTTP/2-capable transport. Extend the canonical bounded response state with detached trailers, commit them through the single final-response owner, and normalize grpc-transcode status without discarding the error representation.

**Tech Stack:** Go 1.26 HTTP/2, gRPC framing/protobuf status, shared proxy cluster.

## Global Constraints

- grpc/h2c observes `max_in_flight`, connect/header/read/write progress timeouts, retry budget, metrics, and cluster close.
- Successful transcode declares `Trailer: grpc-status, grpc-message` and forwards final values.
- Non-zero status preserves status/details; malformed frames return a deterministic gateway error before commit when possible.
- Streaming remains explicitly unsupported and rejected at route compile.

### Task 1: Unify cluster transport

- [x] Add h2c cluster tests for protocol selection, admission rejection, progress timeout, retry observation, and close ownership.
- [x] Add the transport mode to cluster identity and set it from the resolved upstream scheme.
- [x] Remove the bare h2c replacement and its duplicate retry wrapper from route builder.

### Task 2: Stabilize transcode output

- [x] Add bounded writer/executor tests proving declared trailers are detached, cloned, and committed after the response body.
- [x] Add gRPC fixture tests for success trailers, non-zero status with/without details-bin, unknown status, malformed frame, and build-time streaming rejection.
- [x] Preserve a normalized `{"error":...}` representation for non-zero status even when `show_status_in_body=false`; malformed unary frames fail as a deterministic gateway response before commit.

### Task 3: Verify and commit

```bash
bash -lc 'source .envrc && go test -race ./pkg/proxy ./pkg/route ./pkg/plugin/grpc_transcode -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/proxy/... ./pkg/route/... ./pkg/plugin/grpc_transcode/...'
bash -lc 'source .envrc && make build'
```

- [ ] Commit with `git commit -m "fix(grpc): unify transport and response contracts"`.
