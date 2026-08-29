---
id: ADR-0007
title: Prefer bounded all-match ACL JSONPath evaluation
status: proposed
compatibility_target: apisix-3.17
divergence_ids: [DIV-007-bounded-acl-jsonpath]
owner: wklken
owner_approval_ref: ""
date: 2026-08-29
---

# Context

APISIX 3.17 calls `jp.value()` for `acl.external_user_label_field`, so a
JSONPath that matches multiple values checks only the first result. APISIX PR
#13527, commit `3584284c30157b224d5391c03d209c23ef19f90f`, later changed the
plugin to query and parse every match because a denied label in a later match
could otherwise be missed.

That post-3.17 path preserves the configured parser when the query produces at
most one actual match. With multiple actual matches, it applies the configured
JSON or segmented-text parser only when an individual match is a string; table
and scalar matches retain type-aware handling. Dispatching only from the
selector's theoretical multiplicity breaks both single-match allow and deny
semantics, while applying the configured parser to every multiple-match result
can discard a later table value and make a deny-only policy fail open.

The pinned lua-jsonpath 1.0-1 evaluator also permits recursive selectors,
unions, slices, filters, scripts, non-finite arithmetic, and Lua `tostring`
coercion of tables and functions. Unbounded recursive evaluation can amplify
attacker-controlled documents. Lua address strings for reference values are
process-specific and have no stable Go equivalent.

# Decision

The Go ACL plugin implements the reachable lua-jsonpath 1.0-1 selector and
scalar-expression behavior, but adopts the post-3.17 all-match fix. Results
are ordered by concrete path and duplicate concrete paths are evaluated once.
Configured label rules are strings, so numeric and boolean label values retain
their Lua types and do not match text such as `"42"` or `"true"`. Automatic
JSON-array detection preserves those element types, and malformed candidate
arrays produce no match instead of falling back to raw-string matching.

Evaluation is request-local and fails closed after 64 selector steps, 64
recursive levels, 4096 traversal visits, 1024 unique results, 256 union terms,
256 expression nodes, or expression depth 64. Non-finite arithmetic and
reference values that Lua would stringify as unstable address-bearing text
also fail closed.

Configured JSONPath is limited to 4096 encoded bytes before parsing. Terminal
label extraction is request-local and fails closed after 4096 label values or
256 KiB of label input/value bytes. These checks run before separator-driven
preallocation or table-result expansion. Script selectors fail closed when
their result is a map, function, or nested reference instead of silently
treating it as an empty selector result.

Script selector components are evaluated and hashed once per parent instead of
being rebuilt for every child. Across the request they fail closed before hash
allocation after 4096 scalar component visits or 256 KiB of component bytes;
both limits accumulate across distinct parents reached by the JSONPath.

# Consequences

A request that APISIX 3.17 would allow because a denied label was not the first
JSONPath match is rejected. A selector capable of multiple matches but yielding
only one retains the configured parser, while actual mixed multi-match results
preserve non-string label types instead of routing them through a configured
string parser or coercing them into matching strings. Pathological paths,
expressions, documents, script selector results,
and terminal label expansions are bounded before they can consume unbounded CPU
or memory. Configurations that depend on matching Lua table or function address
strings are rejected instead of producing process-specific results.

The converted ACL manifest's longest configured JSONPath is 36 bytes and the
fixed unit corpus's longest literal path is 48 bytes, leaving substantial
headroom beneath the 4096-byte admission ceiling. The hard path and terminal
label limits remain security constraints rather than APISIX 3.17 behavior.

This proposal does not become an accepted production divergence until the
owner explicitly approves it. Qualification must keep the pinned 3.17
single-match behavior, the post-3.17 multi-match correction, and every runtime
budget visible as separate evidence.

# Evidence required to retire

Retirement requires a pinned APISIX target that includes equivalent all-match
behavior and operator-controlled evaluation limits, or a replacement contract
with explicit compatibility, resource-bound, migration, and rollback evidence.
