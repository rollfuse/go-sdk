package growthops_test

import (
	"encoding/json"
	"os"
	"testing"

	growthops "github.com/jeanmolossi/growth-ops/packages/sdk-go"
)

func testFlag(overrides func(*growthops.FlagConfig)) growthops.FlagConfig {
	flag := growthops.FlagConfig{
		FlagKey:          "checkout-redesign",
		Enabled:          true,
		DefaultVariation: "off",
		Variations: []growthops.Variation{
			{Key: "on", Value: json.RawMessage(`true`)},
			{Key: "off", Value: json.RawMessage(`false`)},
		},
		Rules: nil,
	}

	if overrides != nil {
		overrides(&flag)
	}

	return flag
}

func TestEvaluateFlag_Disabled(t *testing.T) {
	flag := testFlag(func(f *growthops.FlagConfig) {
		f.Enabled = false
		f.Rules = []growthops.Rule{{Outcome: growthops.Outcome{VariationKey: "on"}}}
	})

	result := growthops.EvaluateFlag(flag, 1, "user_1", nil)

	if result.Reason != growthops.ReasonDefaultDisabled {
		t.Fatalf("expected ReasonDefaultDisabled, got %v", result.Reason)
	}

	if result.VariationKey != "off" || result.TrackExposure {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestEvaluateFlag_NoRuleMatches(t *testing.T) {
	flag := testFlag(func(f *growthops.FlagConfig) {
		f.Rules = []growthops.Rule{
			{
				Conditions: []growthops.Condition{{Attribute: "plan", Value: "enterprise"}},
				Outcome:    growthops.Outcome{VariationKey: "on"},
			},
		}
	})

	result := growthops.EvaluateFlag(flag, 1, "user_1", map[string]string{"plan": "starter"})

	if result.Reason != growthops.ReasonDefaultNoRuleMatch {
		t.Fatalf("expected ReasonDefaultNoRuleMatch, got %v", result.Reason)
	}
}

func TestEvaluateFlag_MissingAttributeNeverMatches(t *testing.T) {
	flag := testFlag(func(f *growthops.FlagConfig) {
		f.Rules = []growthops.Rule{
			{
				Conditions: []growthops.Condition{{Attribute: "plan", Value: "enterprise"}},
				Outcome:    growthops.Outcome{VariationKey: "on"},
			},
		}
	})

	result := growthops.EvaluateFlag(flag, 1, "user_1", nil)

	if result.Reason != growthops.ReasonDefaultNoRuleMatch {
		t.Fatalf("expected ReasonDefaultNoRuleMatch, got %v", result.Reason)
	}
}

func TestEvaluateFlag_RuleMatchTracksExposure(t *testing.T) {
	flag := testFlag(func(f *growthops.FlagConfig) {
		f.Rules = []growthops.Rule{
			{
				Conditions: []growthops.Condition{{Attribute: "plan", Value: "enterprise"}},
				Outcome:    growthops.Outcome{VariationKey: "on"},
			},
		}
	})

	result := growthops.EvaluateFlag(flag, 3, "user_1", map[string]string{"plan": "enterprise"})

	if result.Reason != growthops.ReasonRuleMatch || result.VariationKey != "on" || !result.TrackExposure {
		t.Fatalf("unexpected result: %+v", result)
	}

	if result.ConfigVersion != 3 {
		t.Fatalf("expected config version 3, got %d", result.ConfigVersion)
	}
}

func TestEvaluateFlag_FirstMatchWins(t *testing.T) {
	flag := testFlag(func(f *growthops.FlagConfig) {
		f.Rules = []growthops.Rule{
			{
				Conditions: []growthops.Condition{{Attribute: "plan", Value: "enterprise"}},
				Outcome:    growthops.Outcome{VariationKey: "on"},
			},
			{Outcome: growthops.Outcome{VariationKey: "off"}}, // unconditional catch-all, would also match
		}
	})

	result := growthops.EvaluateFlag(flag, 1, "user_1", map[string]string{"plan": "enterprise"})

	if result.VariationKey != "on" {
		t.Fatalf("expected the earlier rule to win, got variation %q", result.VariationKey)
	}
}

func TestEvaluateFlag_UnresolvableOutcomeFallsBack(t *testing.T) {
	flag := testFlag(func(f *growthops.FlagConfig) {
		f.Rules = []growthops.Rule{{Outcome: growthops.Outcome{VariationKey: "does-not-exist"}}}
	})

	result := growthops.EvaluateFlag(flag, 1, "user_1", nil)

	if result.Reason != growthops.ReasonDefaultFallback || result.VariationKey != "off" || result.TrackExposure {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestEvaluateFlag_Deterministic(t *testing.T) {
	flag := testFlag(func(f *growthops.FlagConfig) {
		f.Rules = []growthops.Rule{
			{
				Outcome: growthops.Outcome{
					Rollout: []growthops.RolloutSplit{
						{VariationKey: "on", Percentage: 50},
						{VariationKey: "off", Percentage: 50},
					},
				},
			},
		}
	})

	first := growthops.EvaluateFlag(flag, 1, "user_123", nil)

	for i := 0; i < 20; i++ {
		got := growthops.EvaluateFlag(flag, 1, "user_123", nil)
		if got.VariationKey != first.VariationKey || got.Reason != first.Reason {
			t.Fatalf("expected deterministic result, got %+v vs %+v", got, first)
		}
	}
}

// TestEvaluateFlag_RolloutMatchesGoldenVectors is the cross-check tasks.md
// 3.2 asks for: wiring the shared golden-vector fixture into a minimal
// single-rule percentage-rollout FlagConfig, the resolved variation must
// match what the bucket alone would predict.
func TestEvaluateFlag_RolloutMatchesGoldenVectors(t *testing.T) {
	vectors := loadGoldenVectors(t)

	flag := testFlag(func(f *growthops.FlagConfig) {
		f.Rules = []growthops.Rule{
			{
				Outcome: growthops.Outcome{
					Rollout: []growthops.RolloutSplit{
						{VariationKey: "on", Percentage: 30},
						{VariationKey: "off", Percentage: 70},
					},
				},
			},
		}
	})

	for _, v := range vectors[:min(50, len(vectors))] {
		bucket := growthops.Bucket(flag.FlagKey, v.SubjectKey)

		expected := "off"
		if bucket < 3000 {
			expected = "on"
		}

		result := growthops.EvaluateFlag(flag, 1, v.SubjectKey, nil)

		if result.VariationKey != expected {
			t.Fatalf("subject %q: expected variation %q (bucket %d), got %q", v.SubjectKey, expected, bucket, result.VariationKey)
		}

		if result.Reason != growthops.ReasonRuleMatch {
			t.Fatalf("subject %q: expected ReasonRuleMatch, got %v", v.SubjectKey, result.Reason)
		}
	}
}

func loadGoldenVectors(t *testing.T) []bucketingGoldenVector {
	t.Helper()

	raw, err := os.ReadFile(bucketingGoldenVectorsPath)
	if err != nil {
		t.Fatalf("reading golden vectors fixture: %v", err)
	}

	var vectors []bucketingGoldenVector
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatalf("parsing golden vectors fixture: %v", err)
	}

	return vectors
}
