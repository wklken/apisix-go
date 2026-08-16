# Mandatory Secret Materialization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task in an isolated worktree. This plan does not authorize subagents.

**Goal:** Make unresolved `$ENV://` and `$secret://` references impossible to cross a plugin initialization boundary without an owning `SecretMaterializer`, and remove every direct plugin-side `ResolveSecretReference` call that writes resolved plaintext back into public config.

**Architecture:** Retain the existing generation-owned `store.ResolvedSecret` handle and pre-`PostInit` lifecycle. Strengthen the boundary helper so a plugin without a materializer fails when its decoded config still contains a secret reference. Migrate the four current direct resolvers—AWS moderation, authz-keycloak, openid-connect public key, and limit-count reference-backed fields—to materializer-owned private state and redacted descriptors. Destroy handles on initialization failure and route-generation retirement.

**Tech Stack:** Go 1.26, reflection with cycle/depth bounds, `store.ResolvedSecret`, existing route/plugin stop lifecycle.

## Frozen contracts

- A literal string remains compatible; this PR governs reference resolution, not a redesign of every literal credential field.
- A reference descriptor such as `$ENV://NAME#sha256:<digest>` is safe to retain in effective config and is not treated as unresolved.
- Error messages may include the plugin name and bounded field path, but never the environment/Vault value, credential bytes, or full fingerprint.
- `MaterializeSecrets` runs after schema decode and before `PostInit` at route/global, metadata observer, workflow, multi-auth, and stream initialization boundaries.
- Partial materialization destroys all already-created handles. `Stop` is idempotent and destroys every handle owned by that generation.
- Production packages under `pkg/plugin` must have zero calls to `store.ResolveSecretReference`; only `store.MaterializeSecret` is allowed at a `SecretMaterializer` boundary.

### Task 1: Make reference ownership fail closed at every boundary

**Files:**
- Modify: `pkg/plugin/base/secrets.go`
- Create: `pkg/plugin/base/secrets_test.go`
- Modify: `pkg/plugin/types.go`
- Create: `pkg/plugin/secret_materialization_guard_test.go`
- Modify: `pkg/route/builder_lifecycle_test.go`
- Modify: `pkg/plugin/multi_auth/plugin_test.go`
- Modify: `pkg/plugin/workflow/plugin_test.go`
- Modify: `pkg/stream/router_test.go`

- [ ] **Step 1: Add boundary regressions before production changes**

Cover: literal config without materializer succeeds; unresolved reference without materializer fails; nested map/slice/struct reference is reported with a bounded path; a materializer is invoked exactly once; a descriptor is accepted after materialization; a materializer error is redacted; workflow/multi-auth/stream nested plugins propagate the same failure.

```go
type configOwner interface{ Config() any }

err := base.MaterializePluginSecrets(&fakePlugin{
	config: struct{ Token string `json:"token"` }{Token: "$ENV://TOKEN"},
})
if err == nil || !strings.Contains(err.Error(), "token") {
	t.Fatalf("MaterializePluginSecrets() error = %v, want unowned reference", err)
}
```

- [ ] **Step 2: Run the red tests**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/base ./pkg/plugin ./pkg/route ./pkg/plugin/multi_auth ./pkg/plugin/workflow ./pkg/stream -run "(SecretMaterialization|UnownedSecretReference)" -count=1'
```

Expected: compile failure for the new reference scanner or silent success for the fake unowned reference.

- [ ] **Step 3: Strengthen `MaterializePluginSecrets`**

Keep `SecretMaterializer` opt-in as an ownership marker, but make absence fail closed when the public config contains an unresolved reference:

```go
func MaterializePluginSecrets(p any) error {
	materializer, ownsSecrets := p.(SecretMaterializer)
	if !ownsSecrets {
		if owner, ok := p.(interface{ Config() any }); ok {
			if path, found := firstUnmaterializedSecretReference(owner.Config()); found {
				return fmt.Errorf("unowned secret reference at %s", path)
			}
		}
		return nil
	}
	if err := materializer.MaterializeSecrets(); err != nil {
		return err
	}
	if owner, ok := p.(interface{ Config() any }); ok {
		if path, found := firstUnmaterializedSecretReference(owner.Config()); found {
			return fmt.Errorf("secret reference remains unmaterialized at %s", path)
		}
	}
	return nil
}
```

The scanner supports pointers, interfaces, structs, maps with string keys, arrays, and slices; cap recursion depth at 32 and track visited pointers. Treat `$ENV://...#sha256:` and `$secret://...#sha256:` as descriptors. Do not stringify arbitrary values.

- [ ] **Step 4: Add a source guard**

Use `go/parser`/`go/ast` in `pkg/plugin/secret_materialization_guard_test.go` to walk production `.go` files below `pkg/plugin`, excluding `_test.go`, and reject selector calls named `ResolveSecretReference`. This prevents a future `PostInit` regression without depending on grep in CI.

### Task 2: Move AWS moderation credentials into generation-owned handles

**Files:**
- Modify: `pkg/plugin/ai_aws_content_moderation/plugin.go`
- Modify: `pkg/plugin/ai_aws_content_moderation/plugin_test.go`

- [ ] **Step 1: Replace plaintext-resolution assertions with ownership assertions**

Update the existing environment-reference test to assert descriptors remain in `p.config`, materialized handles contain cloned values, signed requests contain the expected credential fields, and `Stop` zeroes all three handles. Add partial-failure coverage for session token materialization.

- [ ] **Step 2: Run the focused red tests**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/ai_aws_content_moderation -run "(Materialize|Environment|SessionToken|Stop)" -count=1'
```

Expected before implementation: public config contains `environment-access` / `environment-secret` and the plugin has no owned handles.

- [ ] **Step 3: Implement `MaterializeSecrets` and cleanup**

Add private handles and keep only descriptors in `Config`:

```go
type Plugin struct {
	base.BasePlugin
	config Config
	accessKeyID     *store.ResolvedSecret
	secretAccessKey *store.ResolvedSecret
	sessionToken    *store.ResolvedSecret
}
```

Materialize required fields first, optional session token last, and destroy prior handles on any error. Remove all reference resolution from `PostInit`. At request signing, clone each handle into a short-lived byte slice, convert only at the AWS signer boundary, and `clear` the byte slices after the signer has copied them. `Stop` destroys all handles.

### Task 3: Migrate the remaining direct plugin resolvers

**Files:**
- Modify: `pkg/plugin/authz_keycloak/plugin.go`
- Modify: `pkg/plugin/authz_keycloak/plugin_test.go`
- Modify: `pkg/plugin/openid_connect/plugin.go`
- Modify: `pkg/plugin/openid_connect/plugin_test.go`
- Modify: `pkg/plugin/limit_count/plugin.go`
- Modify: `pkg/plugin/limit_count/plugin_test.go`

- [ ] **Step 1: Add redacted-config and retirement tests for each owner**

For authz-keycloak, assert client-secret forms receive the value while config/cache identity exposes only descriptor/fingerprint. For OIDC, parse the materialized public key into `staticPublicKey`, replace public config with the descriptor, then destroy the transient handle because the parsed public key is not secret. For limit-count, cover referenced key, Redis host, and cluster nodes; request/client behavior sees resolved values while config retains descriptors and route retirement destroys owned handles.

- [ ] **Step 2: Run the focused tests before implementation**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/authz_keycloak ./pkg/plugin/openid_connect ./pkg/plugin/limit_count -run "(Materialize|Environment|Secret|Reference|Stop)" -count=1'
```

Expected: the existing tests still assert resolved plaintext in public config.

- [ ] **Step 3: Give each plugin one materialization owner**

Use `ResolvedSecret.Fingerprint()` rather than plaintext in shared-client/cache identities. Where a value is needed per request, use a helper that clones `Bytes`, validates non-empty, defers `clear`, and passes the value only to the immediate form/signer/key expansion call. Do not retain a second plaintext copy in `Config`.

For OIDC public-key parsing:

```go
if p.config.PublicKey != "" {
	key, err := store.MaterializeSecret(p.config.PublicKey)
	if err != nil {
		return errors.New("resolve openid-connect public_key reference: credential unavailable")
	}
	defer key.Destroy()
	encoded := key.Bytes()
	defer clear(encoded)
	p.staticPublicKey, err = parsePublicKey(encoded)
	p.config.PublicKey = key.Descriptor()
}
```

Preserve the existing generic client-facing errors.

- [ ] **Step 4: Prove no direct resolver remains**

Run:

```bash
if rg -n 'ResolveSecretReference\(' pkg/plugin --glob '*.go' --glob '!**/*_test.go'; then
  echo 'direct plugin secret resolver remains' >&2
  exit 1
fi
```

### Task 4: Verify lifecycle, race safety, and delivery scope

- [ ] **Step 1: Run impact-scoped verification**

```bash
bash -lc 'source .envrc && go test ./pkg/plugin/base ./pkg/plugin ./pkg/route ./pkg/stream ./pkg/plugin/multi_auth ./pkg/plugin/workflow ./pkg/plugin/ai_aws_content_moderation ./pkg/plugin/authz_keycloak ./pkg/plugin/openid_connect ./pkg/plugin/limit_count -count=1'
bash -lc 'source .envrc && go test -race ./pkg/route ./pkg/stream ./pkg/plugin/ai_aws_content_moderation ./pkg/plugin/authz_keycloak ./pkg/plugin/openid_connect ./pkg/plugin/limit_count -run "(Materialize|Reference|Reload|Stop|Retire)" -count=3'
bash -lc 'source .envrc && make lint'
bash -lc 'source .envrc && make build && make clean'
git diff --check
```

- [ ] **Step 2: Inspect every lifecycle call site**

Run and classify every result; each parse-before-`PostInit` path must call materialization:

```bash
rg -n 'MaterializePluginSecrets|\.PostInit\(\)' pkg/route pkg/plugin pkg/stream
```

- [ ] **Step 3: Commit the independent PR**

```bash
git add pkg/plugin/base/secrets.go pkg/plugin/base/secrets_test.go pkg/plugin/types.go \
  pkg/plugin/secret_materialization_guard_test.go pkg/route/builder_lifecycle_test.go \
  pkg/plugin/multi_auth/plugin_test.go pkg/plugin/workflow/plugin_test.go pkg/stream/router_test.go \
  pkg/plugin/ai_aws_content_moderation pkg/plugin/authz_keycloak \
  pkg/plugin/openid_connect pkg/plugin/limit_count \
  docs/superpowers/plans/2026-08-16-mandatory-secret-materialization.md
git commit -m "fix(secrets): require plugin-owned materialization"
```
