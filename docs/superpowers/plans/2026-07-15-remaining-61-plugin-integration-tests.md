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
| `oas-validator` | three sources / 112 | inline and referenced OpenAPI operations, path/query/header/cookie/body validation, formats, nullable/composition, request/response modes, unmatched operations, schema errors |
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
- [x] `oas-validator` — all 112 pinned blocks across the three sources are mapped exactly once to standalone validation behavior. The manifest covers request and response modes, inline and URL specifications, OAS 3.1 composition/format and parameter/body matrices, initialization-time inline-JSON rejection, lazy external-reference failures, URL headers/cache/TTL behavior, and all 36 pinned runtime diagnostics. Focused package, semantic, real-process, lint, and build gates pass; task review approved the result.
- [x] `traffic-label` — all 38 pinned blocks map exactly once to standalone behavior. Missing and empty `rules` reject independently with identifying diagnostics; the source's two-request case now executes and observes two real requests; invalid action/operator/weight diagnostics are source-specific; explicit zero-weight actions are never selected while 100-weight actions are deterministic and omitted weights keep the default. Package, semantic, real-process, race, scoped lint, post-integration gates, and task review pass.

#### Task 5 — Local-Credential Authentication

- [ ] `key-auth` — header/query hiding cases are real, but environment/Vault source cases are replaced with literal credentials, so secret resolution is not tested.
- [x] `basic-auth` — all 44 pinned blocks across three sources map exactly once to standalone consumer/route schema, parsing/credential, last-good reload, hide/preserve, Vault/env, scheme, anonymous limiter, missing-consumer, and realm behavior. Raw validated consumer snapshots persist without secret I/O; only the selected auth plugin resolves a deep copy lazily per request, unresolved references fail closed across Basic/Key/JWT/HMAC, and late Vault provisioning retries without reload. Package/race/cross-auth/store stress, full real-process, sensitive repeats, confidentiality assertions, scoped lint, build, post-integration gates, and task review pass.
- [ ] `jwt-auth` — a small token matrix replaces the pinned signing endpoint, HS/RS/ES/EdDSA algorithms, `nbf`/grace claims, base64 and Vault keys, schema failures, and context behavior.
- [x] `hmac-auth` — all 70 pinned blocks across six sources map exactly once to 32 isolated standalone groups and 61 real requests. The cases cover strict consumer/route schemas and last-good behavior, parsing and lookup, Date/GMT/skew/replay, SHA-1/SHA-256/SHA-512 plus allowlists, signed-header defaults/cardinality, body digest/413/restoration, hidden credentials, normal and anonymous limiter chains, realms, real Vault lazy retry, and environment resolution. Package/store and race tests, full real-process coverage, sensitive repeats, independent OpenSSL vectors, confidentiality checks, scoped lint, build, post-integration gates, and task review pass.
- [x] `jwe-decrypt` — all 23 pinned blocks map exactly once to real standalone cases. The corpus validates schema and secret lengths, supported key-management/content-encryption algorithms, protected headers, header/cookie/query extraction, forwarded headers, malformed and decryption failures, consumer key selection, and live Jack-to-Chen consumer replacement. The standalone lifecycle now publishes only completed snapshots, synchronizes store events with a same-channel FIFO barrier, fails closed on malformed routes/global rules, and retains the last-good security handler. Package, race, scheduler stress, strict corpus, repeated real-process, scoped lint, build, post-integration, and task-review gates pass.

#### Task 6 — External Authentication and Authorization

- [ ] `ldap-auth` — each source file is represented by one successful bind; realm, bind/search mapping, TLS, schema, and authentication failure behavior remain.
- [ ] `openid-connect` — twelve generic provider-authentication cases replace 141 bearer/introspection/JWT, discovery/JWKS, session/PKCE/Redis, renewal/logout, proxy, TLS, and header behaviors.
- [x] `forward-auth` — all 28 pinned blocks across three sources map exactly once to standalone schema, header propagation/spoof resistance, allow/deny, degradation/error, `$post_arg`, CRLF/no-auth, bounded-body 413, chunked re-framing, GET/POST framing, and absent-header clearing behavior. Raw framing assertions combine required bytes with explicit forbidden-header patterns, so they cannot pass from fixture-generated claims. Package/race/corpus, full real-process, sensitive `-count=3`, scoped lint, build, post-integration gates, and task review pass.
- [ ] `multi-auth` — one successful alternative per source omits ordering, failure precedence, anonymous behavior, consumer propagation, and invalid schemas.
- [ ] `wolf-rbac` — one authorization round trip claims 42 direct blocks and omits two Wolf-specific security-warning blocks; schema/defaults, public APIs, token locations, provider denial/retry mapping, consumer replacement/chaining, exact headers/bodies, Vault/environment appid, TLS verification, trusted client IP, and HTTP warning behavior remain.
- [ ] `authz-keycloak` — one allow decision per source replaces discovery/token/UMA, lazy paths, permissions, client credentials, timeout/TLS, and provider failures.
- [x] `cas-auth` — all 24 CAS-owned blocks across `cas-auth.t` and `security-warning.t` map exactly once to real standalone login/validation/session/logout, multi-SP/SLO, schema, redirect/signature/callback, initiation, isolation, and warning behavior using captured cookies and exact provider requests. A real changed-resource reload preserves the old session; rejected and forged tickets cannot create/cross scopes; the process-local session store is a mutex-protected 10,000-entry one-hour expirable LRU with refresh, expiry, and eviction coverage. Package/race/corpus, full real-process, sensitive repeats, store stress, scoped lint, build, post-integration gates, and task review pass.
- [ ] `saml-auth` — four broad cases claim schema validation, signed ACS, multi-SP sessions, login/logout, and callback failures without preserving those distinct stateful behaviors.
- [ ] `feishu-auth` — one login flow replaces state/cookie validation, token/user lookup details, propagated headers, and failure branches.
- [x] `authz-casbin` — all 21 pinned blocks map exactly once to standalone schema, metadata, inline, file, disabled, route-policy-shape, and request behavior. Deny-to-allow and policy1-to-policy2-to-policy1 transitions use atomic standalone snapshot replacement, a side-effect-free applied-state probe, and one consuming request. The shared watcher survives invalid/Remove/Rename/Create/Write sequences and later valid snapshots; workdir file paths are confined. Package/corpus, watcher recovery, race, real-process `-count=10`, scoped lint/build, post-integration, and task-review gates pass.

#### Task 7 — Limits and Cache

- [ ] `limit-conn` — six two-request scenarios do not preserve real concurrent/in-flight counters, dynamic variable failures, auth/global-rule interactions, Redis authentication/reuse, or HTTP/2 behavior.
- [ ] `limit-count` — repeated three-request probes omit much of the 252-block fixed/sliding window, delayed-sync, Sentinel/Cluster, group sharing, metadata, reset header, and state-transition matrix.
- [ ] `limit-req` — simple burst probes omit delay versus `nodelay`, atomic Redis concurrency, shared routes, variable errors, authentication, degradation, and HTTP/2 cases.
- [ ] `graphql-limit-count` — one generic GraphQL quota request replaces fragments, cost/depth calculation, local/Redis/Cluster state, quota transitions, and schema rejection.
- [ ] `proxy-cache` — disk and memory MISS/HIT only; bypass/no-cache, expiry/TTL, Cache-Control, Set-Cookie, Vary, PURGE, consumer isolation, invalid zones, and persistence remain.
- [ ] `graphql-proxy-cache` — three MISS/HIT probes omit method/body/variables keying, invalid GraphQL requests, mutation bypass, purge failures, Vary, route/host isolation, and consumer isolation.

#### Task 8 — Routing, Workflow, Batch, and Dubbo

- [x] `proxy-mirror` — all 36 pinned blocks across three sources map exactly once to standalone schema, exact primary/mirror traffic, HTTP version, live deletion, bounded concurrent sampling, path/DNS behavior, proxy-rewrite ordering, h2c gRPC, and grpc-web cases. A generic one-shot finalized-request hook applies rewritten URI and method before ordinary or AI terminals; h2c is explicitly configured, bounded counts observe their full window, concurrent captures reject, and DNS diagnostics await the bounded mirror timeout. Affected package/race/harness, full real-process, DNS/sensitive repeats, scoped lint, build, post-integration gates, and task review pass.
- [ ] `traffic-split` — five header-selected routes replace weighted inline/resource upstreams, fallback/zero weights, chash, pass-host, HTTPS, health/retry, timeout, and reload behavior.
- [ ] `workflow` — four happy-path actions omit no-case behavior, ordered fallthrough, isolated action state, invalid/no rules, limit-conn/global-rule interactions, and rewrite/log phase interaction.
- [ ] `batch-requests` — three probes omit most pipeline validation, timeout/partial aggregation, body-file and size limits, header copying, metadata limits, custom URI, and mixed HTTP/gRPC subresponses.
- [ ] `http-dubbo` — one generic source case omits POJO/array serialization, request ID/frame assertions, void/application responses, timeouts, and connect failures.

#### Task 9 — HTTP and Cloud Loggers

- [ ] `http-logger` — one `/probe` delivery per source does not assert JSON/newline/custom formats, request/response bodies and truncation, batching/retry, metadata, or exact sink payloads.
- [x] `clickhouse-logger` — all 23 pinned blocks map exactly once to real standalone cases. The corpus validates required/default/schema configuration, ClickHouse user/key/database headers, JSONEachRow SQL bodies, single and multiple endpoints, deterministic pending-entry overflow against a cancellable slow sink, request/response body capture and expressions, plugin metadata formats, and child-scoped `$ENV://` user resolution including empty values. Package, race, strict corpus, environment isolation, real-process, scoped lint, diff, build, post-integration, and task-review gates pass.
- [ ] `google-cloud-logging` — generic delivery omits OAuth/JWT exchange, monitored-resource/log fields, batching, credentials, endpoint, and error handling.
- [ ] `loggly` — generic delivery omits token/tag endpoint construction, format/body fields, batching, timeout, and failure behavior.
- [x] `loki-logger` — all 22 pinned blocks map exactly once to real standalone cases. The corpus validates schema branches, tenant and authorization headers, custom endpoints, rich nested default records, static/dynamic/post-upstream labels, stable request timestamps, route and metadata format precedence, additive non-clobber extras, exact stream/value grouping and cardinality, three-request label isolation, and deterministic pending-entry overflow. Internal batch state uses a typed envelope so user fields cannot collide. Package, race, strict semantic matcher, repeated real-process, scoped lint, build, post-integration, and task-review gates pass.
- [ ] `datadog` — package-level metadata defaults/schema and exact DogStatsD 8192-byte coalescing versus 8193-byte ordered fallback are TDD-tested, task-reviewed, integrated, and race-clean. The 13-block manifest remains incomplete: block 11 requires the shared embedded-wildcard route prerequisite above so `/articles/*/comments` both matches with APISIX arbitrary-depth semantics and remains the exact plugin path tag; the other source-specific metric/tag/cardinality scenarios must then replace the stale generic six-datagram case.
- [ ] `elasticsearch-logger` — generic delivery omits index/type/auth configuration, bulk framing, log formats, batching/retry, and response failures.
- [ ] `rocketmq-logger` — generic delivery omits nameserver/topic/access-key signing, body/log formats, batching, timeout, and error behavior.
- [ ] `sls-logger` — generic delivery omits Aliyun signing, project/logstore endpoint, structured log groups, credentials, batching, and failures.
- [x] `splunk-hec-logging` — all 17 pinned blocks map exactly once to real standalone cases. The corpus validates schema diagnostics, non-blocking HEC auth failures, exact token/channel/content headers, rich and custom event envelopes, post-upstream variable resolution, three-event concatenated batching, keepalive configuration, standalone ciphertext decryption, additive non-clobber metadata extras, and deterministic pending-entry overflow. Package, race, strict corpus, repeated sensitive real-process, scoped lint, build, post-integration, and task-review gates pass.
- [ ] `tencent-cloud-cls` — generic delivery omits CLS signing, topic/endpoint, protobuf/log payload semantics, credentials, batching, and failures.

#### Task 10 — Network, Kafka, File, and Error Loggers

- [ ] `tcp-logger` — one generic TCP write replaces framing/newlines, custom formats, body truncation, batch flush, reconnect, TLS, and failure cases.
- [x] `udp-logger` — all 14 pinned blocks map exactly once to standalone schema, delivery-failure, live two-sink reload, metadata, and exact request/response-body cases. Default records expose the APISIX-shaped access-log fields with explicit Go-native size approximations; custom records resolve post-downstream and append route/service context. Parsed RFC3339 and strict RFC 6901 network assertions, package/race/corpus, full real-process, reload `-count=10`, sensitive `-count=3`, scoped lint, build, post-integration gates, and task review pass.
- [ ] `syslog` — one generic transport write replaces RFC framing/facility/severity/tag, TCP/UDP/TLS modes, batching, and failures.
- [ ] `kafka-logger` — one produce per source omits topic/key/partition, metadata negotiation, SASL/TLS, body truncation, formats, batching/retry, and broker failures.
- [ ] `file-logger` — generic file output does not preserve append/reopen, exact body/log formats, shutdown flush, path/schema, and failure cases.
- [ ] `log-rotate` — generic file assertions omit size/time rotation, retention counts, reopen lifecycle, metadata/config changes, and exact rotated contents.
- [ ] `error-log-logger` — four pass-through requests configure `{}` and do not test log levels, metadata initialization/update/removal, or ClickHouse/Kafka/SkyWalking delivery.
- [x] `skywalking-logger` — all 15 pinned blocks map exactly once to real standalone cases. The corpus validates minimal/full/missing-endpoint schema paths, exact SkyWalking envelope arrays and nested JSON records, hostname service instances, valid and malformed `sw8` trace context, metadata and route format precedence with route/service identity, exact request/response body capture, and deterministic pending-entry overflow. A typed semantic matcher enforces envelope cardinality, trace presence/absence, and nested payload fields. Package, race, strict corpus, repeated real-process, scoped lint, build, post-integration Loki compatibility, and task-review gates pass.

#### Task 11 — Tracing

- [ ] `opentelemetry` — one export per source omits semantic protobuf decoding, sampling, trace/span IDs, propagation, resource/route attributes, body limits, batching, HTTP/gRPC errors, and shutdown flush.
- [ ] `skywalking` — generic export cases omit trace propagation, segment/span/log semantics, sampling, metadata, body capture, collector failures, and shutdown delivery.

#### Task 12 — Bounded AI Plugins

- [ ] `ai-aws-content-moderation` — one request per source omits real SigV4 assertions, encrypted credentials, threshold/category decisions, endpoint/TLS, replay, and rejection behavior.
- [x] `ai-prompt-decorator` — all 17 pinned blocks map exactly once to real Chat Completions and Responses API requests. Standalone cases assert prepend, append, both, request-to-request isolation, invalid empty configuration, instructions creation/prepend, string/array input append, combined transformations, and the Chat regression with semantic upstream JSON. The shared `json_equals` matcher preserves arbitrary numeric precision, defines mathematical number equality, preserves array order, ignores object-key order, rejects malformed JSON and non-body scopes, and expands iteration placeholders. Package, matcher/corpus, full tests, real-process, scoped lint/build, post-integration, and task-review gates pass.
- [ ] `ai-prompt-guard` — one allow/deny probe replaces pattern/case/role combinations, custom rejection, malformed requests, and streaming behavior.
- [ ] `ai-rag` — one generic provider request omits embedding/retrieval exchanges, constructed context/prompt, headers, failures, and streaming.
- [ ] `ai-rate-limiting` — one quota probe per source omits token estimation, windows/counters, consumer isolation, expressions, headers, and rejection transitions.
- [ ] `ai-request-rewrite` — one rewritten request per source omits message/prompt variants, variables, provider formats, body preservation, malformed JSON, and schema rejection.

#### Task 13 — AI Proxy

- [ ] `ai-proxy` — nineteen generic provider requests claim 303 blocks; provider-specific conversions, tools/media, schema/error mapping, SSE/EventStream chunks, usage summaries, upstream variables, limits, flushes, and client disconnects remain.

### Corrected Completion Gate

- [ ] Every unchecked plugin above has behavior-specific standalone cases and focused integration evidence.
- [ ] The source-behavior ledger and semantic gate reject the former generic manifests.
- [ ] All Task 14 repository, integration, review, documentation, and publication gates pass on the corrected corpus.

---

## Corrected Self-Review Results

- **Inventory:** The ledger contains the exact 61 unique manifests from Tasks 4-13: 20 task-review-approved and 41 remaining.
- **Behavioral placeholders:** Thirty manifests use a generic source-file case pattern; the named manifests were separately checked for claimed blocks that have no behaviorally equivalent request or assertion.
- **Harness gaps:** Task 3 protocol coverage and Task 13 streaming/disconnect primitives remain unchecked and are listed before the plugin ledger.
- **Completion boundary:** Task 14 and PR readiness remain unchecked until all 41 remaining manifests, the strengthened semantic gate, and the complete repository gates pass.

## Recheck: 2026-07-18

The pinned source titles and standalone resources/assertions are being rechecked
manifest by manifest. Passing focused package and real-process tests is necessary
but does not restore a checkbox until a task review confirms source-complete
behavior. `consumer-restriction` and `traffic-label` were initially unchecked
after their reviews found concrete gaps. Both have since passed their follow-up
reviews and post-integration gates. The currently approved scope is **20
complete and 41 remaining**; `oas-validator` also passed its task review with
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

- **Structural source-file stand-ins (30):** `ai-aws-content-moderation`,
  `ai-prompt-guard`, `ai-proxy`, `ai-rag`,
  `ai-rate-limiting`, `ai-request-rewrite`,
  `authz-keycloak`, `datadog`,
  `elasticsearch-logger`, `error-log-logger`, `feishu-auth`, `file-logger`,
  `google-cloud-logging`, `graphql-limit-count`,
  `http-dubbo`, `http-logger`, `kafka-logger`, `ldap-auth`, `log-rotate`,
  `loggly`, `multi-auth`, `openid-connect`, `opentelemetry`,
  `rocketmq-logger`, `skywalking`,
  `sls-logger`, `syslog`, `tcp-logger`,
  `tencent-cloud-cls`, and `wolf-rbac`. Each maps a whole
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
- **Task-review-approved (20):** `ai-prompt-decorator`, `authz-casbin`,
  `brotli`, `clickhouse-logger`, `consumer-restriction`, `cors`, `fault-injection`,
  `basic-auth`, `cas-auth`, `forward-auth`, `hmac-auth`, `jwe-decrypt`, `loki-logger`, `oas-validator`, `proxy-mirror`, `request-validation`, `skywalking-logger`, `splunk-hec-logging`, `traffic-label`, and `udp-logger`. No other
  manifest moved to checked status in this recheck.

## Complexity and Parallel Execution Replan: 2026-07-18

The classification audit started with 56 unchecked manifests at commit
`335203d`. Its consumer-restriction review then approved that manifest, so the
active execution tiers below contained 55 remaining manifests before Easy
Wave 1. `traffic-label`, `authz-casbin`, `ai-prompt-decorator`, and
`clickhouse-logger`, `splunk-hec-logging`, `jwe-decrypt`, `loki-logger`,
`skywalking-logger`, `udp-logger`, `forward-auth`, `proxy-mirror`, and
`basic-auth`, `cas-auth`, and `hmac-auth` have now passed review, so **41 remain**.
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

### Medium — 24 manifests

- [ ] `datadog`
- [x] `basic-auth`
- [x] `hmac-auth`
- [x] `forward-auth`
- [ ] `multi-auth`
- [ ] `wolf-rbac`
- [x] `cas-auth`
- [ ] `feishu-auth`
- [ ] `graphql-limit-count`
- [ ] `graphql-proxy-cache`
- [x] `proxy-mirror`
- [ ] `traffic-split`
- [ ] `workflow`
- [ ] `batch-requests`
- [ ] `http-logger`
- [ ] `google-cloud-logging`
- [ ] `loggly`
- [ ] `elasticsearch-logger`
- [ ] `sls-logger`
- [ ] `tencent-cloud-cls`
- [ ] `tcp-logger`
- [ ] `syslog`
- [ ] `file-logger`
- [ ] `log-rotate`
- [ ] `skywalking`
- [ ] `ai-aws-content-moderation`
- [ ] `ai-prompt-guard`
- [ ] `ai-rag`
- [ ] `ai-request-rewrite`

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
   proxy-rewrite/grpc-web ordering. Only after that reviewed range is integrated
   may `traffic-split` and `batch-requests` run in parallel through their
   distinct remaining owners.
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

### Hard — 17 manifests

- [ ] `key-auth`
- [ ] `jwt-auth`
- [ ] `ldap-auth`
- [ ] `openid-connect`
- [ ] `authz-keycloak`
- [ ] `saml-auth`
- [ ] `limit-conn`
- [ ] `limit-count`
- [ ] `limit-req`
- [ ] `proxy-cache`
- [ ] `http-dubbo`
- [ ] `rocketmq-logger`
- [ ] `kafka-logger`
- [ ] `error-log-logger`
- [ ] `opentelemetry`
- [ ] `ai-rate-limiting`
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
- Current full-suite baseline is not green: `t/plugin` has two unrelated
  `chaitin-waf` expectation mismatches (`metadata-rejects-empty-nodes` and
  `metadata-requires-node-host`), and repository lint has five existing
  findings outside the current consumer fix. Workers must report these exact
  baseline failures separately and may not claim `go test ./...` or repository
  lint passed until they are resolved.
