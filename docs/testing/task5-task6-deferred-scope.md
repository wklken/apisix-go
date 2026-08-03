# Task 5 and Task 6 deferred test-nginx scope

## Decision

Task 5 and Task 6 are intentionally not supported in the current phase. This
document records their exact source scope so Task 7 completion is not mistaken
for full conversion of every Apache APISIX `t/plugin/**/*.t` block.

The baseline is Apache APISIX commit
`c3d7d5ec69774121f53d2e20d29d09c816795dd7`. The authoritative per-file and
per-label disposition remains
[`apisix-test-nginx-corpus-audit.md`](apisix-test-nginx-corpus-audit.md).

## Accounting

| Scope | Source shape | TEST blocks | Current decision |
|---|---:|---:|---|
| Task 5: registered APISIX 3.17 defaults marked `Deferred` | 35 fully unreferenced source files | 448 | Do not add tests or claim parity until each deferred plugin/subsystem has an approved Go contract. |
| Task 6: native/runtime plugins | 20 fully unreferenced source files | 443 | Decide Go-native support, external-runtime integration, or permanent non-goal before implementation. |
| Task 6: non-default, cross-plugin, regression-only, or newer sources | 5 fully unreferenced source files | 65 | Require an explicit product-scope decision; registration in upstream APISIX alone is not authorization. |
| Task 6: unselected shared-source partitions | 2 partially selected source files | 38 | Classify the remaining cross-plugin security-warning behavior independently from the converted labels. |
| Task 6: residual blocked partitions from already-supported owners | mixed partially converted sources | 13 | Resolve Lua/OpenResty/native-only semantics instead of weakening them into unrelated Go assertions. |
| Total deferred decision pool | — | 1,007 | 957 plugin-owned blocks plus 50 non-plugin/framework blocks. |

The 957 figure always means plugin-owned blocks. The 50 non-plugin blocks are
tracked separately and must not be counted as missing plugin implementations.

## Task 5 contents

Task 5 covers registered defaults that are present in the APISIX 3.17 plugin
surface but remain `Deferred` in `docs/plugins.md`. The current source families
include:

- `ai-aliyun-content-moderation`, `ai-prompt-template`, the base `ai` family,
  and the separate `ai-proxy-multi` subsystem;
- `grpc-transcode` and `grpc-web`;
- `mcp-bridge`;
- Prometheus exporter and metric-lifecycle cases;
- Zipkin tracing cases.

These are not simple manifest omissions. They require product behavior,
protocol ownership, lifecycle semantics, or observability contracts to be
settled first. Adding standalone YAML that only exercises a nearby code path
would create false parity.

Task 5 may resume only when a specific deferred owner is approved with:

1. a Go-native behavior contract and explicit non-goals;
2. required product implementation and registration status;
3. a source-label-to-Go-test mapping;
4. real-process or protocol-fixture assertions for observable behavior;
5. aligned `docs/plugins.md` status and full repository gates.

## Task 6 contents

Task 6 owns behavior whose support status cannot be inferred from an APISIX Lua
test alone. Its current families include:

- unregistered AI subsystems such as `ai-cache`, `ai-lakera-guard`, and AI
  transport integration;
- the external plugin runner protocol (`ext-plugin-pre/post-req/resp`);
- Lua/OpenResty or NGINX-native features such as `inspect`, OCSP stapling,
  serverless Lua execution, body-transformer Lua/multipart objects, and
  proxy-rewrite serverless/CRLF internals;
- separate protocol or subsystem owners such as Dubbo proxy;
- cross-plugin TLS security-warning partitions;
- internal Lua unit tests that do not map directly to a public Go plugin
  boundary;
- plugin/consumer framework regression blocks that are `non_plugin`, not
  missing plugin cases.

Before Task 6 implementation, every audit row must be assigned one of four
durable dispositions:

- **Go-native support:** define the observable Go contract and implement it.
- **External-runtime integration:** define and own the external protocol and
  its lifecycle rather than adding a placeholder.
- **Permanent non-goal:** document the native/runtime limitation in
  `docs/plugins.md` and keep the row blocked with a precise reason.
- **Framework migration:** for `non_plugin` rows, port the regression to the
  owning Go framework package instead of inventing a plugin manifest.

## Relationship to Task 7

Task 7 is complete for the converted corpus: all 99 owner manifests were
semantically re-audited, F-001 through F-022 were repaired, and the full Go
test, lint, and build gates passed. That result does not reclassify any Task 5
or Task 6 row.

Until a later explicit scope decision, the repository must continue to report:

- 4,048 converted upstream TEST blocks;
- 957 deferred plugin-owned TEST blocks;
- 50 deferred non-plugin/framework TEST blocks;
- no claim that all 5,055 upstream blocks are implemented in Go.
