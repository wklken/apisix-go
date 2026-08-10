# HTTP Route, Rewrite, and Expression Correctness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close PR-001, PR-002, and PR-019 so host-constrained parameter routes, regex URI query preservation, and negated numeric expressions match the APISIX 3.17 contract.

**Architecture:** Keep the three fixes local to their existing owners. Extend the route dispatcher's path predicate to understand named segments, preserve query strings at the proxy-rewrite URI boundary, and apply numeric negation after comparison rather than pre-rejecting non-numeric values.

**Tech Stack:** Go 1.26, `net/http`, chi route builder, existing plugin expression evaluator and package tests.

## Global Constraints

- Baseline is `bb7fab01e7b46964bfe6cd543959c981d28b49a5`.
- Do not change wildcard precedence, host rank, method rank, route insertion order, or raw URI behavior outside the specified cases.
- For normal regex rewrite, append the incoming raw query once; for `use_real_request_uri_unsafe=true`, the regex input/output already includes the raw query and must not append it again.
- Match a `:name` segment only when the corresponding request segment is non-empty; it must not consume `/`.
- Negation is applied to the boolean comparison result, including conversion failure, matching `lua-resty-expr 1.3.2`.
- Run Go commands with `bash -lc 'source .envrc && ...'`.

---

## File Structure

- Modify `pkg/route/builder.go`: add parameter-segment matching inside the existing wildcard/host dispatcher.
- Modify `pkg/route/uri_test.go`: add host, method, single-parameter, multi-parameter, and mismatch regressions.
- Modify `pkg/plugin/proxy_rewrite/plugin.go`: preserve query state after regex rewrite.
- Modify `pkg/plugin/proxy_rewrite/plugin_test.go`: lock normal and unsafe query contracts.
- Modify `pkg/plugin/expr/expression.go`: remove the numeric pre-parse short circuit.
- Modify `pkg/plugin/expr/expression_test.go`: replace the incorrect missing-value assertion with an APISIX truth table.
- Modify `pkg/route/builder_lifecycle_test.go`: add one route-filter integration assertion for the corrected negation behavior.

### Task 1: Match host-constrained named-parameter routes

**Files:**
- Modify: `pkg/route/builder.go:167-375`
- Test: `pkg/route/uri_test.go`

**Interfaces:**
- Consumes: `wildcardRoute.pattern`, `routeHostRank`, and `matchesWildcardRoute`.
- Produces: `matchesRoutePath(pattern, requestPath string) bool` supporting literal, `:name`, and `*` segments without changing dispatcher precedence.

- [ ] **Step 1: Write the failing route matrix**

Add `TestRouteRegistrarMatchesHostConstrainedParameters` with handlers for `/users/:id` and `/teams/:team/members/:member`. Exercise `GET /users/42` and `POST /teams/core/members/alice` with `Host: api.example.com`, then assert wrong host is 404, wrong method is 405, and `/users/` is 404.

```go
tests := []struct {
	name   string
	method string
	path   string
	host   string
	want   int
}{
	{"single parameter", http.MethodGet, "/users/42", "api.example.com", http.StatusNoContent},
	{"multiple parameters", http.MethodPost, "/teams/core/members/alice", "api.example.com", http.StatusAccepted},
	{"wrong host", http.MethodGet, "/users/42", "other.example.com", http.StatusNotFound},
	{"wrong method", http.MethodDelete, "/users/42", "api.example.com", http.StatusMethodNotAllowed},
	{"empty parameter", http.MethodGet, "/users/", "api.example.com", http.StatusNotFound},
}
```

- [ ] **Step 2: Run the regression and record the current 404**

Run: `bash -lc 'source .envrc && go test ./pkg/route -run "^TestRouteRegistrarMatchesHostConstrainedParameters$" -count=1'`

Expected before implementation: the valid parameter rows fail with 404.

- [ ] **Step 3: Add named-segment matching**

Keep literals byte-exact and require equal segment counts when there is no trailing `*`.

```go
func matchesParameterizedRoute(pattern, requestPath string) bool {
	patternParts := strings.Split(pattern, "/")
	requestParts := strings.Split(requestPath, "/")
	if len(patternParts) != len(requestParts) {
		return false
	}
	for i := range patternParts {
		if strings.HasPrefix(patternParts[i], ":") {
			if len(patternParts[i]) == 1 || requestParts[i] == "" {
				return false
			}
			continue
		}
		if patternParts[i] != requestParts[i] {
			return false
		}
	}
	return true
}
```

Call this predicate from `matchesRoutePath` before the existing `*` path. If a pattern contains both `:` and `*`, preserve the current wildcard prefix/suffix rule and validate parameter segments in the non-wildcard portions; add a focused mixed-pattern row if current route schema permits that shape.

- [ ] **Step 4: Verify route matching and existing URI tests**

Run: `bash -lc 'source .envrc && go test ./pkg/route -run "^(TestRouteRegistrarMatchesHostConstrainedParameters|TestConvertURI|Test.*URI.*)$" -count=1'`

Expected: valid host/parameter routes reach their handlers; mismatch status codes stay unchanged.

### Task 2: Preserve query strings across regex URI rewrite

**Files:**
- Modify: `pkg/plugin/proxy_rewrite/plugin.go:185-265`
- Test: `pkg/plugin/proxy_rewrite/plugin_test.go`

**Interfaces:**
- Consumes: `rewriteSourceURI`, `rewriteURI`, and `appendRequestQuery`.
- Produces: one rewritten URI string passed through `apisixctx.ProxyRewriteKey` with exactly one query component.

- [ ] **Step 1: Add the four-path query matrix**

Add `TestRegexURIQueryContract` with these exact cases:

```go
tests := []struct {
	name   string
	unsafe bool
	source string
	repl   string
	want   string
}{
	{"append incoming query", false, "/items/42?tenant=a", "/products/$1", "/products/42?tenant=a"},
	{"merge replacement query", false, "/items/42?tenant=a", "/products/$1?fixed=1", "/products/42?fixed=1&tenant=a"},
	{"empty incoming query", false, "/items/42", "/products/$1", "/products/42"},
	{"unsafe source includes query once", true, "/items/42?tenant=a", "/raw/$1", "/raw/42?tenant=a"},
}
```

For the unsafe row use a regex that consumes the complete request URI, such as `^/items/([^?]+)(\?.*)?$` with replacement `/raw/$1$2`, so the test documents that unsafe matching owns the query.

- [ ] **Step 2: Run the focused failure**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/proxy_rewrite -run "^TestRegexURIQueryContract$" -count=1'`

Expected before implementation: normal regex rows lose `tenant=a`.

- [ ] **Step 3: Append query only for normalized regex rewrites**

After `rewriteURI`, apply:

```go
if uri != "" && p.config.Uri == "" && !p.config.UseRealRequestURIUnsafe {
	uri = appendRequestQuery(uri, r.URL.RawQuery)
}
```

Retain the existing static `uri` branch and do not change `FinalizeProxyRewrite`.

- [ ] **Step 4: Verify plugin and route integration**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/proxy_rewrite ./pkg/route -run "(RegexURIQueryContract|ProxyRewrite|RegexURI)" -count=1'`

Expected: query order is replacement query first, incoming query second; unsafe mode has no duplicate query.

### Task 3: Match negated numeric conversion semantics

**Files:**
- Modify: `pkg/plugin/expr/expression.go:190-207`
- Test: `pkg/plugin/expr/expression_test.go`
- Test: `pkg/route/builder_lifecycle_test.go`

**Interfaces:**
- Consumes: `condition.eval` and the existing reverse flag.
- Produces: `Expression.Eval` where conversion failure yields comparison false and reverse yields true.

- [ ] **Step 1: Replace the incorrect missing-value test with a truth table**

Add `TestNegatedNumericComparisonTruthTable` covering `nil`, `""`, `"abc"`, `"NaN"`, `"+Inf"`, and numeric values for `>`, `>=`, `<`, and `<=`.

```go
for _, operator := range []string{">", ">=", "<", "<="} {
	expression, err := Compile([]any{[]any{"age", "!", operator, 18}})
	if err != nil { t.Fatal(err) }
	for _, actual := range []any{nil, "", "abc", "NaN", "+Inf"} {
		if !expression.Eval(func(string) any { return actual }) {
			t.Fatalf("operator %s actual %#v = false, want true", operator, actual)
		}
	}
}
```

- [ ] **Step 2: Run and observe the old pre-parse behavior**

Run: `bash -lc 'source .envrc && go test ./pkg/plugin/expr -run "^TestNegatedNumericComparisonTruthTable$" -count=1'`

Expected before implementation: missing and malformed values return false.

- [ ] **Step 3: Remove the numeric pre-parse short circuit**

Delete the `condition.reverse && isNumericOperator(...)` branch and the now-unused helper/import. Leave `condition.eval` as the sole conversion owner and retain:

```go
if e.condition.reverse {
	return !matched
}
```

- [ ] **Step 4: Add one route-filter integration**

Build a route whose `filter_func` is `[["arg_age", "!", ">=", 18]]`; assert a request without `age` selects the filtered route behavior, while `?age=21` does not.

- [ ] **Step 5: Verify all three work units**

Run:

```bash
bash -lc 'source .envrc && go test ./pkg/route ./pkg/plugin/proxy_rewrite ./pkg/plugin/expr -count=1'
bash -lc 'source .envrc && golangci-lint run ./pkg/route/... ./pkg/plugin/proxy_rewrite/... ./pkg/plugin/expr/...'
bash -lc 'source .envrc && make build'
```

Expected: all commands pass. Do not run repository-wide tests.

- [ ] **Step 6: Commit the independent PR scope**

```bash
git add docs/superpowers/plans/2026-08-10-http-route-rewrite-expression-correctness.md \
  pkg/route/builder.go pkg/route/uri_test.go pkg/route/builder_lifecycle_test.go \
  pkg/plugin/proxy_rewrite/plugin.go pkg/plugin/proxy_rewrite/plugin_test.go \
  pkg/plugin/expr/expression.go pkg/plugin/expr/expression_test.go
git commit -m "fix: preserve HTTP routing and expression semantics"
```

