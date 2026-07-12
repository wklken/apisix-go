# Passive health-aware upstream selection

> This plan covers the next bounded APISIX 3.17 parity slice. APISIX-native
> active probes remain deferred; this slice only consumes observed request
> outcomes through the existing Go proxy boundary.

## Goal

Use one shared `pkg/proxy` abstraction for passive upstream health state so
ordinary HTTP routes and the existing `dubbo-proxy`/`http-dubbo` terminals can
quarantine repeatedly failing nodes without inventing NGINX/Tengine state.

## Steps

- [x] Add failing `pkg/proxy` tests for weighted selection, HTTP-status and TCP/
   timeout thresholds, node quarantine, and the documented fail-open behavior
   when every node is unhealthy.
- [x] Implement a concurrency-safe health-aware load balancer that preserves
   weighted round-robin selection, parses the supported `checks.passive`
   fields, reports observed HTTP/TCP outcomes, and leaves active checks
   explicitly unsupported.
- [x] Build route upstreams through the shared abstraction, retain the selected
   target in request context, and report response/error outcomes from the
   reverse-proxy callbacks.
- [x] Reuse the same selection/reporting state for the existing Dubbo terminals;
   do not retry after request bytes are written and do not add persistent
   multiplexing or active probes in this slice.
- [x] Add focused tests, update README/checklist/remaining-TODO/execution-TODO
   with the exact passive-only boundary, then run focused/race/full tests,
   build cleanup, and `git diff --check` through `source .envrc`.

## Explicit boundaries

- `checks.active` is accepted by the existing resource model but does not
  start background HTTP/HTTPS/TCP probes.
- Passive unhealthy nodes stay out of selection until a fresh load-balancer
  snapshot/reload; if all nodes are unhealthy, selection fails open as APISIX
  documents.
- Health state is local to one Go load-balancer instance and is not exposed as
  APISIX's `/v1/healthcheck` control-plane model.
