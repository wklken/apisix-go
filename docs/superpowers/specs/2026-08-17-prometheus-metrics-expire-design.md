# Prometheus Metric Series Expiration Design

## Purpose

Add bounded, idle-time expiration for the seven request-driven Prometheus metric families that Apache APISIX 3.17 exposes with an `expire` option:

- `http_status`
- `http_latency`
- `bandwidth`
- `llm_latency`
- `llm_prompt_tokens`
- `llm_completion_tokens`
- `llm_active_connections`

The change must reclaim both the `client_golang` vector child and the corresponding cardinality-budget entry. It must not allow metric churn to grow process memory without a configured bound, and background cleanup must not share the Prometheus scrape request path or leave a goroutine behind after server shutdown.

## Scope

This change covers the seven existing dynamic HTTP and LLM metric families above. Proxy-runtime, logger-batch, AI-safety, and configuration-health metrics retain their existing resource-lifecycle ownership and are not given a second TTL owner.

The Prometheus exporter address, port, URI, and scrape implementation are unchanged. Export caching and moving collection into another process are separate concerns.

## Configuration Contract

Each supported family accepts the APISIX-compatible setting below, expressed as a non-negative integer number of seconds:

```yaml
plugin_attr:
  prometheus:
    max_http_series: 10000
    max_llm_series: 10000
    metrics:
      http_status:
        expire: 300
      http_latency:
        expire: 300
      bandwidth:
        expire: 300
      llm_latency:
        expire: 300
      llm_prompt_tokens:
        expire: 300
      llm_completion_tokens:
        expire: 300
      llm_active_connections:
        expire: 300
```

Missing `expire` and `expire: 0` disable expiration for that family. Negative, fractional, overflowing, or non-numeric values fail metric initialization with the full configuration path in the error.

`max_http_series` continues to limit each HTTP family independently. The new `max_llm_series` limits each LLM family independently and uses the same accepted range and default as `max_http_series`: default `10000`, minimum `100`, maximum `100000`.

Configuration remains startup-only because the metrics registry is initialized once. Changing an expiration or capacity setting requires a process restart.

## Series Tracker

Replace the HTTP-only `seen` budget with a reusable tracker owned by each of the seven metric families. A tracked entry contains:

- a collision-resistant key derived from the complete ordered label tuple;
- an immutable copy of the label values needed by `DeleteLabelValues`;
- an atomically refreshed `lastSeen` timestamp;
- an in-flight reference count used by active-connection gauges.

The tracker's exact-label entry map is the authoritative admission and expiration index. There is no separate `seen` map, so deleting an expired entry necessarily releases its budget slot.

Each tracker has a hard entry limit. Once full, an unseen tuple is written to one family-specific synthetic tuple in which every label is `__overflow__`. The synthetic tuple is not inserted into the dynamic index and is never expired. Therefore each vector has at most `limit + 1` children created by this subsystem, while the `lastSeen` index never exceeds `limit`. Existing tuples continue to update normally when the budget is full.

HTTP overflow observations continue to increment `http_metric_series_overflow_total{metric=...}`. LLM overflow observations use a new `llm_metric_series_overflow_total{metric=...}` counter so capacity pressure is visible without mislabelling it as HTTP traffic.

## Concurrent Write and Delete Protocol

`client_golang` permits concurrent vector operations, but a handle obtained before `DeleteLabelValues` can continue accepting updates while no longer being exported. The tracker must therefore coordinate application writes with deletion rather than merely making its own map race-free.

Each tracker uses an `RWMutex`:

1. An update of an existing tuple takes a read lock, refreshes the atomic `lastSeen`, obtains and updates the vector child, and only then releases the lock. Multiple existing-series updates can proceed concurrently.
2. Admission of a new tuple takes the write lock, double-checks whether another request admitted it, inserts the bounded entry, and performs the first vector update before unlocking.
3. Expiration takes the write lock, rechecks the candidate timestamp and in-flight count, calls `DeleteLabelValues`, and deletes the tracker entry in the same critical section.

This prevents the sequence “cleaner deletes the child and index entry, then an earlier request updates a detached or untracked child.” The metric update remains inside the read-side critical section, but ordinary updates are not globally serialized.

## Active LLM Connections

`BeginLLMRequest` acquires a tracked `llm_active_connections` entry, increments both its exported gauge and in-flight reference count, and returns a release closure.

The release closure decrements the exported gauge, decrements the in-flight reference count, and refreshes `lastSeen` under the tracker protocol. The expiration scan skips every entry with a non-zero in-flight count. A request lasting longer than the configured TTL therefore remains exported and cannot leave its completion callback writing to a deleted gauge child.

The synthetic overflow gauge is never expired, so overflow acquisitions can safely increment and decrement it without dynamic index entries.

## Expiration Runtime

One expiration runtime goroutine scans all enabled family trackers. No goroutine is started when all seven expiration values are zero.

The scan interval is calculated once at startup as:

```text
clamp(minimum positive family expiration / 2, 1 second, 1 minute)
```

Thus an idle series is normally deleted between `expire` and `expire + scan interval` after its last update. Expiration uses `lastSeen <= now - expire`; it does not promise deletion at an exact wall-clock instant.

To bound request-path disruption, a scan first finds candidates while holding a family read lock, then obtains the write lock and deletes at most 256 revalidated entries from that family per tick. Existing-series observations remain concurrent during candidate discovery. New admissions can pause during the bounded map walk, while actual metric updates are blocked only during the bounded delete phase.

The server starts the runtime after successful metric initialization. The runtime owns a child context and an idempotent stop function. Server shutdown and startup-failure cleanup cancel it and wait for its goroutine to exit. Starting a second runtime for the same global metrics manager is rejected rather than silently creating duplicate cleaners.

## Expiration Semantics

Expiration is based on successful metric observations, not scrape activity. Scraping does not refresh `lastSeen`.

Deleting a counter or histogram series discards its cumulative process-local value. If the same label tuple appears later, it is admitted as a new series starting from zero. This matches an idle-series TTL rather than persistent counter storage.

Failure or a false return from `DeleteLabelValues` does not retain the tracker entry. The tracker remains the source of truth for capacity, and a later observation recreates the child consistently.

## Error Handling and Observability

- Invalid configuration fails startup before the expiration goroutine starts.
- A duplicate runtime start returns an explicit error.
- Shutdown cancellation is normal and is not logged as an error.
- Cleanup has no network or storage dependency.
- Overflow counters expose observations redirected because a family reached its hard limit.

## Test Strategy

Implementation follows test-first development. Focused tests must demonstrate:

1. parsing of all seven nested `expire` settings, default/zero behavior, and invalid values;
2. parsing and bounds of `max_llm_series`;
3. a series is absent from a gathered registry after expiration;
4. expiration removes the tracker entry and a new tuple can occupy the released slot;
5. a refreshed tuple is not deleted using a stale scan candidate;
6. a family never exceeds its dynamic index limit and creates at most one overflow child;
7. HTTP and LLM overflow counters identify the affected family;
8. active LLM connections remain pinned beyond TTL and expire only after release and another idle period;
9. the expiration runtime starts only when needed, stops on cancellation, and rejects duplicate starts;
10. concurrent observation, admission, release, and expiration pass the race detector without resurrecting untracked vector children;
11. existing HTTP metric labels and ordinary below-limit values remain unchanged.

Verification is impact-scoped:

- focused unit tests for `pkg/observability/metrics`;
- focused server lifecycle tests in `pkg/server`;
- race tests for the two affected packages;
- repository lint and build gates.

## Documentation Changes

`conf/config-default.yaml` will show `expire` under each supported `metrics.<family>` entry rather than as a top-level Prometheus option. `docs/configuration.md` will document units, disabled behavior, deletion delay, counter reset semantics, per-family capacity, `max_llm_series`, and the requirement to restart after configuration changes.

## Source Contracts

- Apache APISIX 3.17 reads `plugin_attr.prometheus.metrics.<family>.expire` and passes it to the seven corresponding metric constructors: <https://github.com/apache/apisix/blob/3.17.0/apisix/plugins/prometheus/exporter.lua#L139-L242>.
- The project-pinned `client_golang` API removes exact vector children with `DeleteLabelValues`; retained handles are not safe lifecycle ownership after deletion: <https://github.com/prometheus/client_golang/blob/v1.23.2/prometheus/vec.go>.

## Acceptance Criteria

- The seven supported families accept the APISIX 3.17-compatible nested `expire` path.
- Every dynamic last-seen index has a configured hard limit.
- Expiring an HTTP child also frees its cardinality-budget slot.
- Metric writes and expiration cannot recreate an untracked or detached exported child.
- Active LLM connections are never expired while in flight.
- At most one bounded cleanup goroutine runs, and it terminates during all server shutdown and failed-start paths.
- Expiration work is independent of scrape requests and bounded per family per tick.
- Missing or zero expiration preserves current non-expiring behavior.
- Focused tests, race tests, lint, and build pass.
