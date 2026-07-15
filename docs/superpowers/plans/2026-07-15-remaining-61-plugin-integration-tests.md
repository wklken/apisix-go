# Remaining 61 Plugin Integration Tests Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the remaining 61 generated `t/plugin/*.yaml` placeholders with source-complete standalone integration scenarios that configure the named APISIX-Go plugin, start the real binary, send real requests, and assert plugin-produced behavior.

**Architecture:** Keep the strict manifest contract and one-child-process-per-scenario isolation introduced by PR #4. Extend only the test harness capabilities required by pinned Apache cases, then convert plugins in dependency-ordered waves; external systems are deterministic loopback fixtures, while every behavior assertion remains at the APISIX-Go boundary. A converted manifest is accepted only when every source number is mapped once, every case/variant activates its target plugin, its focused real-process integration run passes, and any exposed production defect has a focused RED-then-GREEN package test.

**Tech Stack:** Go 1.26.4, `go test`, `go.yaml.in/yaml/v3`, `net/http`, `net`, `httptest`, `os/exec`, standalone APISIX YAML, bbolt, existing repository dependencies, GitHub CLI.

## Global Constraints

- Work on `codex/plugin-integration-tests`; keep PR #4 draft until all final gates pass.
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

`{{FIXTURE.<name>.ADDR}}`, `.HOST`, `.PORT`, and `.URL` continue to work. Add `{{WORK_DIR}}` for files created inside the scenario's temporary directory. Network fixture kinds are exactly `tcp`, `udp`, `grpc`, `redis`, `redis-cluster`, `redis-sentinel`, `kafka`, `dubbo`, and `ldap`; unknown kinds fail manifest validation.

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

- [x] **Step 1: Write RED tests for the exact commands/protocols used by the remaining sources**

```go
func TestRedisFixtureSupportsPluginCommands(t *testing.T) {
    client := redis.NewClient(&redis.Options{Addr: fixture.address()})
    require.NoError(t, client.Set(ctx, "quota", "1", time.Minute).Err())
    require.Equal(t, int64(2), client.Incr(ctx, "quota").Val())
    require.Equal(t, "1", client.HGet(ctx, "hash", "field").Val())
}
```

Use the repository's existing Redis client import, not a new package. Add protocol tests for `AUTH`, `SELECT`, `GET`, `SET` with `NX/PX/EX`, `INCR`, `INCRBY`, `DECR`, `EXPIRE`, `PTTL`, `DEL`, `HGET/HSET`, `EVAL/EVALSHA`, cluster `MOVED`, Sentinel `get-master-addr-by-name`, Kafka metadata/produce acknowledgements, Dubbo request ID and Hessian response frames, and LDAP bind/search success/failure.

- [x] **Step 2: Run protocol tests RED**

```bash
bash -lc 'source .envrc && go test ./t/plugin -run "Test(Redis|Kafka|Dubbo|LDAP)Fixture" -count=1 -v'
```

Expected: compile failure because fixture constructors are undefined.

- [x] **Step 3: Implement only the scripted protocol surface asserted in Step 1**

```go
func startRedisFixture(spec FixtureSpec) (*networkFixture, error)
func startRedisClusterFixture(spec FixtureSpec) (*networkFixture, error)
func startRedisSentinelFixture(spec FixtureSpec) (*networkFixture, error)
func startKafkaFixture(spec FixtureSpec) (*networkFixture, error)
func startDubboFixture(spec FixtureSpec) (*networkFixture, error)
func startLDAPFixture(spec FixtureSpec) (*networkFixture, error)
```

Keep state in per-fixture maps guarded by a mutex. Do not implement a general server: reject any command/API key not listed in Step 1 and include the unexpected payload in the test failure.

- [x] **Step 4: Run all protocol fixtures GREEN and commit**

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

- [x] **Step 1: Convert one manifest at a time using the canonical shape**

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

- [x] **Step 2: For each plugin, prove RED then GREEN**

Run before editing and after conversion, replacing `<plugin>` with each table row:

```bash
bash -lc 'source .envrc && go test ./t/plugin -run "^TestManifestCorpusExercisesTargetPlugins/<plugin>$" -count=1'
bash -lc 'source .envrc && go test ./t/plugin -run "TestPluginIntegration/<plugin>" -count=1 -v'
```

Expected before: semantic gate FAIL. Expected after: semantic gate and integration PASS. If integration exposes a mismatch, add `Test<Behavior>` in the matching package, observe RED, make the smallest production fix, and rerun package plus manifest.

- [x] **Step 3: Run the wave gate and commit**

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
- Produces: 5 real manifests covering 18 sources and 301 blocks.

- [x] **Step 1: Translate the exact behavior sets**

| Manifest | Blocks | Required behavior groups |
|---|---:|---|
| `key-auth` | 34 | header/query/cookie keys, hide credentials, anonymous consumer, realm, duplicate/missing/invalid keys, consumer/group attachment |
| `basic-auth` | 44 | Basic parsing, malformed base64, username/password lookup, anonymous consumer, realm, duplicate headers, consumer/group attachment |
| `jwt-auth` | 130 | token issue endpoint, header/query/cookie extraction, HS/RS/ES algorithms, exp/nbf/leeway, base64 secret, public keys, key claims, anonymous/realm, hide credentials |
| `hmac-auth` | 70 | canonical string/signature algorithms, clock skew, signed headers/body digest, query order, escape rules, anonymous/realm, failure messages |
| `jwe-decrypt` | 23 | compact JWE extraction, protected headers, supported algorithms/encodings, key selection, header forwarding, malformed/decryption failures |

Use test-local fixed keys copied from the pinned sources or existing package test fixtures; never generate assertions from the implementation under test.

- [x] **Step 2: Run each semantic and real-process gate RED then GREEN**

```bash
for plugin in key-auth basic-auth jwt-auth hmac-auth jwe-decrypt; do
  bash -lc "source .envrc && go test ./t/plugin -run '^TestManifestCorpusExercisesTargetPlugins/'\"$plugin\"'$' -count=1"
  bash -lc "source .envrc && go test ./t/plugin -run 'TestPluginIntegration/'\"$plugin\" -count=1 -v"
done
```

- [x] **Step 3: Run packages and commit**

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
- Produces: 10 real manifests covering 30 sources and 407 blocks.

- [x] **Step 1: Extend `fixture_auth_test.go` with deterministic endpoint scripts**

Provide named modes selected by fixture response sequences, not plugin-specific shortcuts: LDAP bind/search; OIDC discovery/JWKS/introspection/authorize/token/userinfo/revoke/end-session; forward-auth allow/deny with copied headers; Keycloak/Wolf permission endpoints; CAS service validation XML; SAML metadata/login/ACS/logout payloads; Feishu token/user endpoints. Every fixture request must assert method, path, authorization, form/query/body, and TLS choice from the pinned source.

- [x] **Step 2: Convert and verify the exact behavior sets**

| Manifest | Blocks | Required behavior groups |
|---|---:|---|
| `ldap-auth` | 35 | bind/search, consumer DN mapping, Basic realm, TLS/schema and auth failures |
| `openid-connect` | 141 | bearer/introspection/JWT, discovery/JWKS, client auth modes, scopes/claims, session+PKCE, Redis session, renewal/logout/revocation, proxy/TLS/header behavior |
| `forward-auth` | 28 | request forwarding, auth response status/body/header propagation, upstream headers, URI/body modes, timeout/TLS |
| `multi-auth` | 38 | ordered auth alternatives, consumer propagation, anonymous behavior, schema and failure precedence |
| `wolf-rbac` | 42 | token extraction, permission checks, consumer headers, cache/error/schema behavior |
| `authz-keycloak` | 45 | discovery/token/UMA decisions, lazy paths, permissions, client credentials, timeout/TLS/error handling |
| `cas-auth` | 22 | redirect, ticket validation, cookies, callback/original URI, logout and invalid XML/tickets |
| `saml-auth` | 21 | metadata, AuthnRequest redirect/form, signed response ACS, relay state, session cookie, logout and invalid assertions |
| `feishu-auth` | 14 | authorize redirect, callback token/user lookup, state/cookie, headers and failure branches |
| `authz-casbin` | 21 | model/policy inline and metadata resources, request variable mapping, allow/deny and invalid model/policy |

Run the same two per-plugin commands from Task 4. `openid-connect`, `saml-auth`, and `cas-auth` must use captured state/cookies across ordered steps; no precomputed successful response may bypass the target plugin.

- [x] **Step 3: Run wave packages and commit**

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

- [x] **Step 1: Convert stateful sequences without restarting their APISIX process**

| Manifest | Blocks | State that must remain within one case |
|---|---:|---|
| `limit-conn` | 104 | concurrent/in-flight counters, delays, local/Redis/cluster, variables and rejection headers |
| `limit-count` | 252 | fixed/sliding windows, local/Redis/cluster/sentinel, rules/groups, consumer isolation, delayed sync, metadata headers |
| `limit-req` | 89 | leaky bucket burst/delay/nodelay, shared counters, Redis/cluster, variables and rejection behavior |
| `graphql-limit-count` | 26 | GraphQL cost/depth, fragments, local/Redis/cluster quotas and schema rejection |
| `proxy-cache` | 76 | memory/disk MISS/HIT/EXPIRED/BYPASS, keys, methods/status/TTL, Vary, cache-control, Set-Cookie, purge and persistence |
| `graphql-proxy-cache` | 48 | memory/disk GraphQL keys, POST bodies, Vary/purge, shared zones and invalid zone configuration |

Use `repeat`, `wait`, and ordered steps for source windows; do not split a counter/cache lifecycle into variants. Render disk paths as `{{WORK_DIR}}/cache/<zone>`.

- [x] **Step 2: Run per-plugin RED/GREEN gates and focused package tests**

Use Task 4's two commands for each plugin. Run concurrency-sensitive packages with race detection:

```bash
bash -lc 'source .envrc && go test -race ./pkg/plugin/limit_conn ./pkg/plugin/limit_count ./pkg/plugin/limit_req ./pkg/plugin/graphql_limit_count ./pkg/plugin/proxy_cache ./pkg/plugin/graphql_proxy_cache -count=1'
```

- [x] **Step 3: Commit the stateful wave**

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

- [x] **Step 1: Convert the required behavior groups**

| Manifest | Blocks | Required behavior groups |
|---|---:|---|
| `proxy-mirror` | 36 | mirrored method/path/headers/body, percentage, timeout, host/scheme, primary response independence, mirror failure |
| `traffic-split` | 94 | ordered match vars, weighted inline/resource upstreams, fallback, zero weights, chash keys, pass-host and timeout propagation |
| `workflow` | 42 | no-case behavior, ordered rules, nested vars, plugin config execution, skip/break semantics and invalid expressions |
| `batch-requests` | 46 | HTTP batch parsing, per-entry method/path/headers/body, response aggregation, limits/failures, gRPC entries |
| `http-dubbo` | 5 | serialized POJO/array request frames, timeout/connect failure, void response, application failure status |

For traffic splitting, send enough deterministic requests to prove every explicit 0/100 branch; do not assert probabilistic ratios. For mirroring, assert both primary client response and mirror fixture capture.

- [x] **Step 2: Run focused integrations and packages**

```bash
bash -lc 'source .envrc && go test ./t/plugin -run "TestPluginIntegration/(proxy-mirror|traffic-split|workflow|batch-requests|http-dubbo)" -count=1 -v'
bash -lc 'source .envrc && go test ./pkg/plugin/proxy_mirror ./pkg/plugin/traffic_split ./pkg/plugin/workflow ./pkg/plugin/batch_requests ./pkg/plugin/http_dubbo ./pkg/route ./pkg/proxy -count=1'
```

- [x] **Step 3: Commit**

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

- [x] **Step 1: Convert every delivery lifecycle**

Each manifest must cover its source schema, log-format variables, request/response body truncation, batch size, inactive timeout, retry/status handling, authentication/signature headers, endpoint URI, TLS verification, and shutdown flush. Use fixed timestamps/credentials from upstream when signatures are asserted. Fixture bodies use regex only for nondeterministic timestamps/IDs; assert all stable JSON fields and batch cardinality.

- [x] **Step 2: Run per-plugin semantic/integration gates and logger packages**

```bash
for plugin in http-logger clickhouse-logger google-cloud-logging loggly loki-logger datadog elasticsearch-logger rocketmq-logger sls-logger splunk-hec-logging tencent-cloud-cls; do
  bash -lc "source .envrc && go test ./t/plugin -run 'TestPluginIntegration/'\"$plugin\" -count=1 -v"
done
bash -lc 'source .envrc && go test ./pkg/plugin/http_logger ./pkg/plugin/clickhouse_logger ./pkg/plugin/google_cloud_logging ./pkg/plugin/loggly ./pkg/plugin/loki_logger ./pkg/plugin/datadog ./pkg/plugin/elasticsearch_logger ./pkg/plugin/rocketmq_logger ./pkg/plugin/sls_logger ./pkg/plugin/splunk_hec_logging ./pkg/plugin/tencent_cloud_cls ./pkg/plugin/logger_batch -count=1'
```

- [x] **Step 3: Commit**

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

- [x] **Step 1: Convert transport and filesystem assertions**

Cover schema, JSON/custom formats, batch framing/newlines, body inclusion/truncation, inactive flush, reconnect/retry, TLS where present, Kafka topic/key/partition/SASL behavior, file append/reopen, rotation count/size/time, error-log source levels, and SkyWalking log envelope. Paths must remain under `{{WORK_DIR}}`; assert file content only after APISIX shutdown.

- [x] **Step 2: Run integrations and package tests**

```bash
bash -lc 'source .envrc && go test ./t/plugin -run "TestPluginIntegration/(tcp-logger|udp-logger|syslog|kafka-logger|file-logger|log-rotate|error-log-logger|skywalking-logger)" -count=1 -v'
bash -lc 'source .envrc && go test ./pkg/plugin/tcp_logger ./pkg/plugin/udp_logger ./pkg/plugin/syslog ./pkg/plugin/kafka_logger ./pkg/plugin/file_logger ./pkg/plugin/log_rotate ./pkg/plugin/error_log_logger ./pkg/plugin/skywalking_logger -count=1'
```

- [x] **Step 3: Commit**

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

- [x] **Step 1: Convert OpenTelemetry and SkyWalking behavior**

Cover schema, trace/span IDs, sampling, route/service/resource attributes, W3C/B3/SkyWalking propagation, upstream headers, collector HTTP/gRPC export, batching, error status, plugin metadata, body capture limits, and shutdown delivery. Decode protobuf with existing repository protobuf types in the fixture before asserting semantic fields; do not compare nondeterministic serialized bytes.

- [x] **Step 2: Run integrations and package tests**

```bash
bash -lc 'source .envrc && go test ./t/plugin -run "TestPluginIntegration/(opentelemetry|skywalking)" -count=1 -v'
bash -lc 'source .envrc && go test ./pkg/plugin/otel ./pkg/plugin/skywalking -count=1'
```

- [x] **Step 3: Commit**

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

- [x] **Step 1: Convert exact AI behavior sets**

| Manifest | Blocks | Required behavior groups |
|---|---:|---|
| `ai-aws-content-moderation` | 23 | credentials/encryption, SigV4, endpoint/TLS, category/toxicity thresholds, request replay and rejection |
| `ai-prompt-decorator` | 17 | prepend/append system/user messages, provider body shapes, streaming preservation and schema errors |
| `ai-prompt-guard` | 44 | allow/deny patterns, case handling, message roles, custom rejection, streaming and malformed requests |
| `ai-rag` | 17 | embedding/retrieval fixtures, prompt/context construction, headers, failures and streaming |
| `ai-rate-limiting` | 58 | token extraction/estimation, local counters, consumer isolation, expressions, headers, windows and rejection |
| `ai-request-rewrite` | 19 | prompt/message rewriting, variables, provider formats, body preservation and invalid JSON/schema |

- [x] **Step 2: Run real-process and package gates, then commit**

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

- [x] **Step 1: Add the remaining streaming primitives with RED harness tests**

Add `HTTPInput.DisconnectAfterBytes int` and `HTTPOutput.Chunks []Matcher`. The client closes the response body after the configured byte count; chunk assertions observe flush boundaries without changing payload content. Add an AWS EventStream fixture response encoded from fixed headers/payload/CRC values copied from the pinned Bedrock sources.

Run:

```bash
bash -lc 'source .envrc && go test ./t/plugin -run "TestHarness(DisconnectsClient|AssertsFlushedChunks|RunsAWSEventStreamFixture)" -count=1 -v'
```

Expected RED before implementation and PASS after.

- [x] **Step 2: Convert all 19 source files as separate behavior groups**

Preserve: OpenAI-compatible chat/embeddings, Anthropic request/SSE conversion, Azure paths/version/auth, Gemini, OpenRouter, Vertex auth/body, Bedrock SigV4/EventStream, passthrough mode, protocol conversion, request-body override, upstream variables, streaming limits/duration/flush, client disconnect, provider error mapping, usage/log summaries, and schema validation. Every provider request must be asserted by its fixture; every SSE/EventStream case must assert both client chunks and complete semantic payload.

- [x] **Step 3: Run the 303-block manifest and AI packages**

```bash
bash -lc 'source .envrc && go test ./t/plugin -run "^TestManifestCorpusExercisesTargetPlugins/ai-proxy$" -count=1'
bash -lc 'source .envrc && go test ./t/plugin -run "TestPluginIntegration/ai-proxy" -count=1 -v'
bash -lc 'source .envrc && go test ./pkg/plugin/ai_proxy ./pkg/plugin/ai_runtime -count=1'
```

- [x] **Step 4: Commit**

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

- [x] **Step 1: Recount pinned sources independently of YAML**

```bash
git -C .cache/apache-apisix checkout c3d7d5ec69774121f53d2e20d29d09c816795dd7
rg -c '^=== TEST ' .cache/apache-apisix/t/plugin > .cache/pinned-test-counts.txt
bash -lc 'source .envrc && go test ./t/plugin -run "TestManifestCorpusValidates|TestDocumentedPluginManifests|TestManifestCorpusExercisesTargetPlugins" -count=1'
```

Expected: all tests PASS; 98 source-backed plugin names plus `redirect2`; zero case/variant target-plugin failures. Compare every manifest `source.tests` with the corresponding `rg` count, including nested source paths.

- [x] **Step 2: Run the complete real-process suite**

```bash
bash -lc 'source .envrc && make test-integration'
```

Expected: PASS with no skipped tests, no placeholder manifests, no leaked child processes/listeners, and no external network dependency.

- [x] **Step 3: Update documentation from live counts**

Record the verified complete-manifest counts in the README and plugin status
docs. Document all added fixture kinds and `{{WORK_DIR}}`; remove stale design
statements that live external services or explicit skips remain unsupported.
Mark every checkbox in this plan only after its command has passed.

- [x] **Step 4: Run repository completion gates**

```bash
bash -lc 'source .envrc && go test ./... -count=1'
bash -lc 'source .envrc && make lint'
bash -lc 'source .envrc && make build'
git diff --check
git status --short
```

Expected: all commands PASS; `git status --short` contains only the intended source, manifest, harness, plan, and documentation files; remove the generated `./apisix` binary if present.

- [x] **Step 5: Perform merge-level review**

Use `agent-skills:code-review-and-quality`. Review target-plugin authenticity, fixture self-fulfilling assertions, source-number grouping, process/network cleanup, protocol parser bounds, secret leakage, flaky waits/randomness, and unrelated diffs. Repair only verified findings and rerun affected focused plus final gates.

- [x] **Step 6: Commit, push, and mark PR ready**

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

## Self-Review Results

- **Spec coverage:** All 61 live semantic-gate failures appear exactly once across Tasks 4-13. Their batch totals reconcile to 205 pinned source files and 3,181 upstream `TEST` blocks. Harness work precedes every plugin family that depends on it. The final task covers source recounting, zero skips/placeholders, complete real-binary execution, documentation, review, and publication.
- **Placeholder scan:** The plan contains no deferred implementation markers. Each task names exact files, behavior groups, commands, expected RED/GREEN outcomes, and commit boundaries.
- **Type consistency:** Fixture type and placeholder names are defined once in the file/interface map and consumed unchanged by Tasks 2-13. `opentelemetry` correctly maps to `pkg/plugin/otel`; all other package paths use the live repository names.
- **Scope split:** The 61 plugins span independent subsystems, so Tasks 4-13 are intentionally separate reviewable sub-projects. Each produces a passing plugin wave and can be accepted or rejected independently.
