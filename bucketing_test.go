package rollfuse_test

import (
	"encoding/json"
	"os"
	"testing"

	rollfuse "github.com/jeanmolossi/rollfuse/packages/sdk-go"
)

// TestBucket_Deterministic exercises tasks.md 2.2's stability half: the
// same (flagKey, subjectKey) pair must always bucket identically, across
// repeated calls, regardless of call order.
func TestBucket_Deterministic(t *testing.T) {
	first := rollfuse.Bucket("checkout-redesign", "user_123")

	for i := 0; i < 100; i++ {
		if got := rollfuse.Bucket("checkout-redesign", "user_123"); got != first {
			t.Fatalf("expected stable bucket %d, got %d on call %d", first, got, i)
		}
	}
}

// TestBucket_IndependentAcrossFlags exercises tasks.md 2.2's independence
// requirement: the same subject key must not deterministically bucket
// identically across different flag keys.
func TestBucket_IndependentAcrossFlags(t *testing.T) {
	differs := false

	for i := 0; i < 50; i++ {
		subject := "user_" + string(rune('a'+i))

		bucketA := rollfuse.Bucket("flag-a", subject)
		bucketB := rollfuse.Bucket("flag-b", subject)

		if bucketA != bucketB {
			differs = true
			break
		}
	}

	if !differs {
		t.Fatal("expected bucket assignment to differ across flag keys for at least one subject")
	}
}

// TestBucket_WithinRange checks the documented [0, 10000) basis-point
// bucket space is respected.
func TestBucket_WithinRange(t *testing.T) {
	for i := 0; i < 200; i++ {
		subject := "subject-" + string(rune('a'+i%26)) + string(rune('0'+i%10))

		bucket := rollfuse.Bucket("some-flag", subject)
		if bucket >= 10000 {
			t.Fatalf("expected bucket in [0, 10000), got %d", bucket)
		}
	}
}

// bucketingGoldenVectorsPath points at the fixture add-sdk-js's tasks.md
// 1.1 generated once from apps/api's own Bucket() and checked in for
// every SDK to reproduce bit-for-bit — packages/evaluation-core
// (extracted from packages/sdk-js by add-public-client-credential; both
// packages/sdk-js and packages/sdk-browser depend on it) consumes the
// same file. This is the cross-implementation parity contract described
// in design.md: if a future edit to any of the implementations (API,
// evaluation-core, sdk-go) ever changes its output for any of these
// vectors, this test fails here, and the equivalent tests fail
// identically in the others, so no implementation can silently drift
// from the others.
const bucketingGoldenVectorsPath = "../evaluation-core/test/fixtures/bucketing-vectors.json"

type bucketingGoldenVector struct {
	FlagKey    string `json:"flag_key"`
	SubjectKey string `json:"subject_key"`
	Bucket     uint32 `json:"bucket"`
}

// TestBucket_GoldenVectors asserts the checked-in cross-implementation
// fixture matches this package's current Bucket() output.
func TestBucket_GoldenVectors(t *testing.T) {
	raw, err := os.ReadFile(bucketingGoldenVectorsPath)
	if err != nil {
		t.Fatalf("reading golden vectors fixture: %v", err)
	}

	var vectors []bucketingGoldenVector
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("parsing golden vectors fixture: %v", err)
	}

	if len(vectors) == 0 {
		t.Fatal("expected a non-empty set of golden vectors")
	}

	for _, v := range vectors {
		if got := rollfuse.Bucket(v.FlagKey, v.SubjectKey); got != v.Bucket {
			t.Errorf("Bucket(%q, %q) = %d, want %d (fixture drifted from Bucket()'s current output)", v.FlagKey, v.SubjectKey, got, v.Bucket)
		}
	}
}
