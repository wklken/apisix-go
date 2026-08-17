# Production Readiness Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Eliminate the remaining pre-release fail-open route and candidate-profile safety gaps, bound frontend body reads, and make plugin support-matrix changes continuously verified without claiming that post-merge release qualification already exists.

**Architecture:** Preserve Apache APISIX 3.17 compatibility in the empty compatibility profile, but make `http-data-plane-v1` an explicitly safer contract. Route decoding retains the three upstream fields currently lost by Go: singular `host` is normalized into the existing host dispatcher, while unsupported singular `remote_addr` and `script_id` are rejected during `BuildStrict`. A focused production-policy layer validates effective authentication plugin configs and resolved TLS upstreams before plugin construction. Static profile validation requires a positive body timeout, and a dedicated lightweight workflow owns `docs/plugins.md` to manifest consistency.

**Tech Stack:** Go 1.26, `github.com/goccy/go-json`, chi route dispatch, bbolt-backed configuration snapshots, GitHub Actions, shell contract tests.

**Spec:** [docs/production-readiness-review-2026-08-16.md](../../production-readiness-review-2026-08-16.md), especially sections 4, 6.2, 6.3, and 9; operator contract in [docs/production-profile.md](../../production-profile.md).

## Global Constraints

- Base revision is `master@5f2df72628ef9b3f6ac6f8c0517ba3a6d9dfc397`; refresh the plan if `master` changes any touched interface before delivery.
- Target Go 1.26 from `go.mod`. Run `source .envrc` before every `go`, `go test`, `go build`, or `make` command.
- Verification is impact-scoped. Do not run `go test ./...`, `go test ./pkg/...`, `make test`, or the whole `t/plugin` integration package.
- Do not add dependencies or run `make dep`.
- Preserve compatibility-profile APISIX defaults. Authentication and upstream-TLS enforcement applies only when `deployment.profile == http-data-plane-v1`.
- Do not implement route `vars`, `remote_addr(s)`, Lua `script`, or `script_id`. Unsupported non-empty values fail route compilation and retain the last-good generation.
- Do not remove the README, CLI, production-profile, or ledger “not production ready” warning.
- Do not create a tag, publish an image/release, mutate GitHub branch/environment settings, or claim RC/final/rollback qualification in this PR.
- Format touched Go files with `golangci-lint fmt`, inspect the resulting diff, and retain only task-related changes.

## File map

| File | Responsibility |
| --- | --- |
| `pkg/resource/route.go` | Preserve singular route `host`, `remote_addr`, and `script_id` plus field-presence state. |
| `pkg/resource/route_test.go` | Prove route JSON presence and normalization inputs survive decoding. |
| `pkg/route/builder.go` | Register effective host constraints and invoke production policy before constructing handlers. |
| `pkg/route/route_host_test.go` | Prove singular host is restrictive and host/hosts conflicts fail closed. |
| `pkg/route/unsupported_semantics_test.go` | Prove `remote_addr` and `script_id` reject with route identity. |
| `pkg/route/production_policy.go` | Own candidate-profile authentication and upstream TLS admission rules. |
| `pkg/route/production_policy_test.go` | Cover candidate and compatibility modes, inheritance, disabled plugins, and TLS upstreams. |
| `pkg/config/init.go` | Require positive `client_body_timeout` in `http-data-plane-v1`. |
| `pkg/config/release_gate_test.go` | Protect the production config/profile timeout contract. |
| `conf/config-production.yaml` | Set the checked-in 60-second body timeout. |
| `.github/workflows/plugin-status.yml` | Run only the plugin status/manifest selection gate for its source-of-truth paths. |
| `scripts/plugin_status_gate_test.sh` | Fail if the workflow loses a trigger, exact test, or read-only permission. |
| `docs/configuration.md` | Describe route field and body-timeout behavior. |
| `docs/production-profile.md` | State enforced auth/TLS/timeout rules and external ingress audit-log evidence. |
| `docs/production-readiness-review-2026-08-16.md` | Refresh the sole ledger to this implementation without closing release evidence. |
| `docs/runbooks/production-release.md` | Require branch/environment policy and external request-log evidence before qualification. |

---

### Task 1: Preserve and enforce APISIX route field semantics

**Files:**
- Modify: `pkg/resource/route.go:229-283`
- Modify: `pkg/resource/route_test.go:1-115`
- Modify: `pkg/route/builder.go:556-575,828-853`
- Create: `pkg/route/route_host_test.go`
- Modify: `pkg/route/unsupported_semantics_test.go:12-70`

**Interfaces:**
- Consumes: existing `registerRouteWithHosts`, `validateRouteSemantics`, and last-good `BuildStrict` error propagation.
- Produces: `Route.EffectiveHosts() []string`, `Route.HostConfigured() bool`, and `Route.RemoteAddrConfigured() bool`.

- [ ] **Step 1: Add failing decoder tests**

Add table-driven tests that decode these exact payloads:

```go
{"id":"singular-host","uri":"/host","host":"api.example.com"}
{"id":"singular-remote","uri":"/remote","remote_addr":"10.0.0.1"}
{"id":"script-reference","uri":"/script","script_id":"script-1"}
```

Assert `HostConfigured()` and `EffectiveHosts()` return `true` and `[]string{"api.example.com"}`, `RemoteAddrConfigured()` is true with value `10.0.0.1`, and `ScriptID` retains the JSON string `"script-1"`.

- [ ] **Step 2: Run the decoder tests red**

Run:

```bash
source .envrc && go test ./pkg/resource -run 'TestRouteUnmarshalPreservesSingularMatchingFields' -count=1
```

Expected: FAIL because the fields and methods do not exist.

- [ ] **Step 3: Extend route decoding without recursive unmarshalling**

Add these fields to `resource.Route`:

```go
Host       string          `json:"host,omitempty"`
RemoteAddr string          `json:"remote_addr,omitempty"`
ScriptID   json.RawMessage `json:"script_id,omitempty"`

hostSet       bool
remoteAddrSet bool
```

Extend the existing `UnmarshalJSON` auxiliary object with `Host *string` and `RemoteAddr *string`, set the presence flags, and retain the existing `Status *int` behavior. Add:

```go
func (r Route) HostConfigured() bool { return r.hostSet }

func (r Route) RemoteAddrConfigured() bool { return r.remoteAddrSet }

func (r Route) EffectiveHosts() []string {
	if r.hostSet {
		return []string{r.Host}
	}
	return r.Hosts
}
```

- [ ] **Step 4: Add failing route behavior tests**

Create `route_host_test.go` using the existing route-store helpers. Insert a route with `"host":"api.example.com"`, build with `BuildStrict`, and assert the same URI returns the route response for `api.example.com` but HTTP 404 for `other.example.com`. Add a second case containing both `host` and `hosts`; assert `BuildStrict` returns an error containing the route ID plus `host and hosts`.

Extend `TestBuildStrictRejectsUnsupportedRouteSemantics` with:

```go
{name: "remote_addr", field: "remote_addr", value: `"10.0.0.1"`, wantErr: "remote_addr"}
{name: "script_id", field: "script_id", value: `"script-1"`, wantErr: "script_id"}
```

- [ ] **Step 5: Run the route tests red**

Run:

```bash
source .envrc && go test ./pkg/route -run 'TestBuildStrict(SingularHost|RejectsHostAndHosts|RejectsUnsupportedRouteSemantics)' -count=1
```

Expected: wrong-host request reaches the route, or the new unsupported-field cases do not reject.

- [ ] **Step 6: Wire the effective host and fail-closed checks**

Change route registration to pass `routeResource.EffectiveHosts()`. Extend `validateRouteSemantics` with these exact invariants:

```go
if routeResource.HostConfigured() && len(routeResource.Hosts) > 0 {
	return fmt.Errorf("route %q host and hosts cannot both be configured", routeResource.ID)
}
if routeResource.HostConfigured() && strings.TrimSpace(routeResource.Host) == "" {
	return fmt.Errorf("route %q host must not be empty", routeResource.ID)
}
if routeResource.RemoteAddrConfigured() {
	return fmt.Errorf("route %q remote_addr is unsupported by the Go data plane", routeResource.ID)
}
if scriptID := bytes.TrimSpace(routeResource.ScriptID); len(scriptID) > 0 && !bytes.Equal(scriptID, []byte("null")) {
	return fmt.Errorf("route %q script_id is unsupported by the Go data plane", routeResource.ID)
}
```

- [ ] **Step 7: Run focused green tests**

Run:

```bash
source .envrc && go test ./pkg/resource -run 'TestRouteUnmarshal(PreservesSingularMatchingFields|DistinguishesOmittedAndExplicitStatus)' -count=1
source .envrc && go test ./pkg/route -run 'TestBuildStrict(SingularHost|RejectsHostAndHosts|RejectsUnsupportedRouteSemantics|AllowsEmptyVarsAndRemoteAddrs)' -count=1
```

- [ ] **Step 8: Commit Task 1**

```bash
git add pkg/resource/route.go pkg/resource/route_test.go pkg/route/builder.go \
  pkg/route/route_host_test.go pkg/route/unsupported_semantics_test.go
git commit -m "fix(route): preserve singular host and reject unsupported fields"
```

---

### Task 2: Enforce safe dynamic configuration in the candidate profile

**Files:**
- Create: `pkg/route/production_policy.go`
- Create: `pkg/route/production_policy_test.go`
- Modify: `pkg/route/builder.go:690-758`

**Interfaces:**
- Consumes: `appconfig.HTTPDataPlaneV1Profile`, `selectMaterializedPluginSources`, `parsePluginMetadata`, `resolveRouteUpstream`, and `upstreamUsesTLS`.
- Produces: `validateHTTPDataPlanePluginPolicy(map[string]resource.PluginConfig, string) error` and `validateHTTPDataPlaneUpstreamPolicy(resource.Upstream, string) error`.

- [ ] **Step 1: Write failing policy unit tests**

Cover the following table in `production_policy_test.go`:

| Profile | Config | Result |
| --- | --- | --- |
| empty compatibility | key-auth omits `hide_credentials` | pass |
| `http-data-plane-v1` | key/basic/JWT omit or set `hide_credentials:false` | reject with plugin name and `hide_credentials` |
| `http-data-plane-v1` | JWT omits `claims_to_verify` or contains only `nbf` | reject with `claims_to_verify` and `exp` |
| `http-data-plane-v1` | JWT has `hide_credentials:true`, `claims_to_verify:["exp"]` | pass |
| `http-data-plane-v1` | auth config has `_meta.disable:true` | pass because it cannot forward credentials |
| `http-data-plane-v1` | HTTPS/gRPCS upstream omits TLS or has `verify:false` | reject with `tls.verify` |
| `http-data-plane-v1` | HTTPS upstream has `tls.verify:true` | pass |
| `http-data-plane-v1` | HTTP upstream | pass |

- [ ] **Step 2: Run policy tests red**

Run:

```bash
source .envrc && go test ./pkg/route -run '^TestHTTPDataPlaneV1(Plugin|Upstream)Policy$' -count=1
```

Expected: FAIL because the policy functions do not exist.

- [ ] **Step 3: Implement the focused policy owner**

In `production_policy.go`, return immediately unless the active profile is `http-data-plane-v1`. For each present `key-auth`, `jwt-auth`, and `basic-auth` config, call `parsePluginMetadata`; skip `_meta.disable:true`; require a map config with `hide_credentials == true`. For JWT, accept `claims_to_verify` as either `[]string` or decoded `[]any`, and require the literal string `exp`. Reject other shapes with a stable error containing the source description.

For resolved upstreams, require `upstream.TLS != nil && upstream.TLS.Verify` only when `upstreamUsesTLS(upstream)` is true.

- [ ] **Step 4: Add failing BuildStrict integration tests**

Use store-backed tests to prove:

1. an unsafe JWT config inherited from a service is rejected;
2. a safe route-level JWT config overriding that service config passes;
3. unsafe JWT in a global rule is rejected;
4. an `upstream_id` resolving to HTTPS without `tls.verify:true` is rejected;
5. every same payload passes in compatibility mode where APISIX defaults remain supported.

- [ ] **Step 5: Invoke policy before construction**

Immediately after `selectMaterializedPluginSources`, validate the effective route/service/plugin-config winners. After loading global rules, validate each global rule before `initGlobalPluginBindingsStrict`. Immediately after `resolveRouteUpstream`, validate the resolved upstream before discovery and reverse-handler construction. Wrap failures with route ID and resource provenance.

- [ ] **Step 6: Run focused green tests**

Run:

```bash
source .envrc && go test ./pkg/route -run 'TestHTTPDataPlaneV1(Plugin|Upstream)Policy|TestBuildStrictEnforcesHTTPDataPlaneV1Safety' -count=1
```

- [ ] **Step 7: Commit Task 2**

```bash
git add pkg/route/builder.go pkg/route/production_policy.go pkg/route/production_policy_test.go
git commit -m "fix(profile): enforce authentication and upstream TLS safety"
```

---

### Task 3: Require a bounded frontend body-read deadline

**Files:**
- Modify: `pkg/config/init.go:235-294`
- Modify: `pkg/config/release_gate_test.go:300-760`
- Modify: `conf/config-production.yaml:32-40`

**Interfaces:**
- Consumes: `NginxHTTP.ClientBodyTimeout` and the existing `newConfiguredHTTPServer` mapping.
- Produces: profile startup invariant `nginx_config.http.client_body_timeout > 0`; checked-in value `60s`.

- [ ] **Step 1: Add failing profile mutation and reference-config assertions**

In the valid production-config helper set `ClientBodyTimeout: 60 * time.Second`. Add a mutation row that sets it to zero and expects the field name plus `must be positive`. In the checked-in config test, assert the decoded value is exactly `60*time.Second`.

- [ ] **Step 2: Run profile tests red**

Run:

```bash
source .envrc && go test ./pkg/config -run 'TestProduction(ProfileRejectsOneMutatedFieldPerRow|ConfigFilePassesReleaseGate)' -count=1
```

Expected: zero timeout is accepted or the checked-in file decodes to zero.

- [ ] **Step 3: Add the validation and reference value**

After `client_max_body_size`, add:

```go
if cfg.NginxConfig.HTTP.ClientBodyTimeout <= 0 {
	return profileFieldError(profile, "nginx_config.http.client_body_timeout", "must be positive")
}
```

Set in `conf/config-production.yaml`:

```yaml
client_body_timeout: 60s
```

- [ ] **Step 4: Run focused config and server tests**

```bash
source .envrc && go test ./pkg/config -run 'TestProduction(ProfileAcceptsValidConfig|ProfileRejectsOneMutatedFieldPerRow|ConfigFilePassesReleaseGate)' -count=1
source .envrc && go test ./pkg/server -run 'TestNewConfiguredHTTPServer' -count=1
```

- [ ] **Step 5: Commit Task 3**

```bash
git add pkg/config/init.go pkg/config/release_gate_test.go conf/config-production.yaml
git commit -m "fix(profile): require bounded client body reads"
```

---

### Task 4: Give the plugin status matrix an independent CI gate

**Files:**
- Create: `.github/workflows/plugin-status.yml`
- Create: `scripts/plugin_status_gate_test.sh`

**Interfaces:**
- Consumes: `t/plugin.TestSupportedPluginManifestSelection` and the status rule “labels beginning with Supported select manifests”.
- Produces: GitHub check `Plugin Status Contract` for changes to the status matrix, manifests, selector test, or workflow itself.

- [ ] **Step 1: Write the red shell contract test**

The test must require all four trigger paths, `permissions: contents: read`, Go setup from `go.mod`, and this exact command:

```bash
go test ./t/plugin -run '^TestSupportedPluginManifestSelection$' -count=1
```

Required paths:

```text
docs/plugins.md
t/plugin/*.yaml
t/plugin/coverage_test.go
.github/workflows/plugin-status.yml
```

- [ ] **Step 2: Run the shell contract red**

```bash
bash scripts/plugin_status_gate_test.sh
```

Expected: FAIL because `.github/workflows/plugin-status.yml` does not exist.

- [ ] **Step 3: Create the minimal workflow**

Use `pull_request` and pushes to `master`, each with the four exact paths. Use `workflow_dispatch`, `contents: read`, one Ubuntu job, `actions/checkout@v7`, `actions/setup-go@v7` with `go-version-file: go.mod`, and the exact focused test command. Do not run the real-process plugin cases.

- [ ] **Step 4: Run workflow verification green**

```bash
bash scripts/plugin_status_gate_test.sh
source .envrc && go test ./t/plugin -run '^TestSupportedPluginManifestSelection$' -count=1
source .envrc && actionlint -color .github/workflows/plugin-status.yml
```

- [ ] **Step 5: Commit Task 4**

```bash
git add .github/workflows/plugin-status.yml scripts/plugin_status_gate_test.sh
git commit -m "ci: verify plugin status manifest selection"
```

---

### Task 5: Synchronize operator and release qualification contracts

**Files:**
- Modify: `docs/configuration.md`
- Modify: `docs/production-profile.md`
- Modify: `docs/production-readiness-review-2026-08-16.md`
- Modify: `docs/runbooks/production-release.md`

**Interfaces:**
- Consumes: Tasks 1-4 behavior and error contracts.
- Produces: one current ledger that separates pre-merge code closure from post-merge qualification evidence.

- [ ] **Step 1: Document route and profile behavior**

Record that singular `host` uses the same exact/wildcard dispatcher as `hosts`; simultaneous `host` and `hosts` rejects; route `remote_addr`, non-empty `remote_addrs`, `vars`, `script`, `script_id`, and `filter_func` fail closed. State that the candidate profile now rejects unsafe auth/TLS configs rather than relying only on operator memory.

- [ ] **Step 2: Document the body timeout**

State that `client_body_timeout` is mandatory and positive in `http-data-plane-v1`, the checked-in production value is 60 seconds, and Go maps it to a combined header/body `ReadTimeout` because `net/http` has no body-only server deadline.

- [ ] **Step 3: Make ingress audit logging a qualification prerequisite**

The six-plugin profile still has no in-process request logger. Require the external ingress evidence bundle to demonstrate redacted request ID, method, normalized path without query secrets, status, latency, upstream identity, retention owner, and trace correlation. Do not claim the runtime emits these fields.

- [ ] **Step 4: Keep release qualification open**

The ledger and runbook must still require a post-merge RC and independent final for the same source revision, immutable digest signature/attestation, container SBOM/Trivy, verified-TLS etcd recovery, proxy soak/capacity evidence, and rollback to a distinct older digest. Also retain the external prerequisites: protected `master`, required CI/security/plugin-status checks, a `production-release` tag policy that preserves reviewer/wait protections, and no self-approval.

- [ ] **Step 5: Review documentation consistency**

```bash
git diff --check
rg -n 'not production ready|candidate|release qualification|client_body_timeout|hide_credentials|claims_to_verify|tls.verify|plugin status' \
  docs/production-profile.md docs/production-readiness-review-2026-08-16.md \
  docs/configuration.md docs/runbooks/production-release.md
```

- [ ] **Step 6: Commit Task 5**

```bash
git add docs/configuration.md docs/production-profile.md \
  docs/production-readiness-review-2026-08-16.md docs/runbooks/production-release.md \
  docs/superpowers/plans/2026-08-17-production-readiness-hardening.md
git commit -m "docs: define remaining production qualification boundary"
```

---

### Task 6: Integrated verification and PR delivery

**Files:**
- Review only: all files changed by Tasks 1-5.

**Interfaces:**
- Consumes: every task deliverable.
- Produces: a reviewable PR; not a production-ready declaration.

- [ ] **Step 1: Format and inspect**

```bash
source .envrc && golangci-lint fmt
git diff --check
git status --short
git diff --stat
```

- [ ] **Step 2: Run impact-scoped tests**

```bash
source .envrc && go test ./pkg/resource ./pkg/config ./pkg/server ./pkg/route -count=1
source .envrc && go test ./t/plugin -run '^TestSupportedPluginManifestSelection$' -count=1
bash scripts/plugin_status_gate_test.sh
```

- [ ] **Step 3: Run repository gates required for code/config changes**

```bash
source .envrc && make lint
source .envrc && make build
source .envrc && make clean
```

- [ ] **Step 4: Refactor/dead-code audit**

List every added or changed method/function, run `rg` for all call sites, and confirm no compatibility proxy, unused helper, stale test fixture, or unrelated refactor remains. This plan adds behavior but also changes route decoding, so the audit is mandatory.

- [ ] **Step 5: Independent review**

Request one read-only final review of the combined diff. Resolve only findings that are in scope and re-run affected checks after every mutation.

- [ ] **Step 6: Commit any acceptance corrections**

```bash
git add -- .github/workflows/plugin-status.yml conf/config-production.yaml \
  docs/configuration.md docs/production-profile.md docs/production-readiness-review-2026-08-16.md \
  docs/runbooks/production-release.md docs/superpowers/plans/2026-08-17-production-readiness-hardening.md \
  pkg/config/init.go pkg/config/release_gate_test.go pkg/resource/route.go pkg/resource/route_test.go \
  pkg/route/builder.go pkg/route/production_policy.go pkg/route/production_policy_test.go \
  pkg/route/route_host_test.go pkg/route/unsupported_semantics_test.go scripts/plugin_status_gate_test.sh
git commit -m "fix: harden production candidate admission"
```

- [ ] **Step 7: Push and open the PR**

```bash
git push -u origin codex/production-readiness-hardening
gh pr create --base master --head codex/production-readiness-hardening \
  --title "fix: harden production candidate admission" \
  --body-file "$PR_BODY_FILE"
```

The PR body must list exact verification, state that release evidence remains open, and must not say production ready.

## Self-review

**Spec coverage:** Route fail-open behavior is Task 1; candidate auth/upstream safety is Task 2; frontend read bounding is Task 3; status-matrix CI is Task 4; request logging, branch/environment policy, artifact qualification, and rollback boundaries are Task 5. Actual RC/final/tag/signing/deployment evidence is intentionally post-merge and remains open.

**Placeholder scan:** The plan contains no TBD/TODO, generic “add validation”, unspecified tests, or undefined implementation interfaces.

**Type consistency:** `Route.EffectiveHosts`, `HostConfigured`, and `RemoteAddrConfigured` are defined in Task 1 and consumed only by route validation/registration. Production policy receives effective `map[string]resource.PluginConfig` and resolved `resource.Upstream`, matching existing builder outputs.
