# Task 7 APISIX test-nginx corpus remediation plan

## Goal

Close the 22 findings recorded in `docs/testing/apisix-test-nginx-corpus-audit.md` and make the Go test corpus demonstrate the behavior represented by the corresponding Apache APISIX `test-nginx` cases.

Completion means:

- every F001-F022 finding has concrete Go test evidence or an explicit Go-native disposition for an NGINX/OpenResty-only observation;
- standalone manifests use plaintext inputs for data-encryption write-path cases, prove runtime decryption, and inspect encrypted bbolt persistence after shutdown;
- same-resource update/removal cases exercise live standalone reloads instead of substituting independent resources or `_meta.disable`;
- behavioral boundary cases prove the intended effect rather than merely returning the same response;
- the audit document is updated only after the repaired tests pass;
- `go test ./... -count=1`, `make lint`, and `make build` pass with the checkout-local runtime.

## Scope and decisions

This plan covers Task 7 only. Task 5 and Task 6 remain intentionally
unsupported and are recorded separately in
`docs/testing/task5-task6-deferred-scope.md`.

Apache source is pinned at `c3d7d5ec69774121f53d2e20d29d09c816795dd7`. The Go implementation does not need to reproduce NGINX temporary-file log counts or OpenResty phase callbacks. Those cases must instead prove the corresponding Go-native contract:

- `proxy-control`: a 51,200-byte request crosses the real proxy path, while package tests distinguish eager buffering from an untouched streaming body;
- `example-plugin`: route traffic must reach the configured override fixture; Lua module-load errors and `body_filter`/`delayed_body_filter` callbacks are recorded as non-portable observations;
- `request-id`: the real-process manifest proves format/uniqueness and a concurrent batch, while package tests prove 180 concurrent calls, UUIDv7 overflow refresh, and backwards-clock monotonicity;
- `server-info`: the manifest proves the control API, while `pkg/etcd` tests prove the prefixed registration key, configured TTL, payload write, lease reuse, and keepalive;
- `clickhouse-logger`: deterministic package testing proves both configured endpoints can be selected; the real-process manifest continues to prove delivery through the multi-endpoint configuration without a probabilistic assertion.

No production behavior change is planned. If a repaired test exposes a real implementation defect, stop that work unit and report the smallest required production change before expanding scope.

## Work units

### W1: encryption write path and stored-config immutability

Owned manifests:

- `t/plugin/kafka-proxy.yaml`
- `t/plugin/aws-lambda.yaml`
- `t/plugin/csrf.yaml`
- `t/plugin/openwhisk.yaml`
- `t/plugin/authz-casdoor.yaml`
- `t/plugin/authz-keycloak.yaml`
- `t/plugin/feishu-auth.yaml`
- `t/plugin/jwe-decrypt.yaml`
- `t/plugin/rocketmq-logger.yaml`
- `t/plugin/splunk-hec-logging.yaml`
- `t/plugin/echo.yaml`
- `t/plugin/proxy-rewrite.yaml`
- `t/plugin/response-rewrite.yaml`

For F003 and F006-F014, replace precomputed ciphertext input with the source plaintext, retain or strengthen runtime-provider assertions that see plaintext, and add typed `bbolt_json` assertions for deterministic ciphertext plus plaintext-forbidden matches. For F005, F020, and F021, add after-shutdown bbolt assertions showing the persisted route configuration still contains the original values after request handling.

Focused verification:

```bash
source .envrc && go test ./t/plugin -run 'TestPluginIntegration/(kafka-proxy|aws-lambda|csrf|openwhisk|authz-casdoor|authz-keycloak|feishu-auth|jwe-decrypt|rocketmq-logger|splunk-hec-logging|echo|proxy-rewrite|response-rewrite)(/|$)' -count=1
```

### W2: live lifecycle, server-info registration, and endpoint selection

Owned paths:

- `t/plugin/ua-restriction.yaml`
- `t/plugin/consumer-restriction.yaml`
- `t/plugin/ip-restriction.yaml`
- `t/plugin/server-info.yaml`
- `pkg/etcd/server_info_test.go`
- `pkg/plugin/clickhouse_logger/plugin_test.go`

For F018, F019, and F022, use step-level `config` updates against the same route ID and `config_probe` readiness checks. Exercise the denied state before update, then remove the plugin or change whitelist to blacklist and prove the changed state after reload. Do not use `_meta.disable` as removal evidence.

For F001, retain the process-level control API case and strengthen the fake etcd client test to record and assert the requested 60-second TTL as well as the existing key, payload, lease, and keepalive assertions. For F017, add a deterministic delivery-level package test that injects endpoint selection and proves requests reach each configured endpoint; avoid a probabilistic real-process assertion.

Focused verification:

```bash
source .envrc && go test ./t/plugin -run 'TestPluginIntegration/(ua-restriction|consumer-restriction|ip-restriction|server-info)(/|$)' -count=1
source .envrc && go test ./pkg/etcd ./pkg/plugin/clickhouse_logger -count=1
```

### W3: weakened behavioral boundaries

Owned paths:

- `t/plugin/proxy-control.yaml`
- `t/plugin/example-plugin.yaml`
- `t/plugin/request-id.yaml`
- `t/plugin/data-mask.yaml`

For F002, send the source-sized 51,200-byte request in both buffering modes and preserve exact upstream-body verification; rely on the existing route package tests for the Go-native buffering-versus-streaming distinction. For F004, route to a separate configured override fixture whose response cannot be produced by the default upstream. For F015, add a 180-request concurrent process step that checks every response while leaving cross-request uniqueness to the existing 180-goroutine package test; retain sequential manifest uniqueness/monotonic coverage and focused package tests for overflow/backwards-clock injection. For F016, enable `file-logger` with a request-line field and assert the post-request log file contains the masked URI and excludes both plaintext secrets.

Focused verification:

```bash
source .envrc && go test ./t/plugin -run 'TestPluginIntegration/(proxy-control|example-plugin|request-id|data-mask)(/|$)' -count=1
source .envrc && go test ./pkg/route ./pkg/plugin/request_id ./pkg/plugin/data_mask ./pkg/plugin/example_plugin -count=1
```

## Integration and acceptance

After all work units finish:

1. Inspect the complete diff and reject unrelated formatting or production changes.
2. Run the focused commands above for changed work units.
3. Run all 99 standalone plugin manifests through `go test ./t/plugin -run TestPluginIntegration -count=1`.
4. Run the repository gates:

   ```bash
   source .envrc && go test ./... -count=1
   source .envrc && make lint
   source .envrc && make build
   ```

5. Reconcile F001-F022 in `docs/testing/apisix-test-nginx-corpus-audit.md`, naming the manifest or package-test evidence and any Go-native disposition.
6. Confirm `git status --short` contains only this plan, the audit document, and the accepted test changes. Do not commit or push in this task.

## Stop conditions

- A manifest requires a new general-purpose harness feature rather than a bounded assertion.
- A focused test reveals a production behavior defect that cannot be fixed inside the work unit's owned paths.
- A deterministic ciphertext differs from the expected value for the pinned key/plaintext.
- A live reload cannot be observed with an existing `config_probe` without weakening the assertion.
- Any worker sees edits in an owned path that it did not make after dispatch.
