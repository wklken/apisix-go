# HTTP Candidate Qualification

This runbook qualifies one immutable APISIX-Go source revision for the bounded
HTTP data-plane claim in [HTTP data-plane compatibility](../http-data-plane.md).
It covers repository-owned functional and runtime-stability evidence only.

It does not publish images or archives, validate upgrade/rollback, or accept an
operator environment. Kubernetes or systemd deployment policy remains outside
this repository's candidate decision.

## Run the candidate workflow

Start `Release Candidate` for one commit or tag:

```bash
gh workflow run release-candidate.yml -f ref='<commit-or-tag>'
```

The workflow resolves the ref once and binds every job to the resulting commit.
It must pass:

| Gate | Required evidence |
| --- | --- |
| Source | Lint, build, unit tests, plugin registry drift, and plugin harness. |
| HTTP smoke | Focused real-process authentication, rewrite, proxy-control, and rejection cases. |
| Concurrency and dependencies | Focused race tests plus reachable Go and container vulnerability checks. |
| Container | Linux amd64 build, non-root proxy smoke, graceful shutdown, SBOM, and immutable local image identity. |
| Stability | Canonical 30-minute concurrency-256 proxy soak using [Proxy Runtime Acceptance](../performance/proxy-runtime-acceptance.md). |

The container image is an ephemeral workflow artifact used by candidate checks.
It is not pushed, signed, tagged as a release, or reused by another run.

Missing, skipped, stale, or identity-mismatched evidence fails the candidate.
Evidence from different commits, binaries, images, or runs cannot be combined.

## Local contract checks

Use these checks when changing the workflow or its retained smoke scripts:

```bash
bash scripts/release_gate_test.sh
bash scripts/container_smoke_test.sh
```

They validate local script and workflow contracts. They do not replace the
post-merge candidate run or the timed soak.

## Decision record

Record the candidate as one of:

- **passed**: every required gate passed for the same source and candidate identity;
- **failed**: a required gate failed; or
- **pending**: required infrastructure or evidence was unavailable.

A passing candidate supports only this claim:

> The documented HTTP data plane is functionally and runtime-stability verified
> for the recorded source revision, candidate identity, tested plugin behavior, and
> dependency boundaries.

It is not a repository-wide or environment-specific production-readiness claim.
