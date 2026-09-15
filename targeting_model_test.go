package rollfuse_test

import (
	"encoding/json"
	"os"
	"testing"

	rollfuse "github.com/rollfuse/go-sdk"
)

// targetingModelVectorsPath is expand-targeting-model task 1.1's fixture,
// shared verbatim with apps/api/internal/evaluation/domain/testdata.
const targetingModelVectorsPath = "testdata/targeting-model-vectors.json"

const wantSingleLeafVectorsPassing = 19

// wantCompositionVectorsPassing is exactly how many of
// targeting-model-vectors.json's vectors are section 5's own scope
// (composition/negation/nesting, individual targets, prerequisites) —
// matches apps/api's own TestTargetingModel_CompositionVectors count.
const wantCompositionVectorsPassing = 10

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
// as a composition/segment construct without needing the full shape.
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

type individualTargetVector struct {
	VariationKey string   `json:"variation_key"`
	SubjectKeys  []string `json:"subject_keys"`
}

type prerequisiteVector struct {
	FlagKey              string `json:"flag_key"`
	RequiredVariationKey string `json:"required_variation_key"`
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
	IndividualTargets    []individualTargetVector            `json:"individual_targets"`
	Prerequisites        []prerequisiteVector                `json:"prerequisites"`
	// PrerequisiteStates supplies, for each prerequisite flag key this
	// vector's own Prerequisites reference, the fixed variation key that
	// flag should resolve to — see buildPrerequisiteFlags' own doc
	// comment. Mirrors apps/api's own test fixture-consumption shape.
	PrerequisiteStates map[string]string `json:"prerequisite_states"`
	Segments           json.RawMessage   `json:"segments"`
}

func presentAndNonEmpty(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

func (v targetingModelVector) isSingleLeafScope() bool {
	if len(v.IndividualTargets) > 0 || len(v.Prerequisites) > 0 || presentAndNonEmpty(v.Segments) {
		return false
	}

	for _, rule := range v.Rules {
		if !rule.isSingleLeaf() || !rule.isSingleVariationOutcome() {
			return false
		}
	}

	return true
}

// isCompositionScope reports whether this vector is section 5's own
// scope: composition (AND/OR/negation/nesting), individual targets or
// prerequisites, excluding a segment reference (section 7, not
// implemented) or a rollout outcome. Partitions the fixture against
// isSingleLeafScope so the two tests never double-cover a vector.
func (v targetingModelVector) isCompositionScope() bool {
	if presentAndNonEmpty(v.Segments) {
		return false
	}

	for _, rule := range v.Rules {
		if !rule.isSingleVariationOutcome() {
			return false
		}
	}

	if len(v.IndividualTargets) > 0 || len(v.Prerequisites) > 0 {
		return true
	}

	for _, rule := range v.Rules {
		if !rule.isSingleLeaf() {
			return true
		}
	}

	return false
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

// buildFullFlagConfig is buildFlagConfig's section-5-scope counterpart:
// it decodes the full ClauseTree (composition, negation, nesting)
// directly via ClauseTree's own UnmarshalJSON, and attaches
// IndividualTargets/Prerequisites.
func (v targetingModelVector) buildFullFlagConfig(t *testing.T) rollfuse.FlagConfig {
	t.Helper()

	variations := make([]rollfuse.Variation, 0, len(v.Variations))
	for _, variation := range v.Variations {
		variations = append(variations, rollfuse.Variation{Key: variation.Key, Value: json.RawMessage(`null`)})
	}

	rules := make([]rollfuse.Rule, 0, len(v.Rules))

	for _, rule := range v.Rules {
		var condition *rollfuse.ClauseTree

		if len(rule.Clause) > 0 && string(rule.Clause) != "null" {
			var tree rollfuse.ClauseTree
			if err := json.Unmarshal(rule.Clause, &tree); err != nil {
				t.Fatalf("decode clause tree: %v", err)
			}

			condition = &tree
		}

		rules = append(rules, rollfuse.Rule{
			Condition: condition,
			Outcome:   rollfuse.Outcome{VariationKey: rule.Outcome.VariationKey},
		})
	}

	individualTargets := make([]rollfuse.IndividualTarget, 0, len(v.IndividualTargets))
	for _, target := range v.IndividualTargets {
		individualTargets = append(individualTargets, rollfuse.IndividualTarget{
			VariationKey: target.VariationKey,
			SubjectKeys:  target.SubjectKeys,
		})
	}

	prerequisites := make([]rollfuse.Prerequisite, 0, len(v.Prerequisites))
	for _, p := range v.Prerequisites {
		prerequisites = append(prerequisites, rollfuse.Prerequisite{
			FlagKey:              p.FlagKey,
			RequiredVariationKey: p.RequiredVariationKey,
		})
	}

	return rollfuse.FlagConfig{
		FlagKey:           v.FlagKey,
		Enabled:           true,
		DefaultVariation:  v.DefaultVariationKey,
		Rules:             rules,
		Variations:        variations,
		IndividualTargets: individualTargets,
		Prerequisites:     prerequisites,
	}
}

// buildPrerequisiteFlags synthesizes a minimal FlagConfig for every entry
// in v.PrerequisiteStates: an always-enabled, unconditional flag whose
// single Variation and Rule resolve deterministically to the stated
// value, regardless of subject or attributes — mirrors apps/api's own
// test helper of the same name and purpose.
func (v targetingModelVector) buildPrerequisiteFlags(t *testing.T) []rollfuse.FlagConfig {
	t.Helper()

	flags := make([]rollfuse.FlagConfig, 0, len(v.PrerequisiteStates))

	for flagKey, variationKey := range v.PrerequisiteStates {
		flags = append(flags, rollfuse.FlagConfig{
			FlagKey:          flagKey,
			Enabled:          true,
			DefaultVariation: variationKey,
			Variations:       []rollfuse.Variation{{Key: variationKey, Value: json.RawMessage(`null`)}},
			Rules:            []rollfuse.Rule{{Outcome: rollfuse.Outcome{VariationKey: variationKey}}},
		})
	}

	return flags
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

			result := rollfuse.EvaluateFlagTyped(nil, flag, 1, vector.SubjectKey, attributes)

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

// TestTargetingModel_CompositionVectors runs every composition/
// individual-target/prerequisite vector through EvaluateFlagTyped,
// section 5's own scope. Manually verified load-bearing: temporarily
// changing ClauseTree.Match's "or" case to require every child to match
// (turning it into "and") turned the OR-composition vector red;
// reordering evaluateFlag to check rules before individual targets
// turned the "individual target overrides a differently-matching rule"
// vector red; removing the prerequisite-resolution loop entirely turned
// both prerequisite vectors red; all reverted before committing.
func TestTargetingModel_CompositionVectors(t *testing.T) {
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
		if !vector.isCompositionScope() {
			continue
		}

		ran++
		vector := vector

		t.Run(vector.Description, func(t *testing.T) {
			flag := vector.buildFullFlagConfig(t)
			attributes := vector.buildAttributes(t)
			flags := append(vector.buildPrerequisiteFlags(t), flag)

			result := rollfuse.EvaluateFlagTyped(flags, flag, 1, vector.SubjectKey, attributes)

			if result.VariationKey != vector.ExpectedVariationKey {
				t.Errorf("variation key = %q, want %q", result.VariationKey, vector.ExpectedVariationKey)
			}

			if string(result.Reason) != vector.ExpectedReason {
				t.Errorf("reason = %q, want %q", result.Reason, vector.ExpectedReason)
			}
		})
	}

	if ran != wantCompositionVectorsPassing {
		t.Fatalf("ran %d composition/individual-target/prerequisite vectors, want exactly %d (fixture shape changed)", ran, wantCompositionVectorsPassing)
	}
}
