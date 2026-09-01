# SDK-Go Specification

## Purpose

Define the behavior of the Go SDK for rollfuse: deterministic local flag
evaluation from a cached Configuration, background refresh without
blocking evaluation, concurrency-safe evaluation across goroutines, safe
fallback and failure isolation when the platform is unreachable,
asynchronous best-effort exposure reporting, and explicit credential
configuration.

## Requirements

### Requirement: Deterministic Local Evaluation

The SDK MUST resolve a flag key to a variation for a given subject key and
attributes entirely from its cached Configuration, without a network call,
and MUST produce the same result the API would produce for the same
Configuration Version, flag, subject key and attributes: rules evaluated in
their given order with first-match-wins, a rule's conditions matching only
when every condition's attribute is present in the supplied attributes and
equal to its value, and a rollout outcome resolved via the documented
stable bucketing algorithm (FNV-1a of `flagKey + ":" + subjectKey`, modulo
10000, walked against the rollout's splits in array order using cumulative
bucket ranges).

#### Scenario: Evaluation matches the API for the same inputs

- **WHEN** the SDK evaluates a flag key for a subject key and attributes
  against a cached Configuration Version
- **THEN** the returned variation key MUST be identical to what the
  `POST /v1/evaluate` endpoint would return for the same Configuration
  Version, flag key, subject key and attributes

#### Scenario: Rules are evaluated in order, first match wins

- **GIVEN** a flag with two rules, an earlier one matching the given
  attributes and a later one that would also match
- **WHEN** the SDK evaluates that flag
- **THEN** the outcome MUST come from the earlier rule only

#### Scenario: A missing attribute never matches a condition

- **GIVEN** a rule with a condition on an attribute the caller did not
  supply
- **WHEN** the SDK evaluates that flag for that subject
- **THEN** that rule MUST NOT match

#### Scenario: Percentage rollout bucketing is stable

- **WHEN** the SDK evaluates the same flag key and subject key against the
  same Configuration Version multiple times
- **THEN** every evaluation MUST resolve to the same variation

#### Scenario: This SDK and rollfuse/rollfuse's other implementations agree on bucket assignment

- **WHEN** the same flag key and subject key are bucketed by this SDK and
  by any of `rollfuse/rollfuse`'s other implementations (the API itself,
  `packages/sdk-js`, `packages/evaluation-core`)
- **THEN** all of them MUST resolve to the same bucket
- **AND** this is verified against `testdata/bucketing-vectors.json`, a
  checked-in mirror of `rollfuse/rollfuse`'s
  `packages/evaluation-core/test/fixtures/bucketing-vectors.json` (the
  source of truth), kept from drifting by that repo's own CI

### Requirement: Configuration Caching And Background Refresh

The SDK MUST fetch the caller's Environment Configuration from `GET
/v1/config` and cache it in memory, MUST refresh that cache on a
configurable interval without blocking any in-flight or future evaluation
call, and MUST only replace the cached Configuration with a newly fetched
one after validating it matches the expected shape.

#### Scenario: Evaluation does not wait on a network call

- **GIVEN** the SDK has a cached Configuration
- **WHEN** an integrator calls the evaluation function
- **THEN** the call MUST return synchronously, without performing any
  network request

#### Scenario: A background refresh does not affect concurrent evaluation

- **WHEN** a scheduled Configuration refresh is in flight
- **THEN** evaluation calls made during that refresh, from any goroutine,
  MUST continue serving the previously cached Configuration until the
  refresh completes and validates successfully

#### Scenario: A malformed refresh response is rejected

- **WHEN** a scheduled refresh receives a response that does not match the
  expected Configuration shape
- **THEN** the SDK MUST discard that response
- **AND** MUST continue serving the last successfully validated
  Configuration

### Requirement: Concurrency-Safe Evaluation

The SDK MUST support the evaluation function being called concurrently by
multiple goroutines, and MUST support a background Configuration refresh
occurring concurrently with any number of in-flight evaluation calls,
without a data race and without any evaluation call observing a partially
updated Configuration.

#### Scenario: Concurrent evaluations during a refresh

- **GIVEN** many goroutines calling the evaluation function concurrently
- **WHEN** a background refresh replaces the cached Configuration mid-way
  through those calls
- **THEN** every in-flight evaluation call MUST observe either the
  Configuration that was cached before the refresh or the one cached after
  it, in full, never a mix of both
- **AND** running the SDK's test suite with the race detector enabled MUST
  report no data race

### Requirement: Safe Fallback Behavior

When the evaluation function is called before any Configuration has been
successfully cached, the SDK MUST return an integrator-supplied fallback
value if one was given for that call, and MUST return a distinguishable
error if none was given, rather than silently returning an incorrect
variation.

#### Scenario: No cache yet, fallback supplied

- **GIVEN** the SDK has not yet completed its first successful Configuration
  fetch
- **WHEN** the evaluation function is called with a fallback value
- **THEN** the SDK MUST return that fallback value
- **AND** MUST mark the result as a fallback, not a rule match or default

#### Scenario: No cache yet, no fallback supplied

- **GIVEN** the SDK has not yet completed its first successful Configuration
  fetch
- **WHEN** the evaluation function is called without a fallback value
- **THEN** the SDK MUST return a distinguishable "not ready" error rather
  than a guessed result

### Requirement: Failure Isolation From Platform Unavailability

Once the SDK has successfully cached a Configuration, subsequent inability
to reach the platform (refresh failures) MUST NOT cause evaluation calls to
fail or change their result; the SDK MUST keep serving the last
successfully validated Configuration.

#### Scenario: Refresh fails after a successful initial fetch

- **GIVEN** the SDK has a cached Configuration from a prior successful
  fetch
- **WHEN** a subsequent scheduled refresh fails (network error, non-2xx
  response, or timeout)
- **THEN** evaluation calls MUST continue to succeed using the previously
  cached Configuration
- **AND** MUST NOT return an error attributable to the failed refresh

### Requirement: Asynchronous, Best-Effort Exposure Reporting

For every evaluation whose result indicates a rule actually matched
(`track_exposure: true`), the SDK MUST enqueue an ExposureEvent for
reporting and MUST submit queued events to the platform asynchronously, in
batches, without adding latency to the evaluation call that produced them
and without causing that call to fail if reporting later fails or the
queue is full.

#### Scenario: Enqueuing an exposure does not block evaluation

- **WHEN** the evaluation function resolves a flag via a matching rule
- **THEN** the call MUST return without waiting for that exposure to be
  submitted to the platform

#### Scenario: Exposure submission failure does not affect past evaluations

- **GIVEN** a batch of queued ExposureEvents fails to submit
- **WHEN** that failure occurs
- **THEN** no previously returned evaluation result MUST be affected or
  invalidated by that failure

#### Scenario: A full exposure queue drops rather than blocks

- **GIVEN** the SDK's in-memory exposure queue is at its configured
  capacity
- **WHEN** a new evaluation would enqueue another ExposureEvent
- **THEN** the SDK MUST drop the new event rather than block the
  evaluation call
- **AND** MUST make the drop observable to the integrator (e.g. via a
  registered callback), not silently discard it without any signal

#### Scenario: A default or fallback evaluation is never reported as an exposure

- **WHEN** the evaluation function resolves to a flag's default variation
  or an integrator-supplied fallback
- **THEN** the SDK MUST NOT enqueue an ExposureEvent for that evaluation

### Requirement: Explicit Credential Configuration

The SDK MUST require its Credential to be supplied explicitly by the
integrator when constructing a client, and MUST NOT read it implicitly
from an environment variable or any other ambient source.

#### Scenario: Constructing a client without a credential

- **WHEN** an integrator constructs the client without supplying a
  Credential
- **THEN** construction MUST fail with a distinguishable error
- **AND** MUST NOT fall back to reading any environment variable
