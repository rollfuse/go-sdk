# Project Context

## Product

**go-sdk** is the Go client library for **rollfuse**, a developer-first
feature-flag and progressive-delivery platform. This repository holds only
the SDK — the platform itself (API, web console, other SDKs) lives in the
`rollfuse/rollfuse` monorepo.

It was extracted from `rollfuse/rollfuse`'s `packages/sdk-go` by
`extract-go-sdk-standalone-repo`, preserving its full commit history, so
that it can be published and versioned as a standard Go module
(`go get github.com/rollfuse/go-sdk`) instead of living behind a
subdirectory module path inside a private application monorepo.

## What This SDK Does

Evaluates feature flags **locally**, against a cached, versioned
Configuration fetched from the platform's API, instead of calling the API
synchronously on every evaluation (ADR 0004 in `rollfuse/rollfuse`:
Evaluate Flags Locally in SDKs). It targets Go server-side backends: it
holds a Service Credential secret, so it is not meant for untrusted client
environments (browsers, mobile apps — those integrate via
`@rollfuse/sdk-browser` instead).

## Engineering Priorities

1. Correctness — an evaluation result must always match what the API would
   have returned for the same Configuration Version, flag, subject key and
   attributes.
2. Availability — an integrating application must remain operational when
   the platform is unreachable (safe fallback, failure isolation).
3. Concurrency safety — evaluation and background refresh run concurrently
   across goroutines without a data race and without observing a partially
   updated Configuration.
4. Maintainability
5. Performance

## Architecture Constraints

- No third-party runtime dependencies — standard library only. A second
  Go module pulling this one in should never pull in a third-party
  dependency tree it didn't already have.
- Evaluation (`Evaluate`/`EvaluateAll`) MUST be synchronous and MUST NOT
  perform a network call once a Configuration is cached.
- Background Configuration refresh and exposure-batch flushing MUST NOT
  block or affect the result of any concurrent evaluation call.
- The bucketing algorithm's output MUST match `rollfuse/rollfuse`'s API
  and its other SDKs bit for bit — see `openspec/config.yaml`'s context
  for the cross-repo fixture-parity mechanism.
- This repo has no HTTP server, no database, and no multi-tenant request
  handling — those concerns live in `rollfuse/rollfuse` and are out of
  scope here.

## Repository Shape

```text
/
├── client.go                    NewClient, Evaluate/EvaluateAll, Close
├── configuration_client.go      GET /v1/config fetch + background refresh
├── evaluate.go                  Rule matching, default/fallback resolution
├── bucketing.go                 Stable percentage-rollout bucketing
├── exposure_queue.go            Async, best-effort exposure reporting
├── errors.go
├── doc.go
├── testdata/
│   └── bucketing-vectors.json   Checked-in mirror of the cross-repo golden-vector fixture
├── openspec/
└── README.md
```

## Specification Rules

- `openspec/specs/` contains the currently accepted behavior and
  constraints for this SDK. `openspec/specs/sdk-go/spec.md` is the
  authoritative behavioral contract — `rollfuse/rollfuse`'s
  `platform-conformance-harness` (its Go driver) exercises this SDK
  directly against a real environment as one way of verifying it.
- `openspec/changes/` contains proposed deltas and implementation plans.
- A change design explains only the implementation of that change.
- Tests are part of the same task as the behavior they validate; anything
  touching concurrency must be verified under `go test ./... -race`.
