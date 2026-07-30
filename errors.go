package growthops

import "errors"

// ErrCredentialRequired is returned by NewClient when constructed with an
// empty credential. The credential is never read from an environment
// variable or any other ambient source.
var ErrCredentialRequired = errors.New("growthops: credential is required")

// ErrConfigNotReady is returned by Evaluate/EvaluateAll when no
// Configuration has been successfully cached yet (or the cached one is
// older than WithMaxConfigAge, if set) and no fallback was supplied to
// Evaluate.
var ErrConfigNotReady = errors.New("growthops: configuration not yet available")

// ErrFlagNotFound is returned by Evaluate when flagKey does not exist in
// the cached Configuration's own Project (mirroring POST /v1/evaluate's
// 404 for an unknown flag key) and no fallback was supplied.
var ErrFlagNotFound = errors.New("growthops: flag not found in the cached configuration")
