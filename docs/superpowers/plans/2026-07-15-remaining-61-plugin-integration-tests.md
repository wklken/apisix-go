# Remaining 61 Plugin Integration Tests Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the remaining 61 generated `t/plugin/*.yaml` placeholders with source-complete standalone integration scenarios that configure the named APISIX-Go plugin, start the real binary, send real requests, and assert plugin-produced behavior.

**Architecture:** Keep the strict manifest contract and one-child-process-per-scenario isolation introduced by PR #4. Extend only the test harness capabilities required by pinned Apache cases, then convert plugins in dependency-ordered waves; external systems are deterministic loopback fixtures, while every behavior assertion remains at the APISIX-Go boundary. A converted manifest is accepted only when every source number is mapped once, every case/variant activates its target plugin, its focused real-process integration run passes, and any exposed production defect has a focused RED-then-GREEN package test.

**Tech Stack:** Go 1.26.4, `go test`, `go.yaml.in/yaml/v3`, `net/http`, `net`, `httptest`, `os/exec`, standalone APISIX YAML, bbolt, existing repository dependencies, GitHub CLI.

## Global Constraints

- Work on `codex/plugin-integration-tests`; PR #4 is not ready while any item in the verified remaining-work ledger is unchecked.
- Do not use subagents unless the user explicitly authorizes them; execute this plan inline with `superpowers:executing-plans`.
- Run every Go command from Bash after `source .envrc`.
- Pin every source to Apache APISIX commit `c3d7d5ec69774121f53d2e20d29d09c816795dd7`.
- The remaining scope is exactly 61 plugin manifests, 205 source files, and 3,181 upstream `TEST` blocks at checkpoint `d8079fc`.
- Source setup blocks must be grouped with the behavior that consumes their routes, services, upstreams, consumers, consumer groups, plugin configs, global rules, metadata, certificates, or external-service state.
- Every executable case or variant must contain the target plugin under a standalone resource `plugins` map, or list the target control plugin in `runtime.plugins`.
- A disabled/removal case must retain the named plugin with APISIX `_meta.disable: true` when that is behaviorally equivalent; it may not become an unannotated generic route.
- Fixtures may replace external dependencies, never the target plugin response. Assert fixture requests for request-mutating, logger, tracing, cloud, and AI plugins.
- Preserve upstream methods, paths, query strings, headers, bodies, status codes, body/header/log expectations, repeats, waits, and state transitions. Do not weaken an assertion to match the current Go implementation.
- Invalid configurations must send a request to the rejected route, assert `404`, and match a route-build log containing the target plugin name.
- Fix production code only after a converted integration scenario reproduces a mismatch and a focused package test fails for the same reason.
- Do not add dependencies or run `make dep`; build fixtures from the standard library and existing modules.
- Commit after each task only when that task's focused package and integration gates pass. Use explicit `git add` paths; never `git add .` or `git add -A`.
- Final gates are `make test-integration`, `go test ./... -count=1`, `make lint`, `make build`, and `git diff --check`.

---

## File and Interface Map

- `t/plugin/case.go`: strict manifest types and validation. Extend only for binary/network fixtures, post-shutdown file assertions, and fixture timing.
- `t/plugin/case_test.go`: RED/GREEN contract tests for every new manifest field.
- `t/plugin/runner_test.go`: manifest discovery, process lifecycle, HTTP client, placeholder rendering, and assertion orchestration.
- `t/plugin/fixture_network_test.go`: TCP, UDP, HTTP/2/gRPC, and raw binary fixture listeners.
- `t/plugin/fixture_state_test.go`: deterministic RESP Redis, Redis Cluster redirect, and Sentinel scripts.
- `t/plugin/fixture_auth_test.go`: LDAP bind/search, OIDC/JWKS/introspection/token endpoints, CAS, SAML, and Wolf/Keycloak HTTP fixtures.
- `t/plugin/fixture_protocol_test.go`: Kafka broker and Dubbo response scripts using the repository's existing wire implementations.
- `t/plugin/coverage_test.go`: per-manifest/per-case target-plugin gate and final 98-plugin catalog gate.
- `t/plugin/<plugin>.yaml`: the only source-to-standalone behavior mapping for each plugin.
- `pkg/plugin/<package>/*.go`: modify only for a mismatch proven by a converted case; add the regression to the same package's `*_test.go`.
- `t/plugin/README.md`, `docs/plugins.md`, and `docs/superpowers/specs/2026-07-14-plugin-integration-tests-design.md`: update only after the live counts and complete gate are verified.

The new manifest fields have these exact types:

```go
type FixtureSpec struct {
    Name           string              `yaml:"name"`
    Kind           string              `yaml:"kind"`
    Expect         []HTTPAssertion     `yaml:"expect,omitempty"`
    Respond        []HTTPResponse      `yaml:"respond,omitempty"`
    NetworkExpect  []NetworkAssertion  `yaml:"network_expect,omitempty"`
    NetworkRespond []NetworkResponse   `yaml:"network_respond,omitempty"`
}

type NetworkAssertion struct {
    Payload       *Matcher `yaml:"payload,omitempty"`
    PayloadBase64 *Matcher `yaml:"payload_base64,omitempty"`
}

type NetworkResponse struct {
    Payload       string `yaml:"payload,omitempty"`
    PayloadBase64 string `yaml:"payload_base64,omitempty"`
    Close         bool   `yaml:"close,omitempty"`
    Delay         time.Duration `yaml:"delay,omitempty"`
}

type FileAssertion struct {
    Path *Matcher `yaml:"path"`
    Body *Matcher `yaml:"body"`
}

type Case struct {
    // existing fields stay unchanged
    AfterShutdown []FileAssertion `yaml:"after_shutdown,omitempty"`
}
```

`{{FIXTURE.<name>.ADDR}}`, `.HOST`, `.PORT`, and `.URL` continue to work. Add `{{WORK_DIR}}` for files created inside the scenario's temporary directory. Network fixture kinds are exactly `tcp`, `tls-tcp`, `udp`, `grpc`, `redis`, `redis-cluster`, `redis-sentinel`, `kafka`, `dubbo`, and `ldap`; unknown kinds fail manifest validation.

---

### Task 1: Make Each Remaining Manifest an Independently Runnable RED Gate

**Files:**
- Modify: `t/plugin/coverage_test.go`
- Test: `t/plugin/coverage_test.go`

**Interfaces:**
- Consumes: `scenarioExercisesPlugin(runtime, config, pluginName) bool` and all YAML manifests.
- Produces: `TestManifestCorpusExercisesTargetPlugins/<plugin>` subtests addressable with `go test -run`.

- [x] **Step 1: Refactor the existing corpus loop into named subtests without changing its assertions**

```go
for _, file := range files {
    file := file
    pluginName := manifestPluginName(file)
    t.Run(pluginName, func(t *testing.T) {
        manifest := mustLoadManifest(t, file)
        assertManifestExercisesTargetPlugin(t, file, manifest, pluginName)
    })
}
```

Keep the current manifest-level and every-case/every-variant checks inside `assertManifestExercisesTargetPlugin`; map `redirect2` to `redirect` in `manifestPluginName`.

- [x] **Step 2: Run one known converted and one remaining manifest**

Run:

```bash
bash -lc 'source .envrc && go test ./t/plugin -run "^TestManifestCorpusExercisesTargetPlugins/acl$" -count=1'
bash -lc 'source .envrc && go test ./t/plugin -run "^TestManifestCorpusExercisesTargetPlugins/brotli$" -count=1'
```

Expected: `acl` PASS; `brotli` FAIL with `brotli.yaml never activates target plugin "brotli"`.

- [x] **Step 3: Commit the independently targetable gate**

```bash
git add t/plugin/coverage_test.go
git commit -m "test(plugin): isolate semantic manifest coverage gates"
```

---

### Task 2: Add Network, Binary, and Shutdown Fixture Primitives

**Files:**
- Modify: `t/plugin/case.go`
- Modify: `t/plugin/case_test.go`
- Modify: `t/plugin/runner_test.go`
- Create: `t/plugin/fixture_network_test.go`

**Interfaces:**
- Consumes: the exact `NetworkAssertion`, `NetworkResponse`, and `FileAssertion` types in the file map.
- Produces: TCP/UDP/raw-binary fixtures, HTTP/2 gRPC fixtures, `{{WORK_DIR}}`, and assertions evaluated after graceful child shutdown.

- [x] **Step 1: Add strict-decoding tests for network fixtures**

```go
func TestManifestAcceptsTCPFixture(t *testing.T) {
    manifest := validManifestWithFixture(FixtureSpec{
        Name: "sink", Kind: "tcp",
        NetworkExpect: []NetworkAssertion{{Payload: equalsMatcher("hello")}},
        NetworkRespond: []NetworkResponse{{Payload: "ok"}},
    })
    requireManifestValid(t, manifest)
}

func TestManifestRejectsMixedHTTPAndNetworkFixtureFields(t *testing.T) {
    fixture := FixtureSpec{Name: "sink", Kind: "tcp", Respond: []HTTPResponse{{Status: 200}}}
    requireManifestError(t, fixture, "tcp fixture must use network_expect/network_respond")
}
```

Also test: UDP cannot respond with `close`; exactly one of text/base64 payload is allowed; `after_shutdown.path` must begin with `{{WORK_DIR}}/`; unknown fixture kinds fail.

- [x] **Step 2: Run the new contract tests RED**

```bash
bash -lc 'source .envrc && go test ./t/plugin -run "TestManifest(AcceptsTCPFixture|RejectsMixedHTTPAndNetworkFixtureFields|RejectsUnsafeFileAssertion)" -count=1'
```

Expected: compile failure because the new fields/types do not exist.

- [x] **Step 3: Implement TCP and UDP listeners**

```go
type networkFixture struct {
    listener net.Listener
    packet   net.PacketConn
    received chan []byte
    done     chan struct{}
}

func startTCPFixture(spec FixtureSpec) (*networkFixture, error)
func startUDPFixture(spec FixtureSpec) (*networkFixture, error)
func (f *networkFixture) address() string
func (f *networkFixture) close() error
```

TCP accepts connections serially, reads one scripted payload per `network_expect`, writes the matching response after its delay, and honors `close`. UDP reads one datagram per expectation and writes the matching response to the sender. Match base64 against `base64.StdEncoding.EncodeToString(payload)`.

- [x] **Step 4: Implement the gRPC fixture and post-shutdown assertions**

Use `httptest.NewUnstartedServer`, set `EnableHTTP2 = true`, call `StartTLS`, and capture the five-byte gRPC frame header plus payload and trailers. Render `{{WORK_DIR}}` only inside runtime/config/file-assertion values. Stop APISIX before evaluating `after_shutdown`, then read each matched path and apply its body matcher.

- [x] **Step 5: Run harness tests GREEN**

```bash
bash -lc 'source .envrc && go test ./t/plugin -run "TestHarness(RunsTCPFixture|RunsUDPFixture|RunsGRPCFixture|AssertsFileAfterShutdown)" -count=1 -v'
```

Expected: all four PASS and no leaked listener or child process.

- [x] **Step 6: Commit fixture primitives**

```bash
git add t/plugin/case.go t/plugin/case_test.go t/plugin/runner_test.go t/plugin/fixture_network_test.go
git commit -m "test(plugin): add network and shutdown fixtures"
```

---

### Task 3: Add Stateful Redis, Kafka, Dubbo, and LDAP Fixtures

**Files:**
- Create: `t/plugin/fixture_state_test.go`
- Create: `t/plugin/fixture_protocol_test.go`
- Create: `t/plugin/fixture_auth_test.go`
- Modify: `t/plugin/runner_test.go`
- Test: the three new fixture files

**Interfaces:**
- Consumes: `FixtureSpec.Kind`, network payload scripts, and named fixture placeholders.
- Produces: deterministic protocol servers used by limit/cache, logger, Dubbo, and LDAP manifests.

- [ ] **Step 1: Write RED tests for the exact commands/protocols used by the remaining sources**

```go
func TestRedisFixtureSupportsPluginCommands(t *testing.T) {
    client := redis.NewClient(&redis.Options{Addr: fixture.address()})
    require.NoError(t, client.Set(ctx, "quota", "1", time.Minute).Err())
    require.Equal(t, int64(2), client.Incr(ctx, "quota").Val())
    require.Equal(t, "1", client.HGet(ctx, "hash", "field").Val())
}
```

Use the repository's existing Redis client import, not a new package. Add protocol tests for `AUTH`, `SELECT`, `GET`, `SET` with `NX/PX/EX`, `INCR`, `INCRBY`, `DECR`, `EXPIRE`, `PTTL`, `DEL`, `HGET/HSET`, `EVAL/EVALSHA`, cluster `MOVED`, Sentinel `get-master-addr-by-name`, Kafka metadata/produce acknowledgements, Dubbo request ID and Hessian response frames, and LDAP bind/search success/failure.

- [ ] **Step 2: Run protocol tests RED**

```bash
bash -lc 'source .envrc && go test ./t/plugin -run "Test(Redis|Kafka|Dubbo|LDAP)Fixture" -count=1 -v'
```

Expected: compile failure because fixture constructors are undefined.

- [ ] **Step 3: Implement only the scripted protocol surface asserted in Step 1**

```go
func startRedisFixture(spec FixtureSpec) (*networkFixture, error)
func startRedisClusterFixture(spec FixtureSpec) (*networkFixture, error)
func startRedisSentinelFixture(spec FixtureSpec) (*networkFixture, error)
func startKafkaFixture(spec FixtureSpec) (*networkFixture, error)
func startDubboFixture(spec FixtureSpec) (*networkFixture, error)
func startLDAPFixture(spec FixtureSpec) (*networkFixture, error)
```

Keep state in per-fixture maps guarded by a mutex. Do not implement a general server: reject any command/API key not listed in Step 1 and include the unexpected payload in the test failure.

- [ ] **Step 4: Run all protocol fixtures GREEN and commit**

```bash
bash -lc 'source .envrc && go test ./t/plugin -run "Test(Redis|Kafka|Dubbo|LDAP)Fixture" -count=1 -v'
git add t/plugin/fixture_state_test.go t/plugin/fixture_protocol_test.go t/plugin/fixture_auth_test.go t/plugin/runner_test.go
git commit -m "test(plugin): add deterministic service fixtures"
```

---

### Task 4: Convert Core HTTP, Security, and Validation Plugins

**Files:**
- Modify: `t/plugin/brotli.yaml`
- Modify: `t/plugin/fault-injection.yaml`
- Modify: `t/plugin/cors.yaml`
- Modify: `t/plugin/consumer-restriction.yaml`
- Modify: `t/plugin/request-validation.yaml`
- Modify: `t/plugin/oas-validator.yaml`
- Modify: `t/plugin/traffic-label.yaml`
- Modify only on reproduced mismatch: `pkg/plugin/{brotli,fault_injection,cors,consumer_restriction,request_validation,oas_validator,traffic_label}/*.go`
- Test: matching package `*_test.go` files

**Interfaces:**
- Consumes: HTTP/HTTPS named fixtures, repeated steps, body/header/log matchers, and per-plugin semantic subtests.
- Produces: 7 real manifests covering 16 source files and 443 source blocks.

The conversion acceptance matrix is exact:

| Manifest | Sources / blocks | Required behavior groups |
|---|---:|---|
| `brotli` | `brotli.t` / 37 | schema/defaults, `types` array and `"*"`, min length, quality, HTTP version, `Accept-Encoding` negotiation/q-values, `Vary`, already encoded responses, content type, empty/small body |
| `fault-injection` | `fault-injection.t` 39 + `fault-injection2.t` 7 | invalid abort/delay schema, percentage 0/100, fractional delays, status/body/headers, variable expansion, wrapped/nested vars, abort+delay ordering, redirect interaction |
| `cors` | four sources / 86 | simple/preflight requests, wildcard/regex origins, methods, request/expose headers, credentials, max-age, allow-private-network, missing/disallowed origins, schema rejection |
| `consumer-restriction` | two sources / 71 | username/group/service/route allow/deny, anonymous behavior, consumer groups, plugin metadata, custom message/status, missing consumer |
| `request-validation` | two sources / 55 | JSON/body/form/header/query validation, coercion, required/additional fields, arrays/nested objects, custom rejection status/message, malformed payloads |
| `oas-validator` | three sources / 110 | inline and referenced OpenAPI operations, path/query/header/cookie/body validation, formats, nullable/composition, request/response modes, unmatched operations, schema errors |
| `traffic-label` | two sources / 38 | first-match and match-all rules, nested expressions, variables, numeric/string headers, weighted actions, schema/config-time expression rejection |

- [ ] **Step 1: Convert one manifest at a time using the canonical shape**

```yaml
cases:
  - name: <behavior-group>
    source:
      file: <exact-source-when-multiple>
      tests: [<setup-and-behavior-numbers>]
    runtime:
      plugins: [<target>, <required-auth-or-helper-plugins>]
    config:
      routes:
        - id: <target>-<behavior>
          uri: <source-uri>
          plugins:
            <target>: <source-config>
          upstream:
            type: roundrobin
            nodes:
              "{{FIXTURE.primary.ADDR}}": 1
    fixtures:
      - name: primary
        kind: http
        expect: [<request assertions when behavior reaches upstream>]
        respond: [<source upstream response>]
    steps: [<source requests and target-produced assertions>]
```

Do not put setup-only requests in `steps`; include their source numbers in the case that owns the resulting standalone resource.

- [ ] **Step 2: For each plugin, prove RED then GREEN**

Run before editing and after conversion, replacing `<plugin>` with each table row:

```bash
bash -lc 'source .envrc && go test ./t/plugin -run "^TestManifestCorpusExercisesTargetPlugins/<plugin>$" -count=1'
bash -lc 'source .envrc && go test ./t/plugin -run "TestPluginIntegration/<plugin>" -count=1 -v'
```

Expected before: semantic gate FAIL. Expected after: semantic gate and integration PASS. If integration exposes a mismatch, add `Test<Behavior>` in the matching package, observe RED, make the smallest production fix, and rerun package plus manifest.

- [ ] **Step 3: Run the wave gate and commit**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/brotli ./pkg/plugin/fault_injection ./pkg/plugin/cors ./pkg/plugin/consumer_restriction ./pkg/plugin/request_validation ./pkg/plugin/oas_validator ./pkg/plugin/traffic_label -count=1'
bash -lc 'source .envrc && go test ./t/plugin -run "TestPluginIntegration/(brotli|fault-injection|cors|consumer-restriction|request-validation|oas-validator|traffic-label)" -count=1'
git add t/plugin/brotli.yaml t/plugin/fault-injection.yaml t/plugin/cors.yaml t/plugin/consumer-restriction.yaml t/plugin/request-validation.yaml t/plugin/oas-validator.yaml t/plugin/traffic-label.yaml pkg/plugin/brotli pkg/plugin/fault_injection pkg/plugin/cors pkg/plugin/consumer_restriction pkg/plugin/request_validation pkg/plugin/oas_validator pkg/plugin/traffic_label
git commit -m "test(plugin): convert core security integration suites"
```

---

### Task 5: Convert Local-Credential Authentication Plugins

**Files:**
- Modify: `t/plugin/key-auth.yaml`, `basic-auth.yaml`, `jwt-auth.yaml`, `hmac-auth.yaml`, `jwe-decrypt.yaml`
- Modify only on RED mismatch: `pkg/plugin/{key_auth,basic_auth,jwt_auth,hmac_auth,jwe_decrypt}/*.go`
- Test: matching package tests

**Interfaces:**
- Consumes: standalone consumers/groups, repeated headers, cookie reuse, response capture, and crypto helpers already present in package tests.
- Produces: 5 real manifests covering 21 sources and 325 blocks.

- [ ] **Step 1: Translate the exact behavior sets**

| Manifest | Blocks | Required behavior groups |
|---|---:|---|
| `key-auth` | 58 | header/query keys, hide credentials, environment/Vault secret references, anonymous consumer with limiter chaining, realm, service inheritance, and domain-node/upstream-resource behavior across four pinned sources |
| `basic-auth` | 44 | Basic parsing, malformed base64, username/password lookup, anonymous consumer, realm, duplicate headers, consumer/group attachment |
| `jwt-auth` | 130 | token issue endpoint, header/query/cookie extraction, HS/RS/ES algorithms, exp/nbf/leeway, base64 secret, public keys, key claims, anonymous/realm, hide credentials |
| `hmac-auth` | 70 | consumer/route schema validation, canonical signatures and allowed algorithms, clock skew/default-date/replay behavior, signed-header cardinality, body digest and size limits, hide credentials, anonymous limiter chains, realm, and Vault/environment secrets |
| `jwe-decrypt` | 23 | compact JWE extraction, protected headers, supported algorithms/encodings, key selection, header forwarding, malformed/decryption failures |

Use test-local fixed keys copied from the pinned sources or existing package test fixtures; never generate assertions from the implementation under test.

- [ ] **Step 2: Run each semantic and real-process gate RED then GREEN**

```bash
for plugin in key-auth basic-auth jwt-auth hmac-auth jwe-decrypt; do
  bash -lc "source .envrc && go test ./t/plugin -run '^TestManifestCorpusExercisesTargetPlugins/'\"$plugin\"'$' -count=1"
  bash -lc "source .envrc && go test ./t/plugin -run 'TestPluginIntegration/'\"$plugin\" -count=1 -v"
done
```

- [ ] **Step 3: Run packages and commit**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/key_auth ./pkg/plugin/basic_auth ./pkg/plugin/jwt_auth ./pkg/plugin/hmac_auth ./pkg/plugin/jwe_decrypt -count=1'
git add t/plugin/key-auth.yaml t/plugin/basic-auth.yaml t/plugin/jwt-auth.yaml t/plugin/hmac-auth.yaml t/plugin/jwe-decrypt.yaml pkg/plugin/key_auth pkg/plugin/basic_auth pkg/plugin/jwt_auth pkg/plugin/hmac_auth pkg/plugin/jwe_decrypt
git commit -m "test(plugin): convert credential authentication suites"
```

---

### Task 6: Convert External Authentication and Authorization Plugins

**Files:**
- Modify: `t/plugin/ldap-auth.yaml`, `openid-connect.yaml`, `forward-auth.yaml`, `multi-auth.yaml`, `wolf-rbac.yaml`, `authz-keycloak.yaml`, `cas-auth.yaml`, `saml-auth.yaml`, `feishu-auth.yaml`, `authz-casbin.yaml`
- Modify: `t/plugin/fixture_auth_test.go`
- Modify only on RED mismatch: matching packages under `pkg/plugin/`

**Interfaces:**
- Consumes: LDAP fixture, HTTP/HTTPS fixtures, frontend TLS, cookies/captures, RSA/JWKS helpers, and standalone consumers/metadata.
- Produces: 10 real manifests covering 32 sources and 411 blocks.

- [ ] **Step 1: Extend `fixture_auth_test.go` with deterministic endpoint scripts**

Provide named modes selected by fixture response sequences, not plugin-specific shortcuts: LDAP bind/search; OIDC discovery/JWKS/introspection/authorize/token/userinfo/revoke/end-session; forward-auth allow/deny with copied headers; Keycloak/Wolf permission endpoints; CAS service validation XML; SAML metadata/login/ACS/logout payloads; Feishu token/user endpoints. Every fixture request must assert method, path, authorization, form/query/body, and TLS choice from the pinned source.

- [ ] **Step 2: Convert and verify the exact behavior sets**

| Manifest | Blocks | Required behavior groups |
|---|---:|---|
| `ldap-auth` | 35 | bind/search, consumer DN mapping, Basic realm, TLS/schema and auth failures |
| `openid-connect` | 141 | bearer/introspection/JWT, discovery/JWKS, client auth modes, scopes/claims, session+PKCE, Redis session, renewal/logout/revocation, proxy/TLS/header behavior |
| `forward-auth` | 28 | schema validation, request/generated-header forwarding and spoof resistance, allow/deny response propagation, GET/POST auth body framing, transport degradation/status, extra headers and CRLF rejection, bounded-body 413 behavior, `$post_arg` variables, and clearing absent auth headers |
| `multi-auth` | 38 | ordered auth alternatives, consumer propagation, anonymous behavior, schema and failure precedence |
| `wolf-rbac` | 44 | schema/defaults, public login/user/password APIs, token locations, permission and retry/error mapping, consumer replacement/chaining, user headers, Vault/environment appid, TLS verification, trusted client IP, and HTTP security warnings across `wolf-rbac.t` plus `security-warning2.t` tests 19–20 |
| `authz-keycloak` | 45 | discovery/token/UMA decisions, lazy paths, permissions, client credentials, timeout/TLS/error handling |
| `cas-auth` | 24 | login/callback/service validation, original-URI and session cookies, multi-SP/session isolation, redirect/signature safety, callback derivation and forged Host, local and single logout, schema failures, and HTTP security warnings across `cas-auth.t` plus `security-warning.t` tests 5–6 |
| `saml-auth` | 21 | metadata, AuthnRequest redirect/form, signed response ACS, relay state, session cookie, logout and invalid assertions |
| `feishu-auth` | 14 | authorize redirect, callback token/user lookup, state/cookie, headers and failure branches |
| `authz-casbin` | 21 | model/policy inline and metadata resources, request variable mapping, allow/deny and invalid model/policy |

Run the same two per-plugin commands from Task 4. `openid-connect`, `saml-auth`, and `cas-auth` must use captured state/cookies across ordered steps; no precomputed successful response may bypass the target plugin.

- [ ] **Step 3: Run wave packages and commit**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/ldap_auth ./pkg/plugin/openid_connect ./pkg/plugin/forward_auth ./pkg/plugin/multi_auth ./pkg/plugin/wolf_rbac ./pkg/plugin/authz_keycloak ./pkg/plugin/cas_auth ./pkg/plugin/saml_auth ./pkg/plugin/feishu_auth ./pkg/plugin/authz_casbin -count=1'
git add t/plugin/fixture_auth_test.go t/plugin/ldap-auth.yaml t/plugin/openid-connect.yaml t/plugin/forward-auth.yaml t/plugin/multi-auth.yaml t/plugin/wolf-rbac.yaml t/plugin/authz-keycloak.yaml t/plugin/cas-auth.yaml t/plugin/saml-auth.yaml t/plugin/feishu-auth.yaml t/plugin/authz-casbin.yaml pkg/plugin/ldap_auth pkg/plugin/openid_connect pkg/plugin/forward_auth pkg/plugin/multi_auth pkg/plugin/wolf_rbac pkg/plugin/authz_keycloak pkg/plugin/cas_auth pkg/plugin/saml_auth pkg/plugin/feishu_auth pkg/plugin/authz_casbin
git commit -m "test(plugin): convert external auth integration suites"
```

---

### Task 7: Convert Limits and Cache Plugins

**Files:**
- Modify: `t/plugin/limit-conn.yaml`, `limit-count.yaml`, `limit-req.yaml`, `graphql-limit-count.yaml`, `proxy-cache.yaml`, `graphql-proxy-cache.yaml`
- Modify: `t/plugin/fixture_state_test.go`
- Modify only on RED mismatch: matching packages plus `pkg/plugin/proxy_cache` shared storage code

**Interfaces:**
- Consumes: local/cluster/sentinel Redis fixtures, repeated steps, explicit waits, temporary disk zones, and post-shutdown files.
- Produces: 6 real manifests covering 39 sources and 595 blocks.

- [ ] **Step 1: Convert stateful sequences without restarting their APISIX process**

| Manifest | Blocks | State that must remain within one case |
|---|---:|---|
| `limit-conn` | 104 | concurrent/in-flight counters, delays, local/Redis/cluster, variables and rejection headers |
| `limit-count` | 252 | fixed/sliding windows, local/Redis/cluster/sentinel, rules/groups, consumer isolation, delayed sync, metadata headers |
| `limit-req` | 89 | leaky bucket burst/delay/nodelay, shared counters, Redis/cluster, variables and rejection behavior |
| `graphql-limit-count` | 26 | GraphQL cost/depth, fragments, local/Redis/cluster quotas and schema rejection |
| `proxy-cache` | 76 | memory/disk MISS/HIT/EXPIRED/BYPASS, keys, methods/status/TTL, Vary, cache-control, Set-Cookie, purge and persistence |
| `graphql-proxy-cache` | 48 | memory/disk GraphQL keys, POST bodies, Vary/purge, shared zones and invalid zone configuration |

Use `repeat`, `wait`, and ordered steps for source windows; do not split a counter/cache lifecycle into variants. Render disk paths as `{{WORK_DIR}}/cache/<zone>`.

- [ ] **Step 2: Run per-plugin RED/GREEN gates and focused package tests**

Use Task 4's two commands for each plugin. Run concurrency-sensitive packages with race detection:

```bash
bash -lc 'source .envrc && go test -race ./pkg/plugin/limit_conn ./pkg/plugin/limit_count ./pkg/plugin/limit_req ./pkg/plugin/graphql_limit_count ./pkg/plugin/proxy_cache ./pkg/plugin/graphql_proxy_cache -count=1'
```

- [ ] **Step 3: Commit the stateful wave**

```bash
git add t/plugin/fixture_state_test.go t/plugin/limit-conn.yaml t/plugin/limit-count.yaml t/plugin/limit-req.yaml t/plugin/graphql-limit-count.yaml t/plugin/proxy-cache.yaml t/plugin/graphql-proxy-cache.yaml pkg/plugin/limit_conn pkg/plugin/limit_count pkg/plugin/limit_req pkg/plugin/graphql_limit_count pkg/plugin/proxy_cache pkg/plugin/graphql_proxy_cache
git commit -m "test(plugin): convert limit and cache integration suites"
```

---

### Task 8: Convert Routing, Workflow, Batch, and Dubbo Plugins

**Files:**
- Modify: `t/plugin/proxy-mirror.yaml`, `traffic-split.yaml`, `workflow.yaml`, `batch-requests.yaml`, `http-dubbo.yaml`
- Modify: `t/plugin/fixture_protocol_test.go`
- Modify only on RED mismatch: matching plugin packages, `pkg/route`, or `pkg/proxy`

**Interfaces:**
- Consumes: multiple named HTTP fixtures, gRPC fixture, Dubbo fixture, repeated requests, and fixture request-count assertions.
- Produces: 5 real manifests covering 16 sources and 223 blocks.

- [ ] **Step 1: Convert the required behavior groups**

| Manifest | Blocks | Required behavior groups |
|---|---:|---|
| `proxy-mirror` | 36 | host/scheme/path/ratio schema validation, exact Host/header/query/body delivery, HTTP/1.1, live deletion, concurrent sampling bounds, replacement/prefix paths, DNS failure independence, proxy-rewrite ordering, and h2c gRPC/grpc-web mirroring |
| `traffic-split` | 94 | ordered match vars, weighted inline/resource upstreams, fallback, zero weights, chash keys, pass-host and timeout propagation |
| `workflow` | 42 | no-case behavior, ordered rules, nested vars, plugin config execution, skip/break semantics and invalid expressions |
| `batch-requests` | 46 | HTTP batch parsing, per-entry method/path/headers/body, response aggregation, limits/failures, gRPC entries |
| `http-dubbo` | 5 | serialized POJO/array request frames, timeout/connect failure, void response, application failure status |

For traffic splitting, send enough deterministic requests to prove every explicit 0/100 branch; do not assert probabilistic ratios. For mirroring, assert both primary client response and mirror fixture capture.

- [ ] **Step 2: Run focused integrations and packages**

```bash
bash -lc 'source .envrc && go test ./t/plugin -run "TestPluginIntegration/(proxy-mirror|traffic-split|workflow|batch-requests|http-dubbo)" -count=1 -v'
bash -lc 'source .envrc && go test ./pkg/plugin/proxy_mirror ./pkg/plugin/traffic_split ./pkg/plugin/workflow ./pkg/plugin/batch_requests ./pkg/plugin/http_dubbo ./pkg/route ./pkg/proxy -count=1'
```

- [ ] **Step 3: Commit**

```bash
git add t/plugin/proxy-mirror.yaml t/plugin/traffic-split.yaml t/plugin/workflow.yaml t/plugin/batch-requests.yaml t/plugin/http-dubbo.yaml t/plugin/fixture_protocol_test.go pkg/plugin/proxy_mirror pkg/plugin/traffic_split pkg/plugin/workflow pkg/plugin/batch_requests pkg/plugin/http_dubbo pkg/route pkg/proxy
git commit -m "test(plugin): convert routing and protocol suites"
```

---

### Task 9: Convert HTTP and Cloud Logger Plugins

**Files:**
- Modify: `t/plugin/http-logger.yaml`, `clickhouse-logger.yaml`, `google-cloud-logging.yaml`, `loggly.yaml`, `loki-logger.yaml`, `datadog.yaml`, `elasticsearch-logger.yaml`, `rocketmq-logger.yaml`, `sls-logger.yaml`, `splunk-hec-logging.yaml`, `tencent-cloud-cls.yaml`
- Modify only on RED mismatch: matching logger packages and `pkg/plugin/logger_batch`

**Interfaces:**
- Consumes: HTTP/HTTPS fixtures, repeated requests, waits, request-body matchers, fixed cloud credentials, and shutdown flush.
- Produces: 11 real manifests covering 25 sources and 352 blocks.

- [ ] **Step 1: Convert every delivery lifecycle**

Each manifest must cover its source schema, log-format variables, request/response body truncation, batch size, inactive timeout, retry/status handling, authentication/signature headers, endpoint URI, TLS verification, and shutdown flush. Use fixed timestamps/credentials from upstream when signatures are asserted. Fixture bodies use regex only for nondeterministic timestamps/IDs; assert all stable JSON fields and batch cardinality.

- [ ] **Step 2: Run per-plugin semantic/integration gates and logger packages**

```bash
for plugin in http-logger clickhouse-logger google-cloud-logging loggly loki-logger datadog elasticsearch-logger rocketmq-logger sls-logger splunk-hec-logging tencent-cloud-cls; do
  bash -lc "source .envrc && go test ./t/plugin -run 'TestPluginIntegration/'\"$plugin\" -count=1 -v"
done
bash -lc 'source .envrc && go test ./pkg/plugin/http_logger ./pkg/plugin/clickhouse_logger ./pkg/plugin/google_cloud_logging ./pkg/plugin/loggly ./pkg/plugin/loki_logger ./pkg/plugin/datadog ./pkg/plugin/elasticsearch_logger ./pkg/plugin/rocketmq_logger ./pkg/plugin/sls_logger ./pkg/plugin/splunk_hec_logging ./pkg/plugin/tencent_cloud_cls ./pkg/plugin/logger_batch -count=1'
```

- [ ] **Step 3: Commit**

```bash
git add t/plugin/http-logger.yaml t/plugin/clickhouse-logger.yaml t/plugin/google-cloud-logging.yaml t/plugin/loggly.yaml t/plugin/loki-logger.yaml t/plugin/datadog.yaml t/plugin/elasticsearch-logger.yaml t/plugin/rocketmq-logger.yaml t/plugin/sls-logger.yaml t/plugin/splunk-hec-logging.yaml t/plugin/tencent-cloud-cls.yaml pkg/plugin/http_logger pkg/plugin/clickhouse_logger pkg/plugin/google_cloud_logging pkg/plugin/loggly pkg/plugin/loki_logger pkg/plugin/datadog pkg/plugin/elasticsearch_logger pkg/plugin/rocketmq_logger pkg/plugin/sls_logger pkg/plugin/splunk_hec_logging pkg/plugin/tencent_cloud_cls pkg/plugin/logger_batch
git commit -m "test(plugin): convert HTTP logger integration suites"
```

---

### Task 10: Convert Network, Kafka, File, and Error Logger Plugins

**Files:**
- Modify: `t/plugin/tcp-logger.yaml`, `udp-logger.yaml`, `syslog.yaml`, `kafka-logger.yaml`, `file-logger.yaml`, `log-rotate.yaml`, `error-log-logger.yaml`, `skywalking-logger.yaml`
- Modify: `t/plugin/fixture_network_test.go`, `t/plugin/fixture_protocol_test.go`
- Modify only on RED mismatch: matching logger packages

**Interfaces:**
- Consumes: TCP/UDP/Kafka fixtures, `{{WORK_DIR}}`, post-shutdown file assertions, batching/retry waits, and real child logs.
- Produces: 8 real manifests covering 21 sources and 266 blocks.

- [ ] **Step 1: Convert transport and filesystem assertions**

Cover schema, JSON/custom formats, batch framing/newlines, body inclusion/truncation, inactive flush, reconnect/retry, TLS where present, Kafka topic/key/partition/SASL behavior, file append/reopen, rotation count/size/time, error-log source levels, and SkyWalking log envelope. Paths must remain under `{{WORK_DIR}}`; assert file content only after APISIX shutdown.

- [ ] **Step 2: Run integrations and package tests**

```bash
bash -lc 'source .envrc && go test ./t/plugin -run "TestPluginIntegration/(tcp-logger|udp-logger|syslog|kafka-logger|file-logger|log-rotate|error-log-logger|skywalking-logger)" -count=1 -v'
bash -lc 'source .envrc && go test ./pkg/plugin/tcp_logger ./pkg/plugin/udp_logger ./pkg/plugin/syslog ./pkg/plugin/kafka_logger ./pkg/plugin/file_logger ./pkg/plugin/log_rotate ./pkg/plugin/error_log_logger ./pkg/plugin/skywalking_logger -count=1'
```

- [ ] **Step 3: Commit**

```bash
git add t/plugin/tcp-logger.yaml t/plugin/udp-logger.yaml t/plugin/syslog.yaml t/plugin/kafka-logger.yaml t/plugin/file-logger.yaml t/plugin/log-rotate.yaml t/plugin/error-log-logger.yaml t/plugin/skywalking-logger.yaml t/plugin/fixture_network_test.go t/plugin/fixture_protocol_test.go pkg/plugin/tcp_logger pkg/plugin/udp_logger pkg/plugin/syslog pkg/plugin/kafka_logger pkg/plugin/file_logger pkg/plugin/log_rotate pkg/plugin/error_log_logger pkg/plugin/skywalking_logger
git commit -m "test(plugin): convert network and file logger suites"
```

---

### Task 11: Convert Tracing Plugins

**Files:**
- Modify: `t/plugin/opentelemetry.yaml`, `t/plugin/skywalking.yaml`
- Modify only on RED mismatch: `pkg/plugin/otel/*.go`, `pkg/plugin/skywalking/*.go`, and tracing helpers

**Interfaces:**
- Consumes: HTTP/gRPC collectors, captured binary protobuf bodies, header propagation, repeated spans, and shutdown flush.
- Produces: 2 real manifests covering 10 sources and 113 blocks.

- [ ] **Step 1: Convert OpenTelemetry and SkyWalking behavior**

Cover schema, trace/span IDs, sampling, route/service/resource attributes, W3C/B3/SkyWalking propagation, upstream headers, collector HTTP/gRPC export, batching, error status, plugin metadata, body capture limits, and shutdown delivery. Decode protobuf with existing repository protobuf types in the fixture before asserting semantic fields; do not compare nondeterministic serialized bytes.

- [ ] **Step 2: Run integrations and package tests**

```bash
bash -lc 'source .envrc && go test ./t/plugin -run "TestPluginIntegration/(opentelemetry|skywalking)" -count=1 -v'
bash -lc 'source .envrc && go test ./pkg/plugin/otel ./pkg/plugin/skywalking -count=1'
```

- [ ] **Step 3: Commit**

```bash
git add t/plugin/opentelemetry.yaml t/plugin/skywalking.yaml pkg/plugin/otel pkg/plugin/skywalking
git commit -m "test(plugin): convert tracing integration suites"
```

---

### Task 12: Convert Bounded AI Plugins Except `ai-proxy`

**Files:**
- Modify: `t/plugin/ai-aws-content-moderation.yaml`, `ai-prompt-decorator.yaml`, `ai-prompt-guard.yaml`, `ai-rag.yaml`, `ai-rate-limiting.yaml`, `ai-request-rewrite.yaml`
- Modify only on RED mismatch: matching AI packages and `pkg/plugin/ai_runtime`

**Interfaces:**
- Consumes: HTTP/HTTPS/SSE fixtures, fixed AWS credentials, request/response JSON assertions, consumer state, counters, and expression evaluation.
- Produces: 6 real manifests covering 11 sources and 178 blocks.

- [ ] **Step 1: Convert exact AI behavior sets**

| Manifest | Blocks | Required behavior groups |
|---|---:|---|
| `ai-aws-content-moderation` | 23 | credentials/encryption, SigV4, endpoint/TLS, category/toxicity thresholds, request replay and rejection |
| `ai-prompt-decorator` | 17 | prepend/append system/user messages, provider body shapes, streaming preservation and schema errors |
| `ai-prompt-guard` | 44 | allow/deny patterns, case handling, message roles, custom rejection, streaming and malformed requests |
| `ai-rag` | 17 | embedding/retrieval fixtures, prompt/context construction, headers, failures and streaming |
| `ai-rate-limiting` | 58 | token extraction/estimation, local counters, consumer isolation, expressions, headers, windows and rejection |
| `ai-request-rewrite` | 19 | prompt/message rewriting, variables, provider formats, body preservation and invalid JSON/schema |

- [ ] **Step 2: Run real-process and package gates, then commit**

```bash
bash -lc 'source .envrc && go test ./t/plugin -run "TestPluginIntegration/(ai-aws-content-moderation|ai-prompt-decorator|ai-prompt-guard|ai-rag|ai-rate-limiting|ai-request-rewrite)" -count=1 -v'
bash -lc 'source .envrc && go test ./pkg/plugin/ai_aws_content_moderation ./pkg/plugin/ai_prompt_decorator ./pkg/plugin/ai_prompt_guard ./pkg/plugin/ai_rag ./pkg/plugin/ai_rate_limiting ./pkg/plugin/ai_request_rewrite ./pkg/plugin/ai_runtime -count=1'
git add t/plugin/ai-aws-content-moderation.yaml t/plugin/ai-prompt-decorator.yaml t/plugin/ai-prompt-guard.yaml t/plugin/ai-rag.yaml t/plugin/ai-rate-limiting.yaml t/plugin/ai-request-rewrite.yaml pkg/plugin/ai_aws_content_moderation pkg/plugin/ai_prompt_decorator pkg/plugin/ai_prompt_guard pkg/plugin/ai_rag pkg/plugin/ai_rate_limiting pkg/plugin/ai_request_rewrite pkg/plugin/ai_runtime
git commit -m "test(plugin): convert bounded AI integration suites"
```

---

### Task 13: Convert the `ai-proxy` Provider and Streaming Matrix

**Files:**
- Modify: `t/plugin/ai-proxy.yaml`
- Modify: `t/plugin/fixture_network_test.go`
- Modify only on RED mismatch: `pkg/plugin/ai_proxy/*.go`, `pkg/plugin/ai_runtime/*.go`

**Interfaces:**
- Consumes: HTTP/HTTPS/SSE and AWS EventStream fixtures, chunked responses, client-disconnect support, repeated headers, fixed provider credentials, and response/log assertions.
- Produces: one real manifest covering 19 sources and 303 blocks.

- [ ] **Step 1: Add the remaining streaming primitives with RED harness tests**

Add `HTTPInput.DisconnectAfterBytes int` and `HTTPOutput.Chunks []Matcher`. The client closes the response body after the configured byte count; chunk assertions observe flush boundaries without changing payload content. Add an AWS EventStream fixture response encoded from fixed headers/payload/CRC values copied from the pinned Bedrock sources.

Run:

```bash
bash -lc 'source .envrc && go test ./t/plugin -run "TestHarness(DisconnectsClient|AssertsFlushedChunks|RunsAWSEventStreamFixture)" -count=1 -v'
```

Expected RED before implementation and PASS after.

- [ ] **Step 2: Convert all 19 source files as separate behavior groups**

Preserve: OpenAI-compatible chat/embeddings, Anthropic request/SSE conversion, Azure paths/version/auth, Gemini, OpenRouter, Vertex auth/body, Bedrock SigV4/EventStream, passthrough mode, protocol conversion, request-body override, upstream variables, streaming limits/duration/flush, client disconnect, provider error mapping, usage/log summaries, and schema validation. Every provider request must be asserted by its fixture; every SSE/EventStream case must assert both client chunks and complete semantic payload.

- [ ] **Step 3: Run the 303-block manifest and AI packages**

```bash
bash -lc 'source .envrc && go test ./t/plugin -run "^TestManifestCorpusExercisesTargetPlugins/ai-proxy$" -count=1'
bash -lc 'source .envrc && go test ./t/plugin -run "TestPluginIntegration/ai-proxy" -count=1 -v'
bash -lc 'source .envrc && go test ./pkg/plugin/ai_proxy ./pkg/plugin/ai_runtime -count=1'
```

- [ ] **Step 4: Commit**

```bash
git add t/plugin/ai-proxy.yaml t/plugin/case.go t/plugin/case_test.go t/plugin/runner_test.go t/plugin/fixture_network_test.go pkg/plugin/ai_proxy pkg/plugin/ai_runtime
git commit -m "test(plugin): convert AI proxy provider matrix"
```

---

### Task 14: Prove Complete 98-Plugin Coverage and Publish PR #4

**Files:**
- Modify: `t/plugin/coverage_test.go`
- Modify: `t/plugin/README.md`
- Modify: `docs/plugins.md`
- Modify: `docs/superpowers/specs/2026-07-14-plugin-integration-tests-design.md`
- Modify: `docs/superpowers/plans/2026-07-15-remaining-61-plugin-integration-tests.md`

**Interfaces:**
- Consumes: all 99 manifests, `docs/plugins.md`, and the pinned Apache checkout.
- Produces: zero semantic failures, verified counts, honest documentation, and a ready PR.

- [ ] **Step 1: Recount pinned sources independently of YAML**

```bash
git -C .cache/apache-apisix checkout c3d7d5ec69774121f53d2e20d29d09c816795dd7
rg -c '^=== TEST ' .cache/apache-apisix/t/plugin > .cache/pinned-test-counts.txt
bash -lc 'source .envrc && go test ./t/plugin -run "TestManifestCorpusValidates|TestDocumentedPluginManifests|TestManifestCorpusExercisesTargetPlugins" -count=1'
```

Expected: all tests PASS; 98 source-backed plugin names plus `redirect2`; zero case/variant target-plugin failures. Compare every manifest `source.tests` with the corresponding `rg` count, including nested source paths.

- [ ] **Step 2: Run the complete real-process suite**

```bash
bash -lc 'source .envrc && make test-integration'
```

Expected: PASS with no skipped tests, no placeholder manifests, no leaked child processes/listeners, and no external network dependency.

- [ ] **Step 3: Update documentation from live counts**

Record the verified complete-manifest counts in the README and plugin status
docs. Document all added fixture kinds and `{{WORK_DIR}}`; remove stale design
statements that live external services or explicit skips remain unsupported.
Mark every checkbox in this plan only after its command has passed.

- [ ] **Step 4: Run repository completion gates**

```bash
bash -lc 'source .envrc && go test ./... -count=1'
bash -lc 'source .envrc && make lint'
bash -lc 'source .envrc && make build'
git diff --check
git status --short
```

Expected: all commands PASS; `git status --short` contains only the intended source, manifest, harness, plan, and documentation files; remove the generated `./apisix` binary if present.

- [ ] **Step 5: Perform merge-level review**

Use `agent-skills:code-review-and-quality`. Review target-plugin authenticity, fixture self-fulfilling assertions, source-number grouping, process/network cleanup, protocol parser bounds, secret leakage, flaky waits/randomness, and unrelated diffs. Repair only verified findings and rerun affected focused plus final gates.

- [ ] **Step 6: Commit, push, and mark PR ready**

```bash
git add t/plugin/coverage_test.go t/plugin/README.md docs/plugins.md docs/superpowers/specs/2026-07-14-plugin-integration-tests-design.md docs/superpowers/plans/2026-07-15-remaining-61-plugin-integration-tests.md
git diff --cached --name-status
git commit -m "docs(test): finalize standalone plugin integration corpus"
git push origin codex/plugin-integration-tests
gh pr ready 4
gh pr view 4 --json isDraft,headRefOid,url
```

Expected: remote head equals local `HEAD`, `isDraft` is `false`, and the PR body reports only commands actually run and passing.

---

## Verification Correction: 2026-07-15

The original checked state was not supported by the manifests. This audit compared each of the 61 manifests with the pinned Apache source titles and the behavior requirements above. A source number being listed once and a route containing the target plugin are necessary, but they do not prove source-complete behavior. A manifest is checked below only when its standalone resources, requests, fixture observations, and APISIX-Go boundary assertions can fail when each mapped plugin behavior is broken.

**Verified result:** 9 of 61 manifests have passed source-completeness review; 52 remain. Thirty-eight manifests use the especially weak one-generic-`source-N`-case-per-source-file pattern. The named manifests were also checked individually because descriptive case names alone are not sufficient.

### Remaining Harness and Coverage Work

- [x] Add a real `TestSourceCoverage` gate that verifies the local Apache
  checkout is clean at the pinned commit and compares every manifest source
  declaration with its actual upstream `=== TEST` blocks or sparse labels. It
  fails loudly when a configured checkout drifts and skips with an explicit
  reason when no checkout is available. Its first run found and corrected the
  fabricated `oas-validator.t` labels 19 and 20: that source has 32 blocks with
  labels 1–18 and 21–34, so the three-source total is 110 rather than 112.
- [ ] Strengthen `TestManifestCorpusExercisesTargetPlugins` so one generic request cannot claim an entire source file merely by listing every source number and activating the target plugin.
- [ ] Add a checked source-behavior ledger or equivalent validation that ties each upstream `TEST` title to the standalone resource, request, and assertion that exercises it.
- [ ] Complete the protocol fixtures promised by Task 3, including LDAP search/failure responses and distinct Redis Cluster/Sentinel behavior rather than routing all three kinds through one generic Redis fixture.
- [ ] Add and test the Task 13 client-disconnect, flushed-chunk assertion, and AWS EventStream primitives before converting `ai-proxy` streaming cases.
- [ ] Add APISIX embedded-wildcard route support before completing `datadog` block 11. `/articles/*/comments` must retain the authored matched URI for plugin variables while matching APISIX arbitrary-depth wildcard semantics; both methodless and method-specific routes currently bypass/reject or panic inconsistently in Chi.
- [ ] Re-run every focused integration after its manifest is corrected; a green run of the current generic case is not completion evidence.

### Per-Plugin Verified Remaining-Work Ledger

#### Task 4 — Core HTTP, Security, and Validation

- [x] `brotli` — all 37 pinned blocks run against a standalone child: default and explicit configuration, decoding/content equality, level-0 versus level-11 compressed-size ordering, header negotiation, content types, schema rejection, encoded-upstream bypass, and ETag handling are asserted.
- [x] `fault-injection` — all 46 pinned blocks run against standalone resources, including schema rejection, abort status/body/headers/variables, redirect precedence, negation, zero percentage, and measured one- and two-second delay behavior with zero/nonmatching bounds.
- [x] `cors` — all 86 pinned blocks run against standalone resources with behavior-specific assertions: schema and route lifecycle, wildcard/regex origin matching, default and explicit CORS response fields, credentials, methods, request/expose headers, metadata-origin validation and override, proxy-rewrite ordering, and timing-origin list/regex precedence. The focused CORS package and real-process integration gates pass.
- [x] `consumer-restriction` — all 71 pinned source blocks run as standalone cases. Direct and `multi-auth` paths isolate authentication probes, execute consumer plugins exactly once around the real downstream request, preserve consumer-over-route precedence, and keep route/service identity in cached consumer chains. Stacked auth execution is idempotent and the HMAC helper rejects static `Authorization` in both header maps. Exact RED-then-GREEN regressions, affected packages, race tests, real-process `consumer-restriction` and `multi-auth`, manifest validation, and `make build` pass; task review approved the result.
- [x] `request-validation` — all 55 pinned blocks are mapped exactly once to real standalone requests covering body/header schema types and rejection matrices, scalar JSON forwarding, nested/array/enum/required constraints, custom status/messages, repeated URL-encoded values, and duplicate-key normalization. APISIX legacy `table`/`function` schema types are normalized only in schema-bearing locations, with focused package regressions; the semantic and real-process gates pass.
- [x] `oas-validator` — all 110 pinned blocks across the three sources are
  mapped exactly once to standalone validation behavior. The pinned first
  source intentionally skips labels 19 and 20; the manifest now declares that
  sparse source matrix instead of inventing two blocks. The manifest covers
  request and response modes, inline and URL specifications, OAS 3.1
  composition/format and parameter/body matrices, initialization-time
  inline-JSON rejection, lazy external-reference failures, URL
  headers/cache/TTL behavior, and all 36 pinned runtime diagnostics. Focused
  package, semantic, real-process, lint, and build gates pass; task review
  approved the result.
- [x] `traffic-label` — all 38 pinned blocks map exactly once to standalone behavior. Missing and empty `rules` reject independently with identifying diagnostics; the source's two-request case now executes and observes two real requests; invalid action/operator/weight diagnostics are source-specific; explicit zero-weight actions are never selected while 100-weight actions are deterministic and omitted weights keep the default. Package, semantic, real-process, race, scoped lint, post-integration gates, and task review pass.

#### Task 5 — Local-Credential Authentication

- [x] `key-auth` — all 58 pinned blocks across four sources map exactly once to
  real standalone cases. Header/query credentials, hiding, consumer schema
  failures, environment and Vault resolution, unresolved-reference fail-closed
  behavior, anonymous limiter chains, service inheritance, domain nodes, and
  default/custom realms are asserted. The child explicitly removes the
  unresolved environment variable, realm validation matches the pinned
  printable-ASCII contract, and package/Store, race, real-process, corpus,
  scoped lint, build, post-integration, and independent review gates pass.
- [x] `basic-auth` — all 44 pinned blocks across three sources map exactly once to standalone consumer/route schema, parsing/credential, last-good reload, hide/preserve, Vault/env, scheme, anonymous limiter, missing-consumer, and realm behavior. Raw validated consumer snapshots persist without secret I/O; only the selected auth plugin resolves a deep copy lazily per request, unresolved references fail closed across Basic/Key/JWT/HMAC, and late Vault provisioning retries without reload. Package/race/cross-auth/store stress, full real-process, sensitive repeats, confidentiality assertions, scoped lint, build, post-integration gates, and task review pass.
- [x] `jwt-auth` — all 130 pinned blocks across seven sources map exactly once
  to independent standalone cases covering anonymous consumers, the complete
  HS/RS/PS/ES/EdDSA algorithm matrix, `nbf` and lifetime-grace claims, custom
  token locations, credential hiding, base64 secrets, real Vault/environment
  resolution, encrypted storage, realm behavior, schema failures, and
  `store_in_ctx` propagation. Removing the last credential cookie now deletes
  the header instead of forwarding an empty value. Package/race, exact
  real-process normal/race, structural and semantic corpus, scoped lint, and
  diff gates pass.
- [x] `hmac-auth` — all 70 pinned blocks across six sources map exactly once to 32 isolated standalone groups and 61 real requests. The cases cover strict consumer/route schemas and last-good behavior, parsing and lookup, Date/GMT/skew/replay, SHA-1/SHA-256/SHA-512 plus allowlists, signed-header defaults/cardinality, body digest/413/restoration, hidden credentials, normal and anonymous limiter chains, realms, real Vault lazy retry, and environment resolution. Package/store and race tests, full real-process coverage, sensitive repeats, independent OpenSSL vectors, confidentiality checks, scoped lint, build, post-integration gates, and task review pass.
- [x] `jwe-decrypt` — all 23 pinned blocks map exactly once to real standalone cases. The corpus validates schema and secret lengths, supported key-management/content-encryption algorithms, protected headers, header/cookie/query extraction, forwarded headers, malformed and decryption failures, consumer key selection, and live Jack-to-Chen consumer replacement. The standalone lifecycle now publishes only completed snapshots, synchronizes store events with a same-channel FIFO barrier, fails closed on malformed routes/global rules, and retains the last-good security handler. Package, race, scheduler stress, strict corpus, repeated real-process, scoped lint, build, post-integration, and task-review gates pass.

#### Task 6 — External Authentication and Authorization

- [x] `ldap-auth` — all 35 pinned blocks across the two sources map exactly
  once to real standalone cases. Consumer/route schema, malformed and
  case-insensitive Basic authorization, failed and successful direct binds,
  default/custom realms, unverified/trusted/untrusted LDAPS, and Vault plus
  environment secret resolution are asserted. Authorization payloads are
  excluded from cumulative logs, and the verification-enabled no-CA control
  proves an always-insecure TLS implementation cannot pass. Package/Store,
  race, fixture, exact real-process, pinned-source, corpus, scoped lint, build,
  post-integration, and independent review gates pass.
- [x] `openid-connect` — all 141 pinned blocks are independent standalone
  cases covering bearer/introspection/JWT, discovery/JWKS, session/PKCE/Redis,
  renewal/logout, proxy trust, TLS, claims, and header behavior.
- [x] `forward-auth` — all 28 pinned blocks across three sources map exactly once to standalone schema, header propagation/spoof resistance, allow/deny, degradation/error, `$post_arg`, CRLF/no-auth, bounded-body 413, chunked re-framing, GET/POST framing, and absent-header clearing behavior. Raw framing assertions combine required bytes with explicit forbidden-header patterns, so they cannot pass from fixture-generated claims. Package/race/corpus, full real-process, sensitive `-count=3`, scoped lint, build, post-integration gates, and task review pass.
- [x] `multi-auth` — all 38 pinned blocks across two sources map exactly once to 13 real standalone groups and 38 requests covering ordered Basic/Key/JWT/HMAC alternatives, exact all-failed diagnostics, silent later success, configured token locations and hiding, invalid nested/cardinality reloads with last-good retention, JWT failures, anonymous failures, and exactly-once consumer chaining. Failed body-validating probes replay a bounded captured prefix plus unread source so later alternatives and upstream receive the full body; probe diagnostics are request-scoped and emitted only if every alternative fails. Affected auth/context package and race tests, full real-process and focused race, sensitive repeats, corpus, scoped lint, build, post-integration gates, and task review pass.
- [x] `wolf-rbac` — all 44 owned pinned blocks (`wolf-rbac.t` 1–42 plus `security-warning2.t` 19–20) map exactly once to real standalone schema, public API, token/header, permission/retry, JSON/raw/102-field password, Vault/env, duplicate-appid consumer replacement, TLS, trusted-IP, and HTTP-warning behavior. The duplicate lifecycle retains the original same-appid consumer, appends the Echo consumer, waits boundedly for the newer credential index, then proves exactly one final permission and upstream request before Echo rewriting. Package/race, full real-process, sensitive and lifecycle repeats, confidentiality assertions, scoped lint, build, post-integration gates, and task review pass.
- [x] `authz-keycloak` — all 45 pinned blocks map exactly once to real
  direct/discovery authorization, service-account token, relative-endpoint,
  denial/diagnostic, secret-reference, and verified/unverified TLS cases.
  Package/Store/data-encryption/registry, race, exact real-process,
  pinned-source, corpus, scoped lint, build, post-integration, and task-review
  gates pass.
- [x] `cas-auth` — all 24 CAS-owned blocks across `cas-auth.t` and `security-warning.t` map exactly once to real standalone login/validation/session/logout, multi-SP/SLO, schema, redirect/signature/callback, initiation, isolation, and warning behavior using captured cookies and exact provider requests. A real changed-resource reload preserves the old session; rejected and forged tickets cannot create/cross scopes; the process-local session store is a mutex-protected 10,000-entry one-hour expirable LRU with refresh, expiry, and eviction coverage. Package/race/corpus, full real-process, sensitive repeats, store stress, scoped lint, build, post-integration gates, and task review pass.
- [x] `saml-auth` — all 21 pinned blocks map exactly once to independent
  standalone schema, signed Redirect/POST login, host-sensitive multi-SP
  session, correlated four-hop logout, and terminal failure cases. Redirect
  query signatures, POST correlation, IdP identity/endpoint separation,
  route fallback, and session trust changes are independently reviewed.
  Package/route/harness, race, exact real-process, source/no-alias, scoped
  lint, build, post-integration, and task-review gates pass.
- [x] `feishu-auth` — all 14 pinned blocks map exactly once to real standalone redirect, query/header code, token POST, userinfo Bearer, signed-cookie reuse/expiry, custom-location, secret-rotation/removal, forged-header clearing, and encrypted-fallback behavior. Distinct readiness routes prevent stale snapshots from satisfying reload probes; exact upstream counts prove accepted versus terminal rejected paths. The encrypted case uses ciphertext standalone resources, decrypts fallback arrays at runtime, and excludes plaintext from child configs/logs; removing the decrypted route debug dump closes the RED-proven secret leak. Package/race, full real-process and focused race, sensitive repeats, corpus, scoped lint, build, post-integration gates, and task review pass.
- [x] `authz-casbin` — all 21 pinned blocks map exactly once to standalone schema, metadata, inline, file, disabled, route-policy-shape, and request behavior. Deny-to-allow and policy1-to-policy2-to-policy1 transitions use atomic standalone snapshot replacement, a side-effect-free applied-state probe, and one consuming request. The shared watcher survives invalid/Remove/Rename/Create/Write sequences and later valid snapshots; workdir file paths are confined. Package/corpus, watcher recovery, race, real-process `-count=10`, scoped lint/build, post-integration, and task-review gates pass.

#### Task 7 — Limits and Cache

- [x] `limit-conn` — all 104 pinned blocks are now independent standalone
  cases covering concurrent/in-flight counters, dynamic variables,
  route/global/consumer scope, Redis authentication/reuse, TLS, SNI, and
  HTTP/2 behavior.
- [ ] `limit-count` — repeated three-request probes omit much of the 252-block fixed/sliding window, delayed-sync, Sentinel/Cluster, group sharing, metadata, reset header, and state-transition matrix.
- [ ] `limit-req` — simple burst probes omit delay versus `nodelay`, atomic Redis concurrency, shared routes, variable errors, authentication, degradation, and HTTP/2 cases.
- [ ] `graphql-limit-count` — one generic GraphQL quota request replaces fragments, cost/depth calculation, local/Redis/Cluster state, quota transitions, and schema rejection.
- [x] `proxy-cache` — all 76 pinned disk and memory blocks map exactly once to
  independent standalone cases covering schema and zone validation, MISS/HIT,
  bypass/no-cache, expiry and Cache-Control directives, Set-Cookie/private
  responses, Vary variants, PURGE, consumer isolation, invalid zones, and
  on-disk persistence. The plugin now registers PURGE with Chi and reports the
  APISIX-compatible unsupported-method cache status for disk and memory
  strategies. Package/race, exact real-process normal/race, source/corpus and
  isolated target-plugin gates, scoped lint, build, and diff checks pass.
- [x] `graphql-proxy-cache` — all 48 pinned blocks are independent standalone
  cases across disk, GraphQL, and memory sources, including method/body/
  variables keying, malformed input, mutation bypass, PURGE, Vary, route/host,
  and consumer isolation.

#### Task 8 — Routing, Workflow, Batch, and Dubbo

- [x] `proxy-mirror` — all 36 pinned blocks across three sources map exactly once to standalone schema, exact primary/mirror traffic, HTTP version, live deletion, bounded concurrent sampling, path/DNS behavior, proxy-rewrite ordering, h2c gRPC, and grpc-web cases. A generic one-shot finalized-request hook applies rewritten URI and method before ordinary or AI terminals; h2c is explicitly configured, bounded counts observe their full window, concurrent captures reject, and DNS diagnostics await the bounded mirror timeout. Affected package/race/harness, full real-process, DNS/sensitive repeats, scoped lint, build, post-integration gates, and task review pass.
- [x] `traffic-split` — all 94 pinned blocks are independent standalone cases
  covering weighted inline/resource upstreams, fallback and zero weights,
  chash, pass-host, HTTPS, health/retry, timeout, body matching, and reload.
- [x] `workflow` — all 42 pinned blocks across four sources map exactly once to
  real standalone cases covering unconditional/no-case rules, AND/OR matching,
  ordered fallthrough, isolated limiter state, invalid and missing rules,
  consumer-over-route actions, global-rule and CORS interaction, and real
  concurrent `limit-conn` behavior. Nested `limit-count` actions now receive
  strict schema validation, reject unsupported groups, and consumer workflow
  limiters override their route counterpart without discarding sibling
  overrides. Package/race, exact real-process normal/race, source/corpus/target,
  scoped lint, build, and diff gates pass.
- [x] `batch-requests` — all 46 pinned blocks across three sources map exactly
  once to real standalone pipeline validation, method/path/header/query/body
  merging, custom URI, body and metadata limits, partial timeout aggregation,
  real-IP handling, and mixed HTTP/h2c gRPC behavior. Validated plugin metadata
  is Store-owned, preserves the last good snapshot across invalid reloads and
  route generations, clears on deletion, isolates Stores, and rejects older
  out-of-order publication. Package, Store, focused route, race, exact
  real-process, corpus, scoped lint, build, post-integration, and task-review
  gates pass. The broad route package still has the unrelated pre-existing
  `TestBuildHandlerStrictRunsConsumerRestrictionFromAuthenticatedConsumer`
  failure.
- [x] `http-dubbo` — all five pinned blocks map one-to-one to real standalone
  POJO/array serialization, timeout/connect failure, void, and application
  failure cases. Fixtures assert protocol-valid Dubbo 2.7.21 FastJSON response
  flags, exact request IDs, payload lengths, request frames, exception payload,
  and empty void body. Connection logging is limited to final live dial
  failures and clears retry state across selection errors/cancellation.
  Package, race, exact real-process, corpus, scoped lint, build,
  post-integration, and task-review gates pass.

#### Task 9 — HTTP and Cloud Loggers

- [x] `http-logger` — all 114 pinned blocks across seven sources map exactly
  once to standalone schema, sink, body/truncation/compression, TLS/auth,
  route/global/consumer metadata, nested-format, batching/retry, lifecycle,
  and invalid-configuration behavior. Package/resource/route, race, exact
  real-process, pinned-source, corpus, scoped lint, build, post-integration,
  and task-review gates pass.
- [x] `clickhouse-logger` — all 23 pinned blocks map exactly once to real standalone cases. The corpus validates required/default/schema configuration, ClickHouse user/key/database headers, JSONEachRow SQL bodies, single and multiple endpoints, deterministic pending-entry overflow against a cancellable slow sink, request/response body capture and expressions, plugin metadata formats, and child-scoped `$ENV://` user resolution including empty values. Package, race, strict corpus, environment isolation, real-process, scoped lint, diff, build, post-integration, and task-review gates pass.
- [x] `google-cloud-logging` — all 33 pinned blocks across three sources map
  exactly once to independent standalone cases covering auth-file and inline
  credentials, signed OAuth exchange, token types, Cloud Logging resource and
  payload fields, metadata and route formats, trusted/untrusted/disabled TLS,
  encrypted private-key storage, batching defaults, delivery failures, and
  deterministic pending-entry overflow. The integration run exposed and fixed
  missing `$host`, port-bearing `$remote_addr`, metadata schema validation,
  scenario-file fixture expansion, and macOS trusted-CA loading. Package,
  logger-batch, race, exact real-process, pinned-source, no-alias, corpus,
  scoped lint, build, and diff gates pass.
- [x] `loggly` — all 22 pinned blocks map exactly once to real standalone
  schema, UDP RFC5424, token/tag, severity, body/expression, metadata,
  HTTP-bulk, encryption-at-rest, and buffered-config isolation cases. The
  reviewed implementation preserves request-host framing and strips internal
  fields; generic standalone encryption recursively covers registered
  containers and plugin metadata, and typed bbolt assertions reject trailing
  data. Package/shared config/store, race, exact real-process, source/corpus,
  scoped lint, build, post-integration, and task-review gates pass.
- [x] `loki-logger` — all 22 pinned blocks map exactly once to real standalone cases. The corpus validates schema branches, tenant and authorization headers, custom endpoints, rich nested default records, static/dynamic/post-upstream labels, stable request timestamps, route and metadata format precedence, additive non-clobber extras, exact stream/value grouping and cardinality, three-request label isolation, and deterministic pending-entry overflow. Internal batch state uses a typed envelope so user fields cannot collide. Package, race, strict semantic matcher, repeated real-process, scoped lint, build, post-integration, and task-review gates pass.
- [x] `datadog` — all 13 pinned blocks map exactly once to real plugin-metadata
  endpoint, ordered/coalesced DogStatsD, upstream-latency, tag, runtime update,
  and invalid-resource cases. Package/route/logger-batch/Store/config/server,
  race, exact real-process, pinned-source, corpus, repeated metadata, scoped
  lint, build, post-integration, and task-review gates pass.
- [x] `elasticsearch-logger` — all 27 pinned blocks map exactly once to
  independent standalone cases covering schema variants, version discovery,
  index/type compatibility, password and Authorization-header authentication,
  deterministic multi-endpoint delivery, bulk NDJSON framing, metadata and
  route log formats, request/response body capture, and sink failures.
  Plaintext credentials are encrypted at rest with case-insensitive
  Authorization matching. Package/config/data-encryption/Store, race, exact
  real-process, pinned-source, no-alias, corpus, scoped lint, build, and diff
  gates pass.
- [x] `rocketmq-logger` — all 42 pinned blocks use a real RocketMQ protocol
  fixture for nameserver discovery and broker publication, including topic,
  signing, body/log formats, batching, timeout, reload, and error behavior.
- [x] `sls-logger` — all 17 pinned blocks map exactly once to independent
  standalone cases covering schema failures, TLS RFC5424 delivery,
  project/logstore credentials, ordered batching, subsecond timestamps,
  metadata and route formats, route removal, encrypted secret storage,
  request/response bodies, and metadata validation. The real default-access
  case exposed and fixed missing request/response fields and route identity;
  metadata schema validation now rejects invalid formats. Package, race, exact
  real-process, source/corpus/target, scoped lint, and diff gates pass.
- [x] `splunk-hec-logging` — all 17 pinned blocks map exactly once to real standalone cases. The corpus validates schema diagnostics, non-blocking HEC auth failures, exact token/channel/content headers, rich and custom event envelopes, post-upstream variable resolution, three-event concatenated batching, keepalive configuration, standalone ciphertext decryption, additive non-clobber metadata extras, and deterministic pending-entry overflow. Package, race, strict corpus, repeated sensitive real-process, scoped lint, build, post-integration, and task-review gates pass.
- [x] `tencent-cloud-cls` — all 22 pinned blocks map exactly once to
  independent standalone cases covering schema branches, HTTP failures, exact
  CLS protobuf/signature semantics, metadata and route formats, route removal,
  encrypted credentials at rest, DNS failures, request/response bodies, and
  verified HTTPS. Package, race, exact real-process, source/corpus/target,
  scoped lint, and diff gates pass.

#### Task 10 — Network, Kafka, File, and Error Loggers

- [x] `tcp-logger` — all 17 pinned blocks map exactly once to real standalone
  schema, plain/TLS delivery, exact TLS retry, reconnect, metadata
  set/update/clear, service-context, body-capture, and nested-format cases.
  Custom fields use APISIX-compatible five-level truncation and diagnostics;
  explicit zero retry delay is preserved. Package, race, exact real-process,
  pinned-source, corpus, scoped lint, build, post-integration, and task-review
  gates pass.
- [x] `udp-logger` — all 14 pinned blocks map exactly once to standalone schema, delivery-failure, live two-sink reload, metadata, and exact request/response-body cases. Default records expose the APISIX-shaped access-log fields with explicit Go-native size approximations; custom records resolve post-downstream and append route/service context. Parsed RFC3339 and strict RFC 6901 network assertions, package/race/corpus, full real-process, reload `-count=10`, sensitive `-count=3`, scoped lint, build, post-integration gates, and task review pass.
- [x] `syslog` — all 21 pinned blocks map exactly once to exact RFC5424
  TCP/UDP/TLS, buffering/flush/drop, retry, metadata, body-capture,
  empty/custom/nested format, and route/service context cases. Package,
  logger-batch, Store/config/server, race, exact real-process, pinned-source,
  corpus, scoped lint, build, post-integration, and task-review gates pass.
- [x] `kafka-logger` — all 99 pinned blocks use real Kafka protocol fixtures
  for records, partitions, metadata, PLAIN/SCRAM, body handling, formats,
  batching/retry, service sharing, reload, and broker failures.
- [x] `file-logger` — all 44 pinned blocks across three sources map exactly once
  to real standalone schema, file append/cache/reopen, live `SIGUSR1`, exact
  default/nested/extra/metadata/route formats, pre-DNS node fields,
  request/response body expressions, gzip, failures, and shutdown behavior.
  Route format is separately proven both without metadata and with conflicting
  metadata. A shared path-keyed writer registry owns cached descriptors and
  synchronous final-lease shutdown. Package, race, full exact real-process,
  combined Batch compatibility, corpus, scoped lint, build, post-integration,
  and task-review gates pass.
- [x] `log-rotate` — all 17 pinned blocks across three sources map exactly
  once to independent standalone cases covering interval and size rotation,
  current/history stability, concurrent triggering, hot disable, compression,
  aligned rotation time, retention, custom paths, and disabled access logs.
  The real-process cases exposed and fixed stale cached file descriptors after
  rotation and loss of explicit `max_kept: 0`. Package, race, exact
  real-process, source/corpus/target, scoped lint, build, and diff gates pass.
- [x] `error-log-logger` — all 39 pinned blocks are independent standalone
  cases covering log levels, metadata lifecycle, and real TCP, ClickHouse,
  Kafka, and SkyWalking delivery.
- [x] `skywalking-logger` — all 15 pinned blocks map exactly once to real standalone cases. The corpus validates minimal/full/missing-endpoint schema paths, exact SkyWalking envelope arrays and nested JSON records, hostname service instances, valid and malformed `sw8` trace context, metadata and route format precedence with route/service identity, exact request/response body capture, and deterministic pending-entry overflow. A typed semantic matcher enforces envelope cardinality, trace presence/absence, and nested payload fields. Package, race, strict corpus, repeated real-process, scoped lint, build, post-integration Loki compatibility, and task-review gates pass.

#### Task 11 — Tracing

- [x] `opentelemetry` — all 81 pinned blocks use semantic OTLP protobuf
  assertions for sampling, trace/span IDs, propagation, resource/route
  attributes, body limits, batching, errors, metadata reload, and shutdown.
- [x] `skywalking` — all 17 pinned blocks across the two sources map exactly
  once to independent standalone cases covering propagated and generated
  trace context, segment/span/log semantics, route and metadata attributes,
  sampling, request/response body capture, collector failures, and shutdown
  delivery. A request-scoped marker prevents duplicate nested segments when
  route and global configurations both enable tracing. Package/race, exact
  real-process normal/race, source/corpus/target, scoped lint, and diff gates
  pass.

#### Task 12 — Bounded AI Plugins

- [x] `ai-aws-content-moderation` — all 23 pinned blocks across three sources
  map exactly once to real standalone Vault/environment/literal credentials,
  exact SigV4 request shape and session-token signing, prompt-only protocol
  extraction, category/toxicity precedence, protocol-shaped denial, selected
  AI instance failure modes, provider pass-through, and Comprehend failures.
  Secret resolution copies bbolt values inside their read transaction and
  diagnostics do not expose credentials. Package, Store, race, exact
  real-process, pinned-source, corpus, scoped lint, build, post-integration,
  and task-review gates pass.
- [x] `ai-prompt-decorator` — all 17 pinned blocks map exactly once to real Chat Completions and Responses API requests. Standalone cases assert prepend, append, both, request-to-request isolation, invalid empty configuration, instructions creation/prepend, string/array input append, combined transformations, and the Chat regression with semantic upstream JSON. The shared `json_equals` matcher preserves arbitrary numeric precision, defines mathematical number equality, preserves array order, ignores object-key order, rejects malformed JSON and non-body scopes, and expands iteration placeholders. Package, matcher/corpus, full tests, real-process, scoped lint/build, post-integration, and task-review gates pass.
- [x] `ai-prompt-guard` — all 44 pinned blocks map exactly once to 18 real
  standalone cases/variants covering schema and invalid regex, allow/deny
  combinations, case sensitivity, roles/history, Chat/Responses content
  shapes, custom rejection, malformed/unsupported bodies, and pass-through
  behavior with exact zero-upstream assertions. `fail_mode` supports
  source-compatible `skip`, `warn`, and `error` for every decode/protocol
  failure, and structured Responses input text is guarded. Package, race,
  exact real-process, corpus, scoped lint, build, post-integration, and
  task-review gates pass.
- [x] `ai-rag` — all 17 pinned blocks map exactly once to real standalone
  embedding and vector-search exchanges, provider auth/header/body assertions,
  Chat and Responses RAG injection, exact validation and provider failures,
  TLS verification defaults, and disabled-verification cases. The source cases
  also exposed and fixed ordinary upstream `pass_host` semantics, validation,
  standard-port formatting, and IPv6-safe node authorities while preserving
  traffic-split and proxy-rewrite precedence. Package, route, race, exact
  real-process, corpus, scoped lint, build, post-integration, and task-review
  gates pass.
- [x] `ai-rate-limiting` — all 58 pinned blocks across three source files map
  exactly once to independent standalone cases. The corpus covers consumer
  isolation, global/instance/rule quotas, token strategies and expressions,
  windows, headers, rejection transitions, local/Redis/Sentinel state,
  encrypted credentials, selected-instance fallback, and streaming charging.
  Final-instance headers and counters are isolated after fallback, Redis
  resource keys are collision-safe, and explicit ciphertext markers remove
  plaintext ambiguity. Package/config/data-encryption/Store, race, exact
  real-process, pinned-source, no-alias, corpus, scoped lint, build, and diff
  gates pass.
- [x] `ai-request-rewrite` — all 19 pinned blocks map exactly once to real
  standalone cases covering provider/schema validation, default OpenAI,
  DeepSeek, and AIMLAPI endpoint selection through offline CONNECT fixtures,
  override paths and query parameters, auth propagation, prompt/options
  payloads, request-body replay, empty bodies, and exact provider failure
  responses and logs. Package, race, exact real-process, corpus, scoped lint,
  build, post-integration, and task-review gates pass.

#### Task 13 — AI Proxy

- [ ] `ai-proxy` — nineteen generic provider requests claim 303 blocks; provider-specific conversions, tools/media, schema/error mapping, SSE/EventStream chunks, usage summaries, upstream variables, limits, flushes, and client disconnects remain.

### Corrected Completion Gate

- [ ] Every unchecked plugin above has behavior-specific standalone cases and focused integration evidence.
- [ ] The source-behavior ledger and semantic gate reject the former generic manifests.
- [ ] All Task 14 repository, integration, review, documentation, and publication gates pass on the corrected corpus.

---

## Corrected Self-Review Results

- **Inventory:** The ledger contains the exact 61 unique manifests from Tasks 4-13: 29 task-review-approved and 32 remaining.
- **Behavioral placeholders:** Thirty manifests use a generic source-file case pattern; the named manifests were separately checked for claimed blocks that have no behaviorally equivalent request or assertion.
- **Harness gaps:** Task 3 protocol coverage and Task 13 streaming/disconnect primitives remain unchecked and are listed before the plugin ledger.
- **Completion boundary:** Task 14 and PR readiness remain unchecked until all 32 remaining manifests, the strengthened semantic gate, and the complete repository gates pass.

## Recheck: 2026-07-18

The pinned source titles and standalone resources/assertions are being rechecked
manifest by manifest. Passing focused package and real-process tests is necessary
but does not restore a checkbox until a task review confirms source-complete
behavior. `consumer-restriction` and `traffic-label` were initially unchecked
after their reviews found concrete gaps. Both have since passed their follow-up
reviews and post-integration gates. At this historical checkpoint the approved scope was **23
complete and 38 remaining**; `oas-validator` also passed its task review with
112 source blocks and 36 runtime diagnostics verified.

The local-credential source audit corrected two complexity assumptions before
implementation. `key-auth` has 58 blocks across four pinned `t/plugin` files,
not only the 34 blocks in `key-auth.t`; the omitted sources require anonymous
consumer limiter chaining, realm headers, service inheritance, and a DNS/domain
upstream fixture in addition to environment/Vault resolution. `basic-auth`
remains Medium at 44 blocks across three sources and should establish the shared
strict consumer-snapshot and secret-resolution contracts. `jwt-auth` has 130
blocks across seven sources and moves to Hard because it additionally requires
relative-time token signing, the HS/RS/ES/EdDSA/JWK matrix, and bounded
serverless-context behavior. These three plugins are serialized through one
consumer/secret owner rather than implemented concurrently.

The `hmac-auth` audit confirms 70 blocks across six pinned sources and keeps it
at upper-end Medium after `basic-auth`. Its current eight cases and fifteen
requests mechanically map the source numbers but omit consumer/route schema
rejection, the clock/default-date/replay matrix, signed-header cardinality,
allowed algorithms, hide-credential behavior, anonymous limiter chains,
default realm, body-size transitions, and real Vault/environment resolution.
After the shared consumer/secret owner lands, HMAC owns only the bounded
SHA-1/SHA-256/SHA-512 and fixed/relative-Date signing helper plus its package
and manifest; it must not run concurrently with another consumer/secret owner.

Two Medium foundation audits also corrected their execution order.
`forward-auth` has 28 blocks across three sources, but its current three cases
are only repeated happy paths. It can run alongside `basic-auth`, while owning
bounded-body 413 handling, secure chunked re-framing, `$post_arg` resolution,
and any zero-upstream fixture assertion. `proxy-mirror` has 36 blocks across
three sources and remains upper-Medium, but must precede the other routing wave:
it owns concurrent sampling/count assertions, protocol capture, structured h2c
gRPC fixtures, and the finalized pre-proxy hook needed to observe
proxy-rewrite/grpc-web transformations. `traffic-split` and `batch-requests`
must wait for that reviewed foundation instead of starting beside it.
The subsequent 94-block `traffic-split` audit moved that manifest to Hard:
`traffic-split4.t` tests 15–16 require background active health probes and
threshold transitions, tests 17–19 require connection-failure retry and node
reselection, and `traffic-split5.t` requires body-preserving `post_arg_*`
resolution. These are cross-cutting proxy-health behaviors, not a safe passive
health or manifest-only substitution. `batch-requests` remains Medium.

The next routing and filesystem audits fixed their exact acceptance surfaces.
`batch-requests` has 46 blocks across `batch-requests.t`,
`batch-requests2.t`, and `batch-requests-grpc.t`; its lane must preserve
strict pipeline validation, body-file and metadata limits, custom public URI,
partial-timeout response cardinality, and a real standalone `protos` plus
grpc-transcode/h2c request mixed with HTTP. A fake HTTP substitute for the five
gRPC blocks is not equivalent. `file-logger` has 44 blocks across
`file-logger-reopen.t`, `file-logger.t`, and `file-logger2.t`; it owns cached
file descriptors plus live `SIGUSR1` reopen, nested and extra log formats,
pre-DNS node fields, metadata precedence, and request/response body behavior.
Its filesystem-lifecycle owner must land before `log-rotate`.

The `log-rotate` audit confirms 17 blocks across `log-rotate.t`,
`log-rotate2.t`, and `log-rotate3.t`. It remains Medium but requires a
deduplicated background timer, hot-disable lifecycle, automatic flush/reopen
after partial missing files, wall-clock alignment, compression-content and
retention assertions, explicit-zero configuration, custom runtime log paths,
and disabled-access-log behavior. Request-triggered rotation, child restart,
manual reopen signals inside a rotate case, precreated archives, or assertions
limited to the current file are not equivalent. Its bounded owner starts only
after the reviewed File Logger writer registry and adds workdir-confined
archive glob/count/content/signature assertions as needed.

The `http-logger` audit confirms 114 blocks across seven sources:
`http-logger-json.t`, `http-logger-large-body.t`,
`http-logger-log-format.t`, `http-logger-new-line.t`, `http-logger.t`,
`http-logger2.t`, and `http-logger3.t`. It stays Medium because the existing
HTTP/TLS/reload/shutdown foundations are sufficient, but starts only after
File Logger: both need nested/default formatting, bounded body capture, and
depth handling. HTTP Logger then exclusively owns its typed HTTP sink
assertions and any `logger_batch` serialization required to prove that a later
batch cannot overtake an in-flight or retrying earlier batch. Every sink must
assert request count, endpoint, content type, batch cardinality, ordering, and
the relevant payload fields or absences; seven generic `/probe` deliveries are
not completion evidence.

The `google-cloud-logging` audit confirms 33 blocks across
`google-cloud-logging.t`, `google-cloud-logging2.t`, and
`google-cloud-logging3.t`. It remains Medium as a serialized successor to the
File/HTTP Logger and `logger_batch` foundations. Its 19 behavior cases must
prove real RS256 OAuth assertions and claims, token-type propagation, token and
entries endpoints, auth-file loading, trusted TLS/hostname mismatch/disabled
verification, exact Cloud Logging resource and payload fields, metadata versus
route formats, encrypted private-key consumption, batching/retry/pending
overflow, reload flush, and shutdown flush. The upstream Admin/etcd ciphertext
round trip is not available in standalone mode; encrypted standalone input
that decrypts into a successful OAuth exchange is the bounded equivalent.
Three generic `/probe` deliveries or whole-body-only sink assertions are not
completion evidence.

The `loggly` audit confirms 22 blocks in `loggly.t` and keeps the manifest
Medium after the shared File/HTTP Logger access-log and HTTP-lifecycle work.
Its behavior cases must prove metadata-driven UDP delivery, RFC5424
token/tag/severity framing, the full default event, request/response body
include/match/mismatch and truncation, metadata and route custom-format
precedence with automatic route/service IDs, exact HTTP bulk path/token/tag
payloads, status-derived severity even when custom output omits `status`, and
per-route inactivity-batch configuration isolation. The standalone data plane
cannot reproduce the Admin/etcd plaintext-to-ciphertext round trip; a clearly
named pre-encrypted-token delivery is the bounded equivalent and must not be
claimed as control-plane proof. One path-only POST claiming all 22 blocks is
not completion evidence.

The `wolf-rbac` audit corrected its source surface from 42 blocks in one file
to 44 blocks across two files: all of `wolf-rbac.t` plus Wolf-specific
`security-warning2.t` tests 19–20. The eight Wolf public-endpoint dispatch cases
in `public-api.t` remain owned by `public-api.yaml` and must not be duplicated.
Wolf stays Medium but waits for the Basic-owned lazy consumer-secret foundation;
after that lands it owns only its package and manifest. Its current single allow
request does not cover provider business-denial status/logging, Vault/env appid,
trusted `real-ip`, or HTTP-versus-HTTPS security warnings, all of which require
real standalone assertions before review.

The `cas-auth` audit likewise corrected 22 direct blocks to 24 CAS-owned blocks:
all of `cas-auth.t` plus `security-warning.t` tests 5–6. CAS remains Medium and
can run after the integrated Forward foundation without a plugin-specific
fixture; ordinary scripted HTTP fixtures can assert `/serviceValidate` and
ordered cookie flows. The lane must own only bounded CAS fixes: 302 redirects,
SameSite validation, terminating valid SLO requests, session-only cookies,
relative-service port handling, process-local fingerprint isolation, and HTTP
security warnings. It must capture real flow cookies rather than precompute a
successful callback.

- **Structural source-file stand-ins (27):** `ai-aws-content-moderation`,
  `ai-prompt-guard`, `ai-proxy`, `ai-rag`,
  `ai-rate-limiting`, `ai-request-rewrite`,
  `authz-keycloak`, `datadog`,
  `elasticsearch-logger`, `error-log-logger`, `file-logger`,
  `google-cloud-logging`, `graphql-limit-count`,
  `http-dubbo`, `http-logger`, `kafka-logger`, `ldap-auth`, `log-rotate`,
  `loggly`, `openid-connect`, `opentelemetry`,
  `rocketmq-logger`, `skywalking`,
  `sls-logger`, `syslog`, `tcp-logger`,
  and `tencent-cloud-cls`. Each maps a whole
  pinned source file to a `*-source-N` case with one broad configuration and
  request; it cannot prove the distinct source blocks it claims.
- **Named but partial scenarios (11):** `key-auth`,
  `jwt-auth`, `saml-auth`, `limit-conn`,
  `limit-count`, `limit-req`, `proxy-cache`, `graphql-proxy-cache`,
  `traffic-split`, `workflow`, and `batch-requests`. These have real
  standalone scenarios, but the pinned test titles show that multiple
  independent schemas, protocols, state transitions, or error branches are
  collapsed into a smaller happy-path set. They remain unchecked until those
  exact behaviors are separately executable and asserted.
- **Task-review-approved (23):** `ai-prompt-decorator`, `authz-casbin`,
  `brotli`, `clickhouse-logger`, `consumer-restriction`, `cors`, `fault-injection`,
  `basic-auth`, `cas-auth`, `feishu-auth`, `forward-auth`, `hmac-auth`, `jwe-decrypt`, `loki-logger`, `multi-auth`, `oas-validator`, `proxy-mirror`, `request-validation`, `skywalking-logger`, `splunk-hec-logging`, `traffic-label`, `udp-logger`, and `wolf-rbac`. No other
  manifest moved to checked status in this recheck.

## Complexity and Parallel Execution Replan: 2026-07-18

The classification audit started with 56 unchecked manifests at commit
`335203d`. Its consumer-restriction review then approved that manifest, so the
active execution tiers below contained 55 remaining manifests before Easy
Wave 1. `traffic-label`, `authz-casbin`, `ai-prompt-decorator`, and
`clickhouse-logger`, `splunk-hec-logging`, `jwe-decrypt`, `loki-logger`,
`skywalking-logger`, `udp-logger`, `forward-auth`, `proxy-mirror`, and
`basic-auth`, `cas-auth`, `feishu-auth`, `hmac-auth`, `multi-auth`, and
`wolf-rbac` had passed review at that checkpoint, leaving **38**. The live
`Current Remaining-Work Analysis` ledger below supersedes that checkpoint.
`datadog` moved from Easy to Medium after its
pinned embedded-wildcard case exposed the shared route prerequisite above.
Each manifest was checked against its pinned Apache source matrix, current
standalone YAML, `docs/plugins.md` implementation status, package tests, and
the harness/protocol work needed to make its source titles executable. The
tier is implementation and review cost, not the plugin's documented runtime
coverage percentage.

- **Easy:** existing runtime and harness are sufficient; work is primarily
  splitting cases and strengthening source-specific assertions, with at most a
  bounded package-local fix.
- **Medium:** a focused fixture, lifecycle, state, signing, or production-path
  change is likely, but no broad protocol emulator or major streaming owner is
  required.
- **Hard:** protocol-accurate external fixtures, concurrency/state lifecycle,
  shared cache/broker/telemetry owners, substantial streaming/cancellation, or
  a very large source matrix dominate the work.

### Easy — 0 remaining (6 at replan)

- [x] `jwe-decrypt`
- [x] `udp-logger`
- [x] `clickhouse-logger`
- [x] `loki-logger`
- [x] `splunk-hec-logging`
- [x] `skywalking-logger`

Execution waves:

1. `traffic-label`, `jwe-decrypt`, and `authz-casbin` in three isolated
   worktrees. These are package/manifest-local and have no shared fixture
   prerequisite.
2. `clickhouse-logger` and one other HTTP logger in parallel after the initial
   source-specific matcher contracts are stable.
3. `udp-logger` plus at most two HTTP logger manifests in parallel after the
   UDP capture contract is stable. Shared `logger_batch` production changes
   are serialized through one owner; other lanes stop at manifest/package
   evidence if they expose the same runtime gap.
4. Finish the remaining HTTP logger manifests with the same rule: YAML and
   package-local work may proceed in parallel, but common batch/retry/shutdown
   code has one owner and one review range.

### Medium — 1 remaining (28 listed)

- [x] `datadog`
- [x] `basic-auth`
- [x] `hmac-auth`
- [x] `forward-auth`
- [x] `multi-auth`
- [x] `wolf-rbac`
- [x] `cas-auth`
- [x] `feishu-auth`
- [ ] `graphql-limit-count`
- [x] `graphql-proxy-cache`
- [x] `proxy-mirror`
- [x] `workflow`
- [x] `batch-requests`
- [x] `http-logger`
- [x] `google-cloud-logging`
- [x] `loggly`
- [x] `elasticsearch-logger`
- [x] `sls-logger`
- [x] `tencent-cloud-cls`
- [x] `tcp-logger`
- [x] `syslog`
- [x] `file-logger`
- [x] `log-rotate`
- [x] `skywalking`
- [x] `ai-aws-content-moderation`
- [x] `ai-prompt-guard`
- [x] `ai-rag`
- [x] `ai-request-rewrite`

Execution waves:

1. Run `basic-auth` first to establish strict consumer snapshot validation and
   the shared environment/Vault secret-resolution contract. Run `hmac-auth`
   only after that owner is integrated; run `multi-auth` after the shared
   consumer-runner baseline is stable. Only one lane may change consumer
   attachment, secret providers, or `pkg/route`.
2. Run `forward-auth` first as the Medium HTTP-auth foundation. Its existing
   HTTP/TCP fixtures are sufficient, but this lane exclusively owns a fixture
   zero-request assertion, bounded request-body handling, chunk-safe framing,
   and any shared `$post_arg` request-variable change. After it is reviewed and
   integrated, parallelize package/manifest work for `wolf-rbac`, `cas-auth`,
   and `feishu-auth`; `fixture_auth_test.go` remains single-owner.
3. Run `proxy-mirror` first as the routing/protocol foundation. It exclusively
   owns protocol assertions, bounded parallel repeat/count ranges, structured
   h2c gRPC fixtures, and the finalized pre-proxy mirror hook needed for
   proxy-rewrite/grpc-web ordering. After that reviewed range is integrated,
   `batch-requests` may proceed through its distinct public-endpoint owner.
   `traffic-split` moved to Hard after its exact source audit exposed active
   health-check and connection-retry owners rather than a bounded manifest-only
   conversion.
4. Process HTTP/cloud loggers in groups of three isolated manifests, with one
   serialized `logger_batch` owner. Process `tcp-logger`/`syslog` through one
   network-fixture owner, and `file-logger` before `log-rotate` through one
   filesystem-lifecycle owner.
5. Run bounded AI manifests in isolated worktrees, never concurrently with a
   production change to the same `ai_protocols`, `ai_auth`, or `ai_runtime`
   owner. Fixed clocks, credentials, and signed-request fixtures are shared
   contracts rather than per-plugin variants.
6. Dependency exceptions: `datadog` follows the shared embedded-wildcard route
   prerequisite; `graphql-limit-count` follows the Hard Redis owner,
   `graphql-proxy-cache` follows the Hard `proxy-cache` owner, and `workflow`
   follows the limiter owners. They remain Medium because their own conversion
   is bounded, but they are not scheduled before those Hard prerequisites.

### Hard — 3 remaining (18 listed)

- [x] `key-auth`
- [x] `jwt-auth`
- [x] `ldap-auth`
- [x] `openid-connect`
- [x] `authz-keycloak`
- [x] `saml-auth`
- [x] `limit-conn`
- [ ] `limit-count`
- [ ] `limit-req`
- [x] `proxy-cache`
- [x] `traffic-split`
- [x] `http-dubbo`
- [x] `rocketmq-logger`
- [x] `kafka-logger`
- [x] `error-log-logger`
- [x] `opentelemetry`
- [x] `ai-rate-limiting`
- [ ] `ai-proxy`

Execution waves:

1. After Medium `basic-auth` establishes strict consumer snapshots and shared
   secret resolution, run `key-auth` through the single consumer/secret owner;
   it additionally owns realm, limiter-chain, service inheritance, and the DNS
   domain-node fixture. Run `jwt-auth` only after those shared prerequisites
   are integrated; it owns relative-time signing plus the HS/RS/ES/EdDSA/JWK
   algorithm matrix and bounded serverless-context visibility.
2. In parallel with that serialized credential lane, build independent
   foundation owners: LDAP bind/search/TLS, Redis state/concurrency beginning
   with `limit-conn`, and one broker/protocol fixture beginning with
   `rocketmq-logger`.
3. Run `traffic-split` through one serialized proxy-health owner: implement
   cancellable active HTTP/TCP probes with threshold transitions, request-body
   safe retry/reselection with default and explicit retry semantics, and
   `post_arg_*` resolution before converting its 94 pinned blocks. Passive
   quarantine is not an acceptable substitute for the pinned background active
   checker case.
3. Continue owners sequentially within their conflict group while other groups
   run in parallel: `limit-req` then `limit-count`; `http-dubbo` and Kafka only
   after the protocol-fixture owner is free; `proxy-cache` owns cache-zone and
   persistence semantics.
4. Run `authz-keycloak`, then `saml-auth`, then `openid-connect` through the
   serialized auth/session fixture owner. `openid-connect` is last in that
   group because it combines discovery/JWKS, cookies/PKCE, TLS, and Redis
   sessions.
5. Run `error-log-logger` only after Kafka and the HTTP/SkyWalking sink
   contracts are stable. Run `opentelemetry` with exclusive ownership of OTLP
   protobuf, HTTP/gRPC collector, batching, shutdown, and HTTP/2 isolation
   fixtures.
6. Run `ai-rate-limiting` after bounded AI state conventions are stable.
   `ai-proxy` is last because it owns the 303-block provider matrix and the new
   disconnect, flushed-chunk, and AWS EventStream harness contracts.

### Parallel Worker and Merge Contract

- Use at most three implementation subagents concurrently, each in a distinct
  ignored git worktree and branch created from the same reviewed integration
  head. Never let parallel implementers share the repository index.
- Each worker owns one manifest/task, follows RED-then-GREEN for production or
  harness changes, commits a scoped range, and produces a diff package. A
  separate task reviewer must approve source completeness and code quality
  before the range is integrated.
- Cherry-pick approved ranges into `codex/plugin-integration-tests` one at a
  time. Re-run the plugin's semantic and real-process gates after integration;
  rebase/re-execute a worker if an earlier range changed one of its consumed
  shared contracts.
- Shared owners are serialized: consumer/auth pipeline, auth fixtures, Redis,
  Dubbo/broker protocol fixtures, `logger_batch`, network fixtures, file
  lifecycle, cache zones, tracing collectors, and AI protocol/runtime/streaming.
  Parallel lanes may edit distinct YAML/package files but must stop and report
  before modifying a shared owner assigned to another lane.
- Current full-suite baseline is not green. The 2026-07-28 refresh passed all
  production packages, then `t/plugin` failed six remaining-work cases: two
  `chaitin-waf` metadata expectation mismatches
  (`metadata-rejects-empty-nodes` and `metadata-requires-node-host`), one
  `datadog-source-1` UDP delivery case, and three generic `log-rotate-source-*`
  file-lifecycle cases whose expected access log is never created. Repository
  lint also has five existing findings outside the current consumer fix.
  Workers must report these exact baseline failures separately and may not
  claim `go test ./...` or repository lint passed until they are resolved.

## Current Remaining-Work Analysis: 2026-07-30

The user's reference to 56 remaining manifests is the historical count at
commit `335203d`. The live checked ledger above is authoritative: 57 manifests
are integrated and verified and 4 are still unchecked. An approved worker branch
does not reduce that number until its commits are integrated and its
post-integration gates pass.

The following tiers supersede the 2026-07-18 complexity lists for execution
ordering. They classify the intrinsic implementation and review risk exposed by
the pinned source cases, not merely the number of commands left to land an
already-implemented branch.

### Easy — 0 remaining (4 at replan)

- [x] `ai-prompt-guard` — 44 blocks; existing HTTP/JSON fixtures and package
  behavior cover the needed schema, pattern, role, malformed-body, and
  pass-through cases.
- [x] `ai-rag` — all 17 blocks are source-complete with deterministic
  embedding/vector fixtures, exact Chat and Responses transformations,
  provider failures and logs, TLS defaults, and meaningful upstream Host
  assertions. The approved range includes the shared `pass_host` route fix and
  passed its post-integration package, route, race, real-process, corpus, lint,
  and build gates.
- [x] `ai-request-rewrite` — all 19 blocks are source-complete. Default
  provider endpoints are observed through local CONNECT fixtures, successful
  rewrites preserve the pinned `/anything` request/replay contract, provider
  errors and logs are exact including fixture newlines, and the approved range
  passed its post-integration gates.
- [x] `http-dubbo` — 5 blocks; the framed Dubbo fixture and package tests
  already cover the required POJO/array, application/void, timeout, and
  connection-failure paths.

### Medium — 0 remaining (19 at replan)

- [x] `key-auth` — all 58 pinned blocks are source-complete. The approved range
  adds strict consumer snapshots, exact realm and auth diagnostics, real
  environment/Vault resolution, anonymous limiter and service inheritance,
  domain-node behavior, and deterministic unresolved-secret isolation. Its
  post-integration package/Store, race, harness, exact real-process, corpus,
  scoped lint, build, and diff gates pass.
- [x] `ldap-auth` — all 35 pinned direct-bind blocks are source-complete with
  strict consumer/route schemas, redacted authorization diagnostics,
  case-insensitive Basic parsing, exact realm behavior, real LDAP bind bytes,
  trusted/untrusted LDAPS controls, and real Vault/environment references. Its
  post-integration package/Store, race, fixture, exact real-process,
  pinned-source, corpus, scoped lint, build, and diff gates pass.
- [x] `authz-keycloak` — all 45 pinned blocks are source-complete with direct
  and discovery-based authorization, real service-account tokens, relative
  endpoints, exact denial and diagnostic behavior, secret references, and
  verified/unverified TLS. Resty clients, discovery, and token caches are
  isolated by TLS mode and hashed effective CA contents, so insecure discovery
  cannot feed verified routes. The pinned lazy-path conditional is enforced by
  JSON Schema as well as PostInit. Its post-integration package, Store,
  data-encryption, registry, race, real-process, pinned-source, corpus, scoped
  lint, build, and diff gates pass.
- [x] `saml-auth` — all 21 pinned blocks are source-complete with independent
  real-child schema, signed Redirect/POST authentication, host-sensitive
  multi-SP sessions, standards-compatible query signatures, correlated
  Redirect/POST logout, distinct IdP trust identity and endpoint validation,
  and terminal callback failures with zero upstream traffic. Mixed
  host/hostless route fallback and trust-identity session invalidation close
  the final independent-review gaps. Its post-integration package/route,
  harness, race, real-process, source/no-alias, scoped lint, build, and diff
  gates pass; the whole route package retains an unrelated global-Store test
  ordering baseline.
- [x] `ai-aws-content-moderation` — all 23 blocks are source-complete with
  real Vault/environment/literal credentials, exact SigV4 and session-token
  signing, prompt-only moderation, threshold/category precedence,
  protocol-shaped denial, selected-instance modes, provider pass-through, and
  service failures. The Store resolver owns copied bbolt values and redacted
  diagnostics. Its post-integration package, Store, race, real-process,
  pinned-source, corpus, scoped lint, build, and diff gates pass.
- [x] `datadog` — all 13 pinned blocks are source-complete with real
  plugin-metadata endpoint configuration, ordered/coalesced DogStatsD metrics,
  exact optional upstream latency, tags, runtime metadata updates, and strict
  zero-packet invalid-resource assertions after child shutdown. The shared
  route prerequisite now preserves embedded-wildcard siblings and catch-all
  precedence with method-isolated dispatch. Burst UDP assertion failures cannot
  block fixture shutdown. Its post-integration package, route, logger-batch,
  Store/config/server, race, real-process, pinned-source, corpus, repeated
  metadata, scoped lint, build, and diff gates pass.
- [x] `tcp-logger` — all 17 blocks are source-complete with exact plain/TLS
  delivery and retry behavior, metadata lifecycle, runtime service context,
  body capture, APISIX-compatible nested custom fields, and explicit-zero
  logger-batch configuration. Its post-integration package, race,
  real-process, pinned-source, corpus, scoped lint, build, and diff gates pass.
- [x] `syslog` — all 21 pinned blocks are source-complete with exact RFC5424
  framing over real TCP/UDP/TLS, explicit socket buffering/flush/drop controls,
  bounded write deadlines, connection reuse, metadata lifecycle, body capture,
  empty/custom/nested formats, and runtime route/service identity. Partial,
  failed, and zero-progress writes retain only the unsent suffix in order and
  assign retry ownership without loss or duplication. Its post-integration
  package, logger-batch, Store/config/server, race, real-process,
  pinned-source, corpus, Datadog shared-harness regression, scoped lint, build,
  and diff gates pass.
- [x] `http-logger` — all 114 pinned blocks across seven source files are
  source-complete with real standalone sinks, upstreams, requests, exact
  payloads, body truncation/compression, TLS/auth, route/global/consumer
  metadata, nested formats and final status, batching/retry, unchanged-
  processor stale/capacity lifecycle, and explicit invalid configurations.
  The approved range also adds route-label context and deterministic repeated
  fixture bodies. Its post-integration package/resource/route, race,
  real-process, pinned-source, corpus, scoped lint, build, and diff gates pass.
- [x] `loggly` — all 22 pinned blocks are source-complete with exact
  standalone schema, RFC5424 UDP, HTTP bulk, metadata/route formatting,
  request/response body expressions, severity maps, plaintext-token runtime
  use plus ciphertext-at-rest proof, and per-config buffered isolation.
  Request-host framing and strict typed bbolt decoding close the independent
  review findings; generic container and plugin-metadata encryption preserves
  the runtime plaintext contract. Its post-integration package/config/store,
  race, real-process, source/corpus, scoped lint, build, and diff gates pass.
- [x] `elasticsearch-logger` — all 27 pinned blocks are source-complete with
  independent standalone routes, upstreams, authenticated/versioned sinks,
  exact bulk requests, deterministic delivery across both configured
  endpoints, index/type compatibility, log formats, body capture, and
  plaintext-to-ciphertext-at-rest assertions for passwords and Authorization
  headers. Its post-integration package/config/data-encryption/Store, race,
  real-process, pinned-source, no-alias, corpus, scoped lint, build, and diff
  gates pass.
- [x] `google-cloud-logging` — all 33 pinned blocks are independent,
  source-complete, and alias-free. Real standalone processes prove auth-file
  and inline credentials, OAuth/JWT requests, custom token types, exact
  resource/log fields, metadata and route formats, TLS verification modes,
  encrypted private-key persistence, batching defaults and pending overflow.
  Package and shared logger lifecycle tests prove RS256 claims/signatures,
  retry/flush/stop behavior, and schema rejection. Normal and race exact
  real-process runs, pinned-source/corpus/target gates, scoped lint, build, and
  diff checks pass.
- [x] `sls-logger` — all 17 pinned blocks are independent, alias-free
  standalone cases with real TLS syslog delivery, exact project/logstore
  framing, schema and metadata failures, ordered batches, timestamps, route
  removal, encrypted secret storage, custom formats, and body capture. Default
  records now include request/response context and route identity while
  preserving optional ResponseWriter interfaces. Package/race, exact
  real-process normal/race, source/corpus/target, scoped lint, and diff gates
  pass.
- [x] `tencent-cloud-cls` — all 22 pinned blocks are independent, alias-free
  standalone cases with exact signed protobuf delivery, HTTP and DNS failures,
  metadata and route precedence, route removal, encrypted secret storage, body
  capture, and verified TLS. A dedicated readiness route isolates hot-delete
  assertions from startup probes. Package/race, exact real-process
  normal/race, source/corpus/target, scoped lint, and diff gates pass.
- [x] `error-log-logger` — all 39 pinned blocks are independent, alias-free
  standalone cases with real TCP, ClickHouse, Kafka, and SkyWalking sinks,
  exact level filtering, metadata lifecycle, batching, encrypted secrets, and
  route-independent global error observation. Package/race, exact real-process
  normal/race, source/corpus/target, scoped lint, build, and diff gates pass.
- [x] `ai-rate-limiting` — all 58 pinned blocks are source-complete as
  independent, alias-free standalone cases covering consumer isolation,
  global/instance/rule quotas, token strategies and expressions, windows,
  headers, rejection transitions, local/Redis/Sentinel state, encrypted
  credentials, selected-instance fallback, and streaming charging. Final
  fallback headers/counters, collision-safe Redis resource keys, and explicit
  ciphertext markers close the independent-review findings. Its
  post-integration package/config/data-encryption/Store, race, real-process,
  pinned-source, corpus, scoped lint, build, and diff gates pass.
- [ ] `graphql-limit-count` — blocked on the Hard Redis owner.
- [x] `graphql-proxy-cache` — all 48 pinned disk, GraphQL, and memory blocks
  are independent, alias-free standalone cases covering request keying,
  malformed input, mutation bypass, cache status and TTL, PURGE, Vary,
  route/host identity, and consumer isolation. Package/race, exact real-process
  normal/race, source/corpus/target, scoped lint, build, and diff gates pass.
- [x] `workflow` — all 42 pinned blocks are independent, source-complete,
  alias-free standalone cases with unconditional and conditional rule
  evaluation, ordered fallthrough, isolated limiter state, schema rejection,
  consumer override, global-rule/CORS interaction, and concurrent
  `limit-conn` assertions. Package/race, exact real-process normal/race,
  source/corpus/target, scoped lint, build, and diff gates pass.

### Hard — 3 remaining (15 at replan)

- [x] `batch-requests` — the approved worker range realized this risk through
  strict pipeline/metadata state, mixed HTTP/gRPC behavior, and ordered
  Store-owned last-good metadata publication. It is integrated and passed its
  post-integration gates.
- [x] `file-logger` — the worker range realized this risk through a shared
  writer registry, cached descriptors, live `SIGUSR1`, reload/shutdown
  lifecycle, and filesystem assertions. The rereview gap was fixed by isolating
  source blocks 6–7 without metadata from blocks 8–9 with metadata precedence;
  the corrected range is integrated and passed its post-integration gates.
- [x] `log-rotate` — all 17 pinned blocks across three sources are
  independent, alias-free standalone cases for interval/size rotation,
  compression, aligned scheduling, retention including explicit zero,
  concurrent ticks, hot disable, missing paths, custom names, and disabled
  access logging. Rotation now reopens cached file writers and preserves
  explicit `max_kept: 0`. Package/race, exact real-process normal/race,
  source/corpus/target, scoped lint, build, and diff gates pass.
- [x] `skywalking` — all 17 pinned blocks are independent, source-complete,
  alias-free standalone cases with exact trace propagation, generated
  context, sampling, span/log metadata, body capture, collector failures, and
  shutdown delivery. Route-plus-global activation emits one segment per
  request. Package/race, exact real-process normal/race, source/corpus/target,
  scoped lint, and diff gates pass.
- [x] `jwt-auth` — all 130 pinned blocks across seven sources are independent,
  source-complete, alias-free standalone cases covering algorithms, claims,
  token locations, hiding, base64 and encrypted credentials, real
  Vault/environment resolution, realms, anonymous consumers, schema failures,
  and context propagation. Package/race, exact real-process normal/race,
  structural and semantic corpus, scoped lint, and diff gates pass.
- [x] `openid-connect` — all 141 pinned blocks across twelve sources are
  independent, alias-free standalone cases covering bearer and code flows,
  introspection/JWT/JWKS, sessions and PKCE, Redis, claims, renewal/logout,
  trusted forwarding, proxies, TLS, and header behavior. Package/server/ctx
  race, exact real-process normal/race, source/corpus/target, scoped lint,
  build, and diff gates pass.
- [x] `limit-conn` — all 104 pinned blocks across six sources are independent,
  source-complete standalone cases. They cover local, Redis, and Redis Cluster
  policies; dynamic variables; route/global/consumer scope; real held-request
  concurrency; delay and latency diagnostics; authentication and connection
  reuse; plaintext and TLS clusters; SNI and HTTP/2. The conversion fixed
  NGINX-variable lookup, Redis config scoping, exact Redis diagnostics, request
  latency logging, and empty combination-key fallback. Package/race, Redis
  fixture normal/race, exact real-process normal/race, source/corpus/target,
  full 104-case mapping, build, and diff gates pass.
- [ ] `limit-count` — 69 of 252 pinned blocks are now independent real
  standalone cases across delayed-sync, sliding-window, variable, local
  lifecycle, cost, and Redis keepalive/environment sources. The strict
  manifest gate enforces the full pinned source inventory, ordered singleton
  mapping, target-plugin resources, and a non-increasing duplicate-backlog
  ceiling; 28 duplicate groups/160 cases remain. Review remediation also made
  Sentinel replica routing panic-free, disabled unsafe delayed sync for
  request-resolved limits, bounded local sliding-window state, preserved
  nested Redis configuration, and populated `$rate_limiting_info` for sliding
  and delayed windows. Completion remains unchecked until the remaining
  sources replace their generic mapped cases and pass the final gates.
- [ ] `limit-req` — 16 of 89 pinned blocks are now independent real standalone
  cases covering delayed versus nodelay execution, combination keys and
  fallback diagnostics, consumer counters shared across routes but isolated
  across consumers, SNI TLS, and parallel HTTP/2. Completion remains unchecked
  until the local and Redis source families are converted and reviewed.
- [x] `proxy-cache` — all 76 pinned disk and memory blocks are independent,
  source-complete, alias-free standalone cases covering schema/zones, cache
  status transitions, bypass/no-cache, TTL and Cache-Control, Set-Cookie,
  Vary, PURGE, consumer isolation, invalid zones, and persistence.
  Package/race, exact real-process normal/race, source/corpus and isolated
  target-plugin gates, scoped lint, build, and diff checks pass.
- [x] `traffic-split` — all 94 pinned blocks across five sources are
  independent, alias-free standalone cases covering ordered matches, weighted
  inline/resource upstreams, fallback and zero weights, chash, pass-host,
  HTTPS, health/retry, timeouts, form-body matching, and reload behavior.
  Transport-error retries now honor explicit zero and APISIX's omitted
  nodes-minus-one default while selecting distinct retry targets. Package/
  route/resource/proxy race, exact real-process normal/race,
  source/corpus/target, scoped lint, build, and diff gates pass.
- [x] `rocketmq-logger` — all 42 pinned blocks are independent standalone
  cases with real nameserver code 105 discovery and broker code 10 publish,
  message/topic/key/tag/partition/signature assertions, error paths, reload,
  compressed bodies, batching, and race-safe shutdown. Package/race, exact
  real-process normal/race, source/corpus/target, scoped lint, build, and diff
  gates pass.
- [x] `kafka-logger` — all 99 pinned blocks plus thirteen schema variants are
  independent standalone cases with record-level assertions, metadata and
  partition behavior, PLAIN/SCRAM, formats and compressed bodies, batching,
  retries, service-owned processor sharing, reload, and broker failures.
  Package/route race, exact real-process normal/race, source/corpus/target,
  scoped lint, build, and diff gates pass. `api_version` remains a validated
  compatibility option rather than a forced kafka-go Produce version.
- [x] `opentelemetry` — all 81 pinned blocks are independent standalone cases
  with semantic OTLP protobuf assertions for span/resource attributes,
  trace/span/parent IDs, propagation, sampling, request variables, batching,
  metadata reload, errors, and shutdown flush. Package/race, exact
  real-process normal/race, source/corpus/target, scoped lint, build, and diff
  gates pass.
- [ ] `ai-proxy` — provider smoke, the main OpenAI cases, official endpoint
  proxying, passthrough routing, flush timing, and upstream-variable cases now
  use real provider fixtures and streaming assertions. The conversion has
  already found and fixed passthrough method/path/query precedence, Bedrock
  validation and model URL escaping, logical SSE frame timing, and
  `$upstream_*` registration. Anthropic, protocol conversion, request override,
  stream limits, client disconnect observability, Vertex AI, and the remaining
  generic source groups keep this item unchecked.

### Updated Parallel Execution Waves

1. In parallel, remediate and rereview File Logger, integrate the approved
   Batch Requests range, and implement `ai-prompt-guard` plus `http-dubbo` in
   isolated worktrees. Batch integration stays in the primary worktree; no
   worker may edit Store metadata or the file-writer lifecycle owned by those
   two ranges.
2. Finish Easy with `ai-rag`, then `ai-request-rewrite`; keep one owner for
   shared AI protocol/runtime changes.
3. Run independent Medium foundations in parallel: `key-auth`, `ldap-auth`,
   `tcp-logger`, and `ai-aws-content-moderation`. Serialize their dependent
   lanes (`jwt-auth`; auth/session; network loggers; bounded AI) behind reviewed
   integration heads.
4. Give `http-logger` exclusive `logger_batch` ownership, then parallelize
   source-specific HTTP/cloud logger manifests that consume the reviewed
   contract without changing it.
5. Run Hard owners concurrently only across independent domains: Redis,
   cache, proxy health, broker, telemetry, and auth/session. Serialize every
   plugin within its owner group and rebase/re-execute consumers when a
   prerequisite changes.
6. Run `ai-proxy` last, after bounded AI contracts and the disconnect,
   flushed-chunk, and AWS EventStream harness primitives are reviewed.

### 2026-07-31 Checkpoint Verification

- The standalone runner executes independent cases with a global six-process
  cap while preserving ordered steps and variants inside each case. This keeps
  the expanded corpus inside Go's default package timeout.
- `source .envrc && go test ./... -count=1` passes; the complete `t/plugin`
  package finishes in 337.762 seconds.
- `source .envrc && make build` passes.
- `make lint` reports only five pre-existing findings in untouched Brotli,
  CORS, and request-validation files; the current diff adds no lint finding.
