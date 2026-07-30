package growthops

import "hash/fnv"

// bucketModulus is the granularity of the bucketing space: 10000 gives
// basis-point resolution (0.01%) for percentage rollouts, matching
// apps/api/internal/evaluation/domain/bucketing.go's own constant.
const bucketModulus = 10000

// percentageScale converts a rollout split's whole-percentage share
// (1-100) into the same basis-point space as Bucket, so a split of e.g.
// 50% covers exactly half of the 10000-wide bucket space.
const percentageScale = bucketModulus / 100

// Bucket deterministically maps a (flagKey, subjectKey) pair to an
// integer in [0, bucketModulus). It reproduces
// apps/api/internal/evaluation/domain/bucketing.go's Bucket exactly: both
// use Go's hash/fnv standard library package, so there is no
// cross-implementation reimplementation risk here the way there was for
// packages/sdk-js's JavaScript port. Verified against the same shared
// golden-vector fixture as sdk-js
// (packages/sdk-js/test/fixtures/bucketing-vectors.json) in
// bucketing_test.go, as a regression check.
//
// This algorithm is a stable, versioned contract: it MUST NOT change in
// place. Any future change must be additive (a new, separately-named
// algorithm), never an in-place replacement of this function's behavior —
// doing so would silently reassign existing rollout subjects to different
// variations.
func Bucket(flagKey, subjectKey string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(flagKey))
	_, _ = h.Write([]byte(":"))
	_, _ = h.Write([]byte(subjectKey))

	return h.Sum32() % bucketModulus
}
