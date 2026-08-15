# AI Streaming and Provider Parity Implementation Plan

**Goal:** Close P1 5.3 and P1 5.12 against the current request/response phase runtime by preserving client streaming intent, making terminal stream failures observable, and matching APISIX 3.17 endpoint, priority, and consistent-hash behavior.

**Refresh baseline:** `9f8862d7bf5d01d80db8d2de2f677b8ba473d8fe` after Plan 24 merged.

**Pinned upstream:** Apache APISIX `9ef2ecab67f652d38365049613610ef649bb4ad0` and its `resty.chash` contract: 160 points per normalized node weight, CRC32 point generation, sorted node identity, and first clockwise point selection.

## Frozen contracts

- The existing request-local `ai_runtime.State` owns streaming intent. It is computed once from the client document before provider conversion and is not changed when AI multi advances to another provider.
- `ai-rate-limiting` selects bounded versus streaming response handling only from that client intent.
- A stream failure before any provider body byte is committed returns a stable 502. After commit, SSE and AWS EventStream emit a bounded protocol terminal error event when the client is still writable.
- Every terminal AI stream records exactly one fixed outcome: `success`, `error`, or `canceled`. Metric labels are closed enums and the same outcome is available to request logging.
- AIMLAPI defaults to `https://api.aimlapi.com/chat/completions` in both AI proxy owners.
- `ai-aws-content-moderation` priority is 1050.
- One shared ring implementation owns AI multi and traffic-split chash. Node weights are divided by their GCD, each normalized unit owns 160 points, node identity is sorted, and zero-weight nodes are excluded.
- The committed chash corpus is literal data generated independently from the pinned APISIX/resty algorithm. Removing a node may remap keys owned by that node but must not broadly reshuffle retained ownership.

## Task 1: Client streaming intent and outcomes

- [x] Add focused regressions proving a Bedrock client request remains streaming after provider conversion and AI rate limiting selects the streaming path.
- [x] Add malformed SSE and AWS EventStream regressions for precommit 502, postcommit terminal error event, first-chunk flush, and `success/error/canceled` outcome recording.
- [x] Implement the minimal existing-runtime changes and fixed-label metric facade.

## Task 2: Provider endpoint, priority, and chash

- [x] Add endpoint and full priority-table regressions before production edits.
- [x] Add the shared APISIX-compatible ring and literal pinned key-to-node/removal fixtures.
- [x] Replace AI multi and traffic-split modulo FNV selection with the shared ring, preserving health and retry behavior.
- [x] Prove both consumers select according to the same pinned owner.

## Task 3: Verification and delivery

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/chash ./pkg/plugin/ai_stream ./pkg/plugin/ai_runtime ./pkg/plugin/ai_proxy ./pkg/plugin/ai_proxy_multi ./pkg/plugin/ai_rate_limiting ./pkg/plugin/ai_aws_content_moderation ./pkg/plugin/traffic_split ./pkg/observability/metrics -count=1'
bash -lc 'source .envrc && go test -race ./pkg/plugin/chash ./pkg/plugin/ai_stream ./pkg/plugin/ai_proxy ./pkg/plugin/ai_proxy_multi ./pkg/plugin/ai_rate_limiting ./pkg/plugin/traffic_split ./pkg/observability/metrics -run "(StreamingIntent|StreamOutcome|TerminalError|Ketama|Chash|AIMLAPI|Priority)" -count=3'
bash -lc 'source .envrc && golangci-lint run ./pkg/plugin/chash/... ./pkg/plugin/ai_stream/... ./pkg/plugin/ai_runtime/... ./pkg/plugin/ai_proxy/... ./pkg/plugin/ai_proxy_multi/... ./pkg/plugin/ai_rate_limiting/... ./pkg/plugin/ai_aws_content_moderation/... ./pkg/plugin/traffic_split/... ./pkg/observability/metrics/...'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

- [x] Inspect all changed call sites and remove the old modulo/FNV helpers and imports.
- [x] Run an independent merge-level review and remediate confirmed findings; delivery follows after the final preflight.
