package rollfuse_test

import (
	"encoding/json"
	"os"
	"testing"

	rollfuse "github.com/rollfuse/go-sdk"
)

func testFlag(overrides func(*rollfuse.FlagConfig)) rollfuse.FlagConfig {
	flag := rollfuse.FlagConfig{
		FlagKey:          "checkout-redesign",
		Enabled:          true,
		DefaultVariation: "off",
		Variations: []rollfuse.Variation{
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
	flag := testFlag(func(f *rollfuse.FlagConfig) {
		f.Enabled = false
		f.Rules = []rollfuse.Rule{{Outcome: rollfuse.Outcome{VariationKey: "on"}}}
	})

	result := rollfuse.EvaluateFlag(flag, 1, "user_1", nil)

	if result.Reason != rollfuse.ReasonDefaultDisabled {
		t.Fatalf("expected ReasonDefaultDisabled, got %v", result.Reason)
	}

	if result.VariationKey != "off" || result.TrackExposure {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestEvaluateFlag_NoRuleMatches(t *testing.T) {
	flag := testFlag(func(f *rollfuse.FlagConfig) {
		f.Rules = []rollfuse.Rule{
			{
				Conditions: []rollfuse.Condition{{Attribute: "plan", Value: "enterprise"}},
				Outcome:    rollfuse.Outcome{VariationKey: "on"},
			},
		}
	})

	result := rollfuse.EvaluateFlag(flag, 1, "user_1", map[string]string{"plan": "starter"})

	if result.Reason != rollfuse.ReasonDefaultNoRuleMatch {
		t.Fatalf("expected ReasonDefaultNoRuleMatch, got %v", result.Reason)
	}
}

func TestEvaluateFlag_MissingAttributeNeverMatches(t *testing.T) {
	flag := testFlag(func(f *rollfuse.FlagConfig) {
		f.Rules = []rollfuse.Rule{
			{
				Conditions: []rollfuse.Condition{{Attribute: "plan", Value: "enterprise"}},
				Outcome:    rollfuse.Outcome{VariationKey: "on"},
			},
		}
	})

	result := rollfuse.EvaluateFlag(flag, 1, "user_1", nil)

	if result.Reason != rollfuse.ReasonDefaultNoRuleMatch {
		t.Fatalf("expected ReasonDefaultNoRuleMatch, got %v", result.Reason)
	}
}

func TestEvaluateFlag_RuleMatchTracksExposure(t *testing.T) {
	flag := testFlag(func(f *rollfuse.FlagConfig) {
		f.Rules = []rollfuse.Rule{
			{
				Conditions: []rollfuse.Condition{{Attribute: "plan", Value: "enterprise"}},
				Outcome:    rollfuse.Outcome{VariationKey: "on"},
			},
		}
	})

	result := rollfuse.EvaluateFlag(flag, 3, "user_1", map[string]string{"plan": "enterprise"})

	if result.Reason != rollfuse.ReasonRuleMatch || result.VariationKey != "on" || !result.TrackExposure {
		t.Fatalf("unexpected result: %+v", result)
	}

	if result.ConfigVersion != 3 {
		t.Fatalf("expected config version 3, got %d", result.ConfigVersion)
	}
}

func TestEvaluateFlag_FirstMatchWins(t *testing.T) {
	flag := testFlag(func(f *rollfuse.FlagConfig) {
		f.Rules = []rollfuse.Rule{
			{
				Conditions: []rollfuse.Condition{{Attribute: "plan", Value: "enterprise"}},
				Outcome:    rollfuse.Outcome{VariationKey: "on"},
			},
			{Outcome: rollfuse.Outcome{VariationKey: "off"}}, // unconditional catch-all, would also match
		}
	})

	result := rollfuse.EvaluateFlag(flag, 1, "user_1", map[string]string{"plan": "enterprise"})

	if result.VariationKey != "on" {
		t.Fatalf("expected the earlier rule to win, got variation %q", result.VariationKey)
	}
}

func TestEvaluateFlag_UnresolvableOutcomeFallsBack(t *testing.T) {
	flag := testFlag(func(f *rollfuse.FlagConfig) {
		f.Rules = []rollfuse.Rule{{Outcome: rollfuse.Outcome{VariationKey: "does-not-exist"}}}
	})

	result := rollfuse.EvaluateFlag(flag, 1, "user_1", nil)

	if result.Reason != rollfuse.ReasonDefaultFallback || result.VariationKey != "off" || result.TrackExposure {
		t.Fatalf("unexpected result: %+v", result)
	}
}

// TestEvaluateFlag_NeverPanicsOnUnexpectedShape exercises
// harden-sdk-runtime task 5.3's "Evaluation with an unexpected shape"
// scenario. Go's structs cannot hold a truly arbitrary shape the way a
// dynamically-typed language's decoded JSON value can — there is no
// pointer field here to be nil, and json.Unmarshal leaves an omitted or
// mistyped-then-rejected field at its zero value rather than an unsafe
// one — so every case below is a zero-value or otherwise degenerate but
// well-typed FlagConfig a caller could still construct directly (bypassing
// configurationClient's own fetch-time validation entirely, exactly as a
// direct caller of evaluate.ts's exported evaluateFlag can in the JS
// clients). Every case must return the flag's default variation rather
// than panicking.
func TestEvaluateFlag_NeverPanicsOnUnexpectedShape(t *testing.T) {
	cases := map[string]rollfuse.FlagConfig{
		"zero-value FlagConfig": {},
		"nil Variations, rule referencing a variation": {
			FlagKey: "f", Enabled: true, DefaultVariation: "off",
			Rules: []rollfuse.Rule{{Outcome: rollfuse.Outcome{VariationKey: "on"}}},
		},
		"nil Rules": {
			FlagKey: "f", Enabled: true, DefaultVariation: "off",
			Variations: []rollfuse.Variation{{Key: "off"}},
		},
		"a Rule with a zero-value Outcome (no VariationKey, no Rollout)": {
			FlagKey: "f", Enabled: true, DefaultVariation: "off",
			Variations: []rollfuse.Variation{{Key: "off"}},
			Rules:      []rollfuse.Rule{{Outcome: rollfuse.Outcome{}}},
		},
		"a rollout split with a zero Percentage": {
			FlagKey: "f", Enabled: true, DefaultVariation: "off",
			Variations: []rollfuse.Variation{{Key: "off"}},
			Rules: []rollfuse.Rule{{Outcome: rollfuse.Outcome{
				Rollout: []rollfuse.RolloutSplit{{VariationKey: "on", Percentage: 0}},
			}}},
		},
		"DefaultVariation not present among Variations": {
			FlagKey: "f", Enabled: false, DefaultVariation: "does-not-exist",
			Variations: []rollfuse.Variation{{Key: "off"}},
		},
		"a Condition with an empty Attribute": {
			FlagKey: "f", Enabled: true, DefaultVariation: "off",
			Variations: []rollfuse.Variation{{Key: "off"}},
			Rules: []rollfuse.Rule{{
				Conditions: []rollfuse.Condition{{Attribute: "", Value: "x"}},
				Outcome:    rollfuse.Outcome{VariationKey: "off"},
			}},
		},
	}

	for name, flag := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("EvaluateFlag panicked: %v", r)
				}
			}()

			result := rollfuse.EvaluateFlag(flag, 1, "user_1", nil)

			if result.Reason == "" {
				t.Fatalf("expected a non-empty reason, got %+v", result)
			}
		})
	}
}

func TestEvaluateFlag_Deterministic(t *testing.T) {
	flag := testFlag(func(f *rollfuse.FlagConfig) {
		f.Rules = []rollfuse.Rule{
			{
				Outcome: rollfuse.Outcome{
					Rollout: []rollfuse.RolloutSplit{
						{VariationKey: "on", Percentage: 50},
						{VariationKey: "off", Percentage: 50},
					},
				},
			},
		}
	})

	first := rollfuse.EvaluateFlag(flag, 1, "user_123", nil)

	for i := 0; i < 20; i++ {
		got := rollfuse.EvaluateFlag(flag, 1, "user_123", nil)
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

	flag := testFlag(func(f *rollfuse.FlagConfig) {
		f.Rules = []rollfuse.Rule{
			{
				Outcome: rollfuse.Outcome{
					Rollout: []rollfuse.RolloutSplit{
						{VariationKey: "on", Percentage: 30},
						{VariationKey: "off", Percentage: 70},
					},
				},
			},
		}
	})

	for _, v := range vectors[:min(50, len(vectors))] {
		bucket := rollfuse.Bucket(flag.FlagKey, v.SubjectKey)

		expected := "off"
		if bucket < 3000 {
			expected = "on"
		}

		result := rollfuse.EvaluateFlag(flag, 1, v.SubjectKey, nil)

		if result.VariationKey != expected {
			t.Fatalf("subject %q: expected variation %q (bucket %d), got %q", v.SubjectKey, expected, bucket, result.VariationKey)
		}

		if result.Reason != rollfuse.ReasonRuleMatch {
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
