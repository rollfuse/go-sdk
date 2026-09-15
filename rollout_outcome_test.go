package rollfuse_test

import (
	"encoding/json"
	"os"
	"testing"

	rollfuse "github.com/rollfuse/go-sdk"
)

// rolloutOutcomeVectorsPath points at testdata/rollout-outcome-vectors.json,
// the second half of the cross-implementation conformance fixture (see
// bucketingGoldenVectorsPath's own comment for its cross-repo role): every
// vector here targets a subject whose bucket falls exactly on, or just
// beside, a rollout split's cumulative boundary, per
// rollfuse/sdk-conformance-fixtures's README and harden-sdk-runtime task
// 1.1. This is what a fixture covering only bucketing cannot catch: an
// off-by-one in how a client walks cumulative split ranges.
const rolloutOutcomeVectorsPath = "testdata/rollout-outcome-vectors.json"

type rolloutSplitVector struct {
	VariationKey string `json:"variation_key"`
	Percentage   int    `json:"percentage"`
}

type rolloutOutcomeVector struct {
	Description          string               `json:"description"`
	FlagKey              string               `json:"flag_key"`
	SubjectKey           string               `json:"subject_key"`
	Rollout              []rolloutSplitVector `json:"rollout"`
	DefaultVariationKey  string               `json:"default_variation_key"`
	ExpectedVariationKey *string              `json:"expected_variation_key"`
}

func (v rolloutOutcomeVector) buildFlagConfig() rollfuse.FlagConfig {
	seen := map[string]bool{v.DefaultVariationKey: true}
	variations := []rollfuse.Variation{{Key: v.DefaultVariationKey, Value: json.RawMessage(`null`)}}

	splits := make([]rollfuse.RolloutSplit, 0, len(v.Rollout))

	for _, s := range v.Rollout {
		splits = append(splits, rollfuse.RolloutSplit{VariationKey: s.VariationKey, Percentage: float64(s.Percentage)})

		if !seen[s.VariationKey] {
			seen[s.VariationKey] = true
			variations = append(variations, rollfuse.Variation{Key: s.VariationKey, Value: json.RawMessage(`null`)})
		}
	}

	return rollfuse.FlagConfig{
		FlagKey:          v.FlagKey,
		Enabled:          true,
		DefaultVariation: v.DefaultVariationKey,
		Variations:       variations,
		Rules: []rollfuse.Rule{
			{Outcome: rollfuse.Outcome{Rollout: splits}},
		},
	}
}

// TestRolloutOutcome_GoldenVectors asserts this SDK's EvaluateFlag —
// exercised through its public entry point, exactly as an integrator
// calls it, not the unexported resolve() — matches every vector in the
// checked-in cross-implementation fixture.
func TestRolloutOutcome_GoldenVectors(t *testing.T) {
	raw, err := os.ReadFile(rolloutOutcomeVectorsPath)
	if err != nil {
		t.Fatalf("reading rollout outcome vectors fixture: %v", err)
	}

	var vectors []rolloutOutcomeVector
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("parsing rollout outcome vectors fixture: %v", err)
	}

	if len(vectors) == 0 {
		t.Fatal("expected a non-empty set of rollout outcome vectors")
	}

	for _, v := range vectors {
		flag := v.buildFlagConfig()

		result := rollfuse.EvaluateFlag(nil, flag, 1, v.SubjectKey, nil)

		if v.ExpectedVariationKey == nil {
			if result.Reason != rollfuse.ReasonDefaultFallback {
				t.Errorf("%s: EvaluateFlag(flag=%q, subject=%q) reason = %q, want %q (fallback)",
					v.Description, v.FlagKey, v.SubjectKey, result.Reason, rollfuse.ReasonDefaultFallback)
			}

			if result.VariationKey != v.DefaultVariationKey {
				t.Errorf("%s: EvaluateFlag(flag=%q, subject=%q) variation = %q, want default %q",
					v.Description, v.FlagKey, v.SubjectKey, result.VariationKey, v.DefaultVariationKey)
			}

			continue
		}

		if result.VariationKey != *v.ExpectedVariationKey {
			t.Errorf("%s: EvaluateFlag(flag=%q, subject=%q) variation = %q, want %q (fixture drifted from EvaluateFlag()'s current output)",
				v.Description, v.FlagKey, v.SubjectKey, result.VariationKey, *v.ExpectedVariationKey)
		}

		if result.Reason != rollfuse.ReasonRuleMatch {
			t.Errorf("%s: EvaluateFlag(flag=%q, subject=%q) reason = %q, want %q",
				v.Description, v.FlagKey, v.SubjectKey, result.Reason, rollfuse.ReasonRuleMatch)
		}
	}
}
