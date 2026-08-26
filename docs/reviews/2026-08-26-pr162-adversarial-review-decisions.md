# PR #162 adversarial review decisions

This ledger records the owner-confirmed disposition of the independent reviews
of `master@92587aca` after Tasks 1–11. It is a review input, not a production
qualification claim. Re-report an item only when the implementation or the
stated precondition changes, or when new evidence contradicts the decision.

## Fixed on PR #163

| Item | Governing behavior after remediation | Evidence |
| --- | --- | --- |
| Environment proxy use by plugin and secret HTTP clients | Internal control-plane, identity, policy, and managed-secret clients use proxy-disabled transports unless the plugin has an explicit proxy contract. | `pkg/httpclient`, `pkg/secret`, affected plugin tests |
| Kafka WebSocket `Origin` | PubSub and raw bridge upgrades apply the configured origin policy instead of accepting every browser origin. | `pkg/plugin/kafka_proxy` |
| Stream routes sharing a listener | Different `remote_addr` predicates may share one listener. Overlapping routes are ordered by provable set containment; disjoint routes coexist; equal or incomparable overlaps fail compilation. | `pkg/stream/router.go`, ADR-0004 |
| `graphql-proxy-cache` generation state | Cache ownership is instance/generation scoped; Prepare and failed-generation cleanup cannot overwrite or delete the active generation's route cache. | `pkg/plugin/graphql_proxy_cache` |
| Process probes on data-plane paths | `/livez` and `/readyz` are ordinary route paths. The separate status listener defaults to `127.0.0.1:7085`; `/status` is liveness and `/status/ready` means a serviceable committed configuration exists. Etcd loss does not withdraw readiness while last-good remains serviceable. | `pkg/server/server.go`, `pkg/config/defaults.go` |
| Empty `trusted_addresses` | Empty means trust nobody. An untrusted peer's inbound X-Forwarded-For is removed; an explicitly trusted peer may preserve the chain. Strict mode still requires an explicit non-empty trust set. | `pkg/server/server.go`, config validation |
| SLS wire authentication | The generation-owned access-key secret is serialized only into the RFC5424 SLS structured-data authentication field over TLS, then revoked on plugin stop. Public config and diagnostics retain only a descriptor. HMAC signing is not the SLS syslog contract. | `pkg/plugin/sls_logger` |
| Serverless Lua retirement blocking | Prepare-time chunk evaluation and request execution have an operator-controlled, non-zero hard deadline. GopherLua is context-interrupted before the call returns; OS/IO and filesystem-loading entry points are unavailable. | `pkg/plugin/serverless`, ADR-0004 |
| Plugin metadata secret sharing | Shared canonical metadata contains descriptors, not plaintext. Each factory receives a scoped view; generation cleanup revokes all views after plugin cleanup. | `pkg/runtime/metadata.go`, `pkg/compiler/metadata_preparer.go` |
| HMAC request-body buffering | One streaming SHA-256 replay snapshot is shared across multi-auth probes and downstream handling. It spills above the memory threshold to a mode-0600 temporary file and is removed at request finalization. The validation cap is the lower of HMAC and ingress limits. | `pkg/plugin/base/request_body_snapshot.go`, HMAC/multi-auth tests |

## Strict-only hardening; compat behavior is intentional

| Item | `compat` | `strict` | Re-report condition |
| --- | --- | --- | --- |
| Chaitin WAF debug response headers | Preserves APISIX-compatible `append_waf_debug_header`. | Rejects `append_waf_debug_header: true` during plugin preparation. | Strict accepts the option or leaks the internal error/address without the option. |
| CSRF cookie transport | Preserves the APISIX-compatible non-Secure default and current double-submit token format. `HttpOnly` remains false because client JavaScript must read the token. | Forces `Secure`. | Strict emits a non-Secure cookie, or a versioned/session-bound token migration is proposed with compatibility evidence. |
| Referer leading-star pattern | Preserves the APISIX-compatible suffix meaning of `*example.com`. | Accepts exact hosts, `*`, and `*.example.com`; rejects ambiguous leading-star forms. `*.example.com` does not include the apex. | Strict accepts an ambiguous pattern or changes wildcard/apex behavior. |

Do not replace the existing CSRF signature with HMAC as a stand-alone cleanup.
A token-format change needs versioning, session/user binding, migration, and
rollback evidence; otherwise it changes bytes without fixing token replay.

## Deferred structural work

| Item | Decision | Re-report condition |
| --- | --- | --- |
| Consumer credentials remain plaintext in generation memory | Request authentication requires usable credentials. Go cannot guarantee zeroization of all copies. A future redesign should expose plugin-specific credential records and redacted consumer identity rather than a generic consumer document, but it is not part of PR #163. | A plaintext value reaches config dump, logs, metrics, `$consumer`, another plugin, or survives generation cleanup; or a concrete credential-record design is proposed. |
| `wolf-rbac` retry uses `time.Sleep` in a request goroutine | Accepted low-priority reliability debt. Go parks the goroutine and the current retry budget is bounded to 200 ms; this is not a generation-retirement blocker. Prefer a request-context-aware timer in a focused change. | Sleep becomes unbounded, ignores a material cancellation interval, holds a generation-owned background task, or causes measured capacity loss. |

## Accepted boundaries and dismissed findings

- Serverless configuration is operator-equivalent code execution, but the
  runtime still enforces the hard deadline and safe-library boundary. Full
  OpenResty/`ngx_lua` fidelity is intentionally out of scope.
- Consumer/plugin configuration may contain plaintext inside the exact owning
  generation when runtime use requires it. Diagnostics and shared views must
  remain redacted and generation cleanup must revoke the authority.
- The SLS logger does not need an invented request HMAC. Its compatibility
  contract is RFC5424 structured-data credentials over verified TLS.
- Default authentication fail-open, durable-store publication ahead of the
  handler, CSRF `HttpOnly`, and “Vault supports only KV v1” were not confirmed
  against the reviewed source and should not be reintroduced as findings
  without new call-path evidence.

## Review protocol

Future reviews should check this file before filing a duplicate. A known item
is still actionable when its guard test is removed, the documented profile
boundary changes, secret material crosses an additional authority boundary, or
new current-source evidence invalidates the original conclusion.
