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

// ErrCredentialRejected is returned (wrapped, with the rejecting status
// code) by Start when GET /v1/config answers 401 or 403: retrying a
// rejected credential can only ever reproduce the same rejection, so the
// configuration client fails immediately and permanently instead of
// retrying with backoff forever (task 2.2). Match it with errors.Is.
var ErrCredentialRejected = errors.New("rollfuse: credential was rejected by the platform")
