# Etcd and Local Store Reliability Design

**Date:** 2026-08-19
**Base:** `origin/master` at `f99dd611671065df0878ac9266bda939f3c2b802`
**Scope:** user-approved P0 + P1 reliability findings in the traditional etcd configuration path

## Problem statement

The current watcher has sound revision-recovery basics, but the durable and published configuration boundaries are narrower than an etcd response:

- `/apisix` is watched with raw prefix matching, so sibling namespaces and operational records can enter the resource Store.
- a full snapshot deletes only keys remembered by the current process, so bbolt rows removed while the process was down survive restart;
- one watch response is committed one event at a time, allowing durable partial state, repeated fsyncs, and repeated reload hooks;
- `BuildStrict` snapshots routes, global rules, and metadata but reads services, upstreams, plugin configs, and SSLs from the live global Store;
- stream reload errors are logged after Store commit and do not prevent provider revision acknowledgement;
- malformed plugin metadata can disappear from a snapshot and silently fall back to defaults;
- startup fetch retry ignores cancellation, documented watch/resync settings are not wired through, and bbolt lock acquisition is unbounded.

## Reliability contract

1. The watched keyspace is the canonical configured root plus `/`. Only exact supported resource shapes are admitted. Sibling prefixes, collection sentinels, `data_plane/server_info`, and unknown subtrees are ignored.
2. A full etcd snapshot authoritatively replaces every managed bbolt bucket in one write transaction. Existing rows absent from etcd are removed even after process restart.
3. All valid mutations in one etcd snapshot/watch response commit atomically. Deterministically invalid resources are quarantined and excluded before one retry; their existing last-good row is preserved. Any non-validation failure leaves watcher revision state unchanged and enters snapshot recovery.
4. Derived indexes, generations, and update hooks publish only after the bbolt transaction commits. A batch emits at most one update notification per affected bucket.
5. HTTP route construction uses one immutable Store generation for all resources read during `BuildStrict`: routes, global rules, plugin metadata, services, upstreams, plugin configs, SSLs, and nested plugin references such as traffic-split `upstream_id`. Request-time consumer and consumer-group resolution remains live. The passed `*store.Store`, not the package global, owns the snapshot.
6. Malformed plugin metadata fails snapshot construction with bucket/id context. It never vanishes into an empty/default configuration.
7. When stream mode is enabled, stream publication is a required config-apply stage. A dynamic stream reload failure retains the runtime's last-good router, marks readiness unhealthy, returns an acknowledged hook error, and causes etcd snapshot recovery/retry without advancing the watcher revision. Initial publication, acknowledged reload, failed-start cleanup, and shutdown close are serialized so configuration cannot be lost or applied to a closing runtime.
8. Startup fetch and retry honor the server context. Snapshot recovery gives the remote Get and local Store/publication phases independent bounded time budgets. `watch_timeout` bounds an idle watch stream and reopens from the last applied revision without declaring etcd unavailable. Configured `resync_delay` controls recovery delay with up to 50 percent jitter.
9. bbolt lock acquisition has a finite timeout. An invalid persisted consumer row is skipped during pre-provider index rebuild so the authoritative provider can repair it; actual bbolt corruption still fails startup.

## Fixed interfaces

The Store batch boundary is represented by cloned values, never pooled child events:

```go
type Mutation struct {
	Type  EventType
	Key   []byte
	Value []byte
}

type ResourceKey struct {
	Bucket string
	ID     string
}

type BatchOptions struct {
	ReplaceManaged bool
	Preserve       []ResourceKey
}

type RejectedMutation struct {
	Index int
	Err   *ResourceValidationError
}

type BatchValidationError struct {
	Rejected []RejectedMutation
}

func NewAcknowledgedBatch(mutations []Mutation, options BatchOptions) *Event
```

`Event.Wait(ctx)` remains the single acknowledgement method. Existing single-event producers remain source-compatible. The event loop recognizes a batch event and executes it as one unit.

The Server uses a second, error-returning hook API for publication acknowledgement while retaining the existing notification hook API:

```go
type AcknowledgedEventUpdateHook func(event *Event) error

func (s *Store) AddAcknowledgedEventUpdateHook(hook AcknowledgedEventUpdateHook)
```

## Invalid resource handling

Validation is completed before a write transaction. If a batch contains invalid resources, Store returns one `BatchValidationError` containing stable mutation indexes and does not write anything. The etcd client then:

1. records each rejected full etcd key and revision in the bounded quarantine map;
2. removes rejected mutations from the candidate batch;
3. for a full replacement, asks Store to preserve the corresponding existing bucket/id rows;
4. retries the remaining candidate exactly once as one transaction.

This keeps unrelated valid resources moving without exposing an intermediate durable generation. A second validation error indicates a programming/invariant failure and aborts the response.

## Publication and revision ordering

The ordering for a successful response is:

```text
validate all resources
  -> one bbolt write transaction
  -> publish derived Store indexes/generation
  -> run coalesced bucket notifications
  -> synchronously acknowledge required stream publication
  -> commit watcher knownKeys/quarantine/lastRevision
```

HTTP publication remains asynchronous and last-good: a Store notification queues one coalesced rebuild, and the HTTP readiness stage reports its result independently. Stream publication is synchronous because the watcher must not acknowledge a revision that the enabled stream data plane rejected.

## Scope boundaries

This PR does not add offline startup from bbolt, a Store schema/version migration framework, checksums, automatic bbolt compaction, or a new external dependency. It does not claim a measured latency/throughput improvement; batching removes obvious per-key transactions and reload amplification, but benchmark evidence remains separate work under the repository benchmark contract.
