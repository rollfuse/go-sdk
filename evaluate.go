package rollfuse

import "encoding/json"

// Variation is one possible value a FeatureFlag can resolve to.
type Variation struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

// RolloutSplit is one entry of a rollout Outcome: the share of matching
// subjects that should receive VariationKey. Percentage is a float64, not
// int (task 9.2's own fix): the wire has carried a fractional percentage
// since expand-targeting-model task 8.5, and an int field fails to
// JSON-decode a fractional value outright — crashing this client's ENTIRE
// configuration fetch, not just mis-evaluating the one flag using it.
// bucketPositions converts this split's wire percentage into bucket
// positions (the evaluator's own unit of truth, matching the platform's
// RolloutSplit.BucketPositions) via a single multiply-and-round at this
// one boundary, never re-derived per accumulation step — the same
// discipline the platform's own RolloutSplit applies, so a fractional
// split sums exactly rather than accumulating float drift.
type RolloutSplit struct {
	VariationKey string  `json:"variation_key"`
	Percentage   float64 `json:"percentage"`
}

// bucketPositions converts this split's wire percentage into bucket
// positions, the space Outcome.resolve actually accumulates in.
func (s RolloutSplit) bucketPositions() uint32 {
	return uint32(s.Percentage * float64(percentageScale))
}

// Outcome is what a Rule resolves to when it matches: either exactly one
// Variation key, or a percentage-based rollout split across two or more
// Variation keys.
type Outcome struct {
	VariationKey string         `json:"variation_key,omitempty"`
	Rollout      []RolloutSplit `json:"rollout,omitempty"`
}

// isRollout reports whether this Outcome is a percentage-based rollout
// rather than a single Variation.
func (o Outcome) isRollout() bool { return len(o.Rollout) > 0 }

// resolve picks the Variation key this Outcome assigns to subjectKey for
// flagKey, using the stable Bucket contract for rollout outcomes. ok is
// false only if a rollout's splits do not cover the bucket space, or a
// fixed-variation Outcome has an empty VariationKey.
func (o Outcome) resolve(flagKey, subjectKey string) (string, bool) {
	if !o.isRollout() {
		return o.VariationKey, o.VariationKey != ""
	}

	bucket := Bucket(flagKey, subjectKey)

	var cumulative uint32

	for _, split := range o.Rollout {
		cumulative += split.bucketPositions()
		if bucket < cumulative {
			return split.VariationKey, true
		}
	}

	return "", false
}

// Condition is one attribute-equality check a Rule requires to match.
// Deprecated: superseded by Clause (expand-targeting-model section 4),
// kept only so a Rule published before the wire contract grew Clauses
// still decodes and matches exactly as before — see Rule.matches.
type Condition struct {
	Attribute string `json:"attribute"`
	Value     string `json:"value"`
}

// Rule is one entry of a FlagConfig's ordered targeting list, evaluated
// in order; the first Rule whose condition matches (or that is
// unconditional) wins.
//
// A Rule carries Condition (the current wire shape, a full ClauseTree
// supporting AND/OR/negation/nesting per expand-targeting-model section
// 5), OR Clauses (a flat AND-only list, section 4's shape) OR Conditions
// (the shape every Rule had before section 4), never more than one in
// practice; matches prefers Condition, then Clauses, then Conditions, so
// a config already migrated to a richer model is never re-interpreted
// through an older, less expressive path.
type Rule struct {
	Condition  *ClauseTree `json:"condition,omitempty"`
	Clauses    []Clause    `json:"clauses,omitempty"`
	Conditions []Condition `json:"conditions,omitempty"`
	Outcome    Outcome     `json:"outcome"`
}

// matches reports whether this Rule applies to the given subject
// attributes. A Rule with no Condition, Clauses and no Conditions is an
// unconditional catch-all and always matches.
func (r Rule) matches(attributes map[string]AttributeValue) (bool, string) {
	if r.Condition != nil {
		return r.Condition.Match(attributes, 0)
	}

	if len(r.Clauses) > 0 {
		for _, clause := range r.Clauses {
			matched, diagnostic := clause.Match(attributes)
			if !matched {
				return false, diagnostic
			}
		}

		return true, ""
	}

	for _, condition := range r.Conditions {
		value, ok := attributes[condition.Attribute]
		if !ok || value.Type != AttributeTypeString || value.String != condition.Value {
			return false, ""
		}
	}

	return true, ""
}

// IndividualTarget serves VariationKey to every subject key listed in
// SubjectKeys, evaluated before any Rule or rollout —
// environment-flag-targeting's "A Flag May Target Named Individuals"
// requirement.
type IndividualTarget struct {
	VariationKey string   `json:"variation_key"`
	SubjectKeys  []string `json:"subject_keys"`
}

func (t IndividualTarget) includes(subjectKey string) bool {
	for _, key := range t.SubjectKeys {
		if key == subjectKey {
			return true
		}
	}

	return false
}

// Prerequisite is a dependency on another FlagConfig in the same
// Configuration serving RequiredVariationKey for the same subject before
// this flag's own targeting applies — environment-flag-targeting's "A
// Flag May Depend On A Prerequisite Flag" requirement. FlagKey is
// resolved against the same []FlagConfig slice Evaluate receives, not a
// separate lookup (this package makes no network calls from evaluation).
type Prerequisite struct {
	FlagKey              string `json:"flag_key"`
	RequiredVariationKey string `json:"required_variation_key"`
}

// clientFormatVersion is the highest configuration format version this
// package can evaluate — declared to the platform on every GET /v1/config
// request via configuration_client.go's X-Rollfuse-Client-Format-Version
// header, per expand-targeting-model task 3.1. Bump this only alongside
// actually implementing whatever new construct the next format version
// introduces (task 3.5: this client must never evaluate a construct it
// does not support).
//
// 2, matching FormatVersion2 (task 9.2's own finding and fix): section
// 5.8 already implemented ClauseTree composition, IndividualTarget and
// Prerequisite here, but this constant was left at 1 the whole time —
// meaning the platform has been marking every composed/individual-
// target/prerequisite flag non_evaluable for this client regardless,
// since format-version negotiation happens before evaluation ever runs.
// Every construct FormatVersion2 covers is genuinely implemented below
// as of this fix (composition/individual-targets/prerequisites since
// section 5.8, fractional rollout percentage since this same fix to
// RolloutSplit.Percentage).
const clientFormatVersion = 2

// FlagConfig is one FeatureFlag's per-Environment view: whether it is
// enabled, its default Variation, its ordered Rules, and the full set of
// Variations rule outcomes may reference.
//
// NonEvaluable is true when the platform withheld Rules/Variations
// because this flag uses a construct newer than clientFormatVersion (see
// expand-targeting-model task 3.3) — evaluate() serves the caller's own
// fallback for such a flag rather than treating an empty Rules/Variations
// as "no targeting configured," which would silently mis-evaluate it.
type FlagConfig struct {
	FlagKey           string             `json:"flag_key"`
	Enabled           bool               `json:"enabled"`
	DefaultVariation  string             `json:"default_variation"`
	Variations        []Variation        `json:"variations"`
	Rules             []Rule             `json:"rules"`
	NonEvaluable      bool               `json:"non_evaluable"`
	IndividualTargets []IndividualTarget `json:"individual_targets,omitempty"`
	Prerequisites     []Prerequisite     `json:"prerequisites,omitempty"`
}

func (f FlagConfig) hasVariation(key string) bool {
	for _, v := range f.Variations {
		if v.Key == key {
			return true
		}
	}

	return false
}

func (f FlagConfig) variationValue(key string) json.RawMessage {
	for _, v := range f.Variations {
		if v.Key == key {
			return v.Value
		}
	}

	return nil
}

// Configuration is the full published Configuration for one Environment at
// a point in time, as returned by GET /v1/config.
type Configuration struct {
	EnvironmentID string       `json:"environment_id"`
	Version       int64        `json:"version"`
	Flags         []FlagConfig `json:"flags"`
	// PollIntervalSeconds is the platform's advised interval for a client
	// polling GET /v1/config, honored in preference to this package's own
	// default when no explicit WithRefreshInterval was given (task 9.3).
	PollIntervalSeconds int64 `json:"poll_interval_seconds"`
}

// EvaluationReason explains why an evaluation returned the Variation it
// did. A closed, stable set matching the API's own EvaluationResult.reason
// enum.
type EvaluationReason string

const (
	// ReasonRuleMatch means an enabled FlagConfig's Rule matched the
	// subject (directly, or via its rollout bucket) and its Outcome
	// determined the Variation.
	ReasonRuleMatch EvaluationReason = "rule_match"
	// ReasonDefaultDisabled means the flag is disabled, so the default
	// Variation was served without evaluating any Rule.
	ReasonDefaultDisabled EvaluationReason = "default_disabled"
	// ReasonDefaultNoRuleMatch means the flag was enabled but no Rule
	// matched the subject, so the default Variation was served.
	ReasonDefaultNoRuleMatch EvaluationReason = "default_no_rule_match"
	// ReasonDefaultFallback is the last-resort safety net: a Rule matched
	// but its Outcome could not be resolved to a known Variation.
	ReasonDefaultFallback EvaluationReason = "default_fallback"
	// ReasonIndividualTarget means the subject key matched an
	// IndividualTarget listing — expand-targeting-model section 5.
	ReasonIndividualTarget EvaluationReason = "individual_target"
	// ReasonPrerequisiteUnsatisfied means a Prerequisite FlagConfig did
	// not resolve to its RequiredVariationKey for this subject, so the
	// default Variation was served without evaluating this flag's own
	// targets, Rules or rollout — expand-targeting-model section 5.
	ReasonPrerequisiteUnsatisfied EvaluationReason = "prerequisite_unsatisfied"
)

// EvaluationResult is the outcome of evaluating one FeatureFlag for one
// subject at one Configuration Version.
type EvaluationResult struct {
	FlagKey       string           `json:"flag_key"`
	VariationKey  string           `json:"variation_key"`
	Value         json.RawMessage  `json:"value"`
	Reason        EvaluationReason `json:"reason"`
	ConfigVersion int64            `json:"config_version"`
	TrackExposure bool             `json:"track_exposure"`
}

// EvaluateFlag deterministically assigns subjectKey (with attributes) a
// Variation of flag, entirely in-process, against flag and configVersion.
// Reproduces apps/api/internal/evaluation/domain/configuration.go's
// Evaluate/Outcome.resolve/Rule.matches line for line:
//
//   - a disabled flag always resolves to its default Variation
//     (ReasonDefaultDisabled), without evaluating any Rule;
//   - an enabled flag evaluates Rules in order; the first matching Rule's
//     Outcome (resolved via the stable Bucket contract for rollouts)
//     determines the Variation (ReasonRuleMatch);
//   - if no Rule matches, the default Variation is served
//     (ReasonDefaultNoRuleMatch);
//   - if a matched Rule's Outcome cannot be resolved to a known Variation,
//     the default Variation is still served (ReasonDefaultFallback)
//     rather than erroring.
//
// Never fails: every input resolves to some Variation of flag.
//
// attributes is map[string]string for backward compatibility with every
// caller before expand-targeting-model section 4: each value becomes a
// string-typed AttributeValue, which only ever matches an "eq"/"neq"/
// membership/string-operator clause expecting a string — exactly the
// equality-only behavior this function always had. A caller that needs
// a typed (number/boolean/list) attribute must use EvaluateFlagTyped.
//
// flags is the full Configuration.Flags slice flag itself came from,
// needed to resolve any Prerequisite flag.FlagKey references (section 5)
// with no extra I/O — pass nil if flag has no Prerequisites (a
// Prerequisite that cannot be resolved fails safe as unsatisfied, never
// panics on a nil/short slice).
func EvaluateFlag(flags []FlagConfig, flag FlagConfig, configVersion int64, subjectKey string, attributes map[string]string) EvaluationResult {
	return EvaluateFlagTyped(flags, flag, configVersion, subjectKey, stringAttributesToTyped(attributes))
}

func stringAttributesToTyped(attributes map[string]string) map[string]AttributeValue {
	typed := make(map[string]AttributeValue, len(attributes))
	for name, value := range attributes {
		typed[name] = AttributeValue{Type: AttributeTypeString, String: value}
	}

	return typed
}

// EvaluateFlagTyped is EvaluateFlag's typed-attribute counterpart, the
// entry point a caller supplying a number, boolean or list attribute
// (via StringAttr/NumberAttr/BoolAttr/ListAttr, or WithTypedAttributes
// through Client.Evaluate) reaches. Identical evaluation order and
// fail-safe semantics to EvaluateFlag; see its own doc comment.
func EvaluateFlagTyped(flags []FlagConfig, flag FlagConfig, configVersion int64, subjectKey string, attributes map[string]AttributeValue) EvaluationResult {
	return evaluateFlag(flags, flag, configVersion, subjectKey, attributes, 0)
}

// maxPrerequisiteChainDepth is task 1.2's bound decision (testdata/
// README.md: "prerequisite chain length 4"), enforced here as
// evaluation's own defensive fail-safe against a chain deeper than
// authoring should ever have allowed to be persisted — data that somehow
// bypassed the platform's authoring-time cycle/length check degrades to
// "unsatisfied" rather than recursing unboundedly, per
// feature-evaluation's "A Malformed Or Unsupported Construct Fails Safe"
// requirement.
const maxPrerequisiteChainDepth = 4

func lookupFlagConfig(flags []FlagConfig, flagKey string) (FlagConfig, bool) {
	for _, f := range flags {
		if f.FlagKey == flagKey {
			return f, true
		}
	}

	return FlagConfig{}, false
}

// evaluateFlag is EvaluateFlagTyped's recursive core: depth counts how
// many Prerequisite hops resolving flag itself required, so a chain
// longer than maxPrerequisiteChainDepth fails safe instead of recursing
// without bound.
func evaluateFlag(flags []FlagConfig, flag FlagConfig, configVersion int64, subjectKey string, attributes map[string]AttributeValue, depth int) EvaluationResult {
	if !flag.Enabled {
		return defaultResult(flag, configVersion, ReasonDefaultDisabled)
	}

	if depth > maxPrerequisiteChainDepth {
		return defaultResult(flag, configVersion, ReasonPrerequisiteUnsatisfied)
	}

	for _, prerequisite := range flag.Prerequisites {
		prerequisiteFlag, found := lookupFlagConfig(flags, prerequisite.FlagKey)
		if !found {
			return defaultResult(flag, configVersion, ReasonPrerequisiteUnsatisfied)
		}

		result := evaluateFlag(flags, prerequisiteFlag, configVersion, subjectKey, attributes, depth+1)
		if result.VariationKey != prerequisite.RequiredVariationKey {
			return defaultResult(flag, configVersion, ReasonPrerequisiteUnsatisfied)
		}
	}

	for _, target := range flag.IndividualTargets {
		if !target.includes(subjectKey) {
			continue
		}

		if !flag.hasVariation(target.VariationKey) {
			return defaultResult(flag, configVersion, ReasonDefaultFallback)
		}

		return EvaluationResult{
			FlagKey:       flag.FlagKey,
			VariationKey:  target.VariationKey,
			Value:         flag.variationValue(target.VariationKey),
			Reason:        ReasonIndividualTarget,
			ConfigVersion: configVersion,
			TrackExposure: true,
		}
	}

	for _, rule := range flag.Rules {
		matched, _ := rule.matches(attributes)
		if !matched {
			continue
		}

		variationKey, ok := rule.Outcome.resolve(flag.FlagKey, subjectKey)
		if !ok || !flag.hasVariation(variationKey) {
			return defaultResult(flag, configVersion, ReasonDefaultFallback)
		}

		return EvaluationResult{
			FlagKey:       flag.FlagKey,
			VariationKey:  variationKey,
			Value:         flag.variationValue(variationKey),
			Reason:        ReasonRuleMatch,
			ConfigVersion: configVersion,
			TrackExposure: true,
		}
	}

	return defaultResult(flag, configVersion, ReasonDefaultNoRuleMatch)
}

// defaultResult builds the safe-fallback EvaluationResult for flag: its
// default Variation, with reason explaining why the default was served.
// Default-path results never track exposure, since no Rule actually
// targeted the subject.
func defaultResult(flag FlagConfig, configVersion int64, reason EvaluationReason) EvaluationResult {
	return EvaluationResult{
		FlagKey:       flag.FlagKey,
		VariationKey:  flag.DefaultVariation,
		Value:         flag.variationValue(flag.DefaultVariation),
		Reason:        reason,
		ConfigVersion: configVersion,
		TrackExposure: false,
	}
}
