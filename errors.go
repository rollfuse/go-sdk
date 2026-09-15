package rollfuse

import "errors"

// ErrCredentialRequired is returned by NewClient when constructed with an
// empty credential. The credential is never read from an environment
// variable or any other ambient source.
var ErrCredentialRequired = errors.New("rollfuse: credential is required")

// ErrConfigNotReady is returned by Evaluate/EvaluateAll when no
// Configuration has been successfully cached yet (or the cached one is
// older than WithMaxConfigAge, if set) and no fallback was supplied to
// Evaluate.
var ErrConfigNotReady = errors.New("rollfuse: configuration not yet available")

// ErrFlagNotFound is returned by Evaluate when flagKey does not exist in
// the cached Configuration's own Project (mirroring POST /v1/evaluate's
// 404 for an unknown flag key) and no fallback was supplied.
var ErrFlagNotFound = errors.New("rollfuse: flag not found in the cached configuration")

// ErrFlagNotEvaluable is returned by Evaluate when flagKey uses a
// configuration construct newer than this client's own clientFormatVersion
// declares support for (the platform marks such a flag
// FlagConfig.NonEvaluable and withholds its Rules/Variations). This
// client version needs an upgrade to evaluate it — per
// expand-targeting-model task 3.3, the caller's own fallback is served
// when one is supplied, exactly like ErrFlagNotFound, since there is no
// legitimate default value this client can derive for a flag definition
// it was never shown.
var ErrFlagNotEvaluable = errors.New("rollfuse: flag requires a newer client to evaluate")

// ErrCredentialRejected is returned (wrapped, with the rejecting status
// code) by Start when GET /v1/config answers 401 or 403: retrying a
// rejected credential can only ever reproduce the same rejection, so the
// configuration client fails immediately and permanently instead of
// retrying with backoff forever (task 2.2). Match it with errors.Is.
var ErrCredentialRejected = errors.New("rollfuse: credential was rejected by the platform")

// ErrRegexPatternUnbounded is returned by ValidateBoundedRegex when a
// pattern is not in the bounded RE2-safe subset expand-targeting-model's
// "A Clause Supports Operators Beyond Equality" requirement mandates.
var ErrRegexPatternUnbounded = errors.New("rollfuse: regex pattern is not in the bounded RE2-safe subset")
