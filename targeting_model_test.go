package rollfuse_test

import (
	"encoding/json"
	"os"
	"testing"

	rollfuse "github.com/rollfuse/go-sdk"
)

// targetingModelVectorsPath is expand-targeting-model task 1.1's fixture,
// shared verbatim with apps/api/internal/evaluation/domain/testdata (see
// that package's own targeting_model_test.go for the scoping rationale).
// Task 4.7 only implements single-leaf clauses; composition, individual
// targets, prerequisites and segment-membership clauses are sections
// 5-7's job. This test filters to the same set apps/api's own test
// proved passing — 19 vectors — and fails loudly if that count drifts.
const targetingModelVectorsPath = "testdata/targeting-model-vectors.json"

const wantSingleLeafVectorsPassing = 19

type targetingRuleVector struct {
	Clause  json.RawMessage `json:"clause"`
	Outcome struct {
		VariationKey string          `json:"variation_key"`
		Rollout      json.RawMessage `json:"rollout"`
	} `json:"outcome"`
}

func (r targetingRuleVector) isSingleVariationOutcome() bool {
	return len(r.Outcome.Rollout) == 0 || string(r.Outcome.Rollout) == "null"
}

// clauseOpOnly decodes only the "op" field, enough to classify a clause
// as a composition/segment construct (out of section 4's scope) without
// needing the full Clause shape.
type clauseOpOnly struct {
	Op string `json:"op"`
}

func (r targetingRuleVector) isSingleLeaf() bool {
	if len(r.Clause) == 0 || string(r.Clause) == "null" {
		return true
	}

	var probe clauseOpOnly
	if err := json.Unmarshal(r.Clause, &probe); err != nil {
		return true
	}

	switch probe.Op {
	case "and", "or", "not", "segment":
		return false
	default:
		return true
	}
}

type targetingAttributeVector struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

type targetingModelVector struct {
	Description string `json:"description"`
	FlagKey     string `json:"flag_key"`
	Variations  []struct {
		Key string `json:"key"`
	} `json:"variations"`
	DefaultVariationKey  string                              `json:"default_variation_key"`
	Rules                []targetingRuleVector               `json:"rules"`
	SubjectKey           string                              `json:"subject_key"`
	Attributes           map[string]targetingAttributeVector `json:"attributes"`
	ExpectedVariationKey string                              `json:"expected_variation_key"`
	ExpectedReason       string                              `json:"expected_reason"`
	IndividualTargets    json.RawMessage                     `json:"individual_targets"`
	Prerequisites        json.RawMessage                     `json:"prerequisites"`
	Segments             json.RawMessage                     `json:"segments"`
}

func presentAndNonEmpty(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

func (v targetingModelVector) isSingleLeafScope() bool {
	if presentAndNonEmpty(v.IndividualTargets) || presentAndNonEmpty(v.Prerequisites) || presentAndNonEmpty(v.Segments) {
		return false
	}

	for _, rule := range v.Rules {
		if !rule.isSingleLeaf() || !rule.isSingleVariationOutcome() {
			return false
		}
	}

	return true
}

func (v targetingModelVector) buildFlagConfig(t *testing.T) rollfuse.FlagConfig {
	t.Helper()

	variations := make([]rollfuse.Variation, 0, len(v.Variations))
	for _, variation := range v.Variations {
		variations = append(variations, rollfuse.Variation{Key: variation.Key, Value: json.RawMessage(`null`)})
	}

	rules := make([]rollfuse.Rule, 0, len(v.Rules))

	for _, rule := range v.Rules {
		var clauses []rollfuse.Clause

		if len(rule.Clause) > 0 && string(rule.Clause) != "null" {
			var clause rollfuse.Clause
			if err := json.Unmarshal(rule.Clause, &clause); err != nil {
				t.Fatalf("decode clause: %v", err)
			}

			clauses = []rollfuse.Clause{clause}
		}

		rules = append(rules, rollfuse.Rule{
			Clauses: clauses,
			Outcome: rollfuse.Outcome{VariationKey: rule.Outcome.VariationKey},
		})
	}

	return rollfuse.FlagConfig{
		FlagKey:          v.FlagKey,
		Enabled:          true,
		DefaultVariation: v.DefaultVariationKey,
		Rules:            rules,
		Variations:       variations,
	}
}

func (v targetingModelVector) buildAttributes(t *testing.T) map[string]rollfuse.AttributeValue {
	t.Helper()

	attributes := make(map[string]rollfuse.AttributeValue, len(v.Attributes))

	for name, attr := range v.Attributes {
		value := rollfuse.AttributeValue{Type: rollfuse.AttributeType(attr.Type)}

		switch attr.Type {
		case "number":
			if err := json.Unmarshal(attr.Value, &value.Number); err != nil {
				t.Fatalf("decode number attribute %q: %v", name, err)
			}
		case "boolean":
			if err := json.Unmarshal(attr.Value, &value.Boolean); err != nil {
				t.Fatalf("decode boolean attribute %q: %v", name, err)
			}
		case "list":
			if err := json.Unmarshal(attr.Value, &value.List); err != nil {
				t.Fatalf("decode list attribute %q: %v", name, err)
			}
		default:
			if err := json.Unmarshal(attr.Value, &value.String); err != nil {
				t.Fatalf("decode string attribute %q: %v", name, err)
			}
		}

		attributes[name] = value
	}

	return attributes
}

// TestTargetingModel_SingleLeafClauseVectors runs every single-leaf-clause
// vector in targeting-model-vectors.json through EvaluateFlagTyped — the
// same public entry point Client.Evaluate uses — and asserts the
// expected variation and reason. Manually verified load-bearing:
// temporarily changing matchOrdered's "<=" to "<" turned the lte
// boundary vector red; restored before committing.
func TestTargetingModel_SingleLeafClauseVectors(t *testing.T) {
	data, err := os.ReadFile(targetingModelVectorsPath)
	if err != nil {
		t.Fatalf("read %s: %v", targetingModelVectorsPath, err)
	}

	var vectors []targetingModelVector
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatalf("unmarshal %s: %v", targetingModelVectorsPath, err)
	}

	ran := 0

	for _, vector := range vectors {
		if !vector.isSingleLeafScope() {
			continue
		}

		ran++
		vector := vector

		t.Run(vector.Description, func(t *testing.T) {
			flag := vector.buildFlagConfig(t)
			attributes := vector.buildAttributes(t)

			result := rollfuse.EvaluateFlagTyped(flag, 1, vector.SubjectKey, attributes)

			if result.VariationKey != vector.ExpectedVariationKey {
				t.Errorf("variation key = %q, want %q", result.VariationKey, vector.ExpectedVariationKey)
			}

			if string(result.Reason) != vector.ExpectedReason {
				t.Errorf("reason = %q, want %q", result.Reason, vector.ExpectedReason)
			}
		})
	}

	if ran != wantSingleLeafVectorsPassing {
		t.Fatalf("ran %d single-leaf-clause vectors, want exactly %d (fixture shape changed)", ran, wantSingleLeafVectorsPassing)
	}
}
