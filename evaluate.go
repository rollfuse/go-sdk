package rollfuse

import "encoding/json"

// Variation is one possible value a FeatureFlag can resolve to.
type Variation struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

// RolloutSplit is one entry of a percentage-based rollout Outcome: the
// share (1-100) of matching subjects that should receive VariationKey.
type RolloutSplit struct {
	VariationKey string `json:"variation_key"`
	Percentage   int    `json:"percentage"`
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
		cumulative += uint32(split.Percentage) * percentageScale
		if bucket < cumulative {
			return split.VariationKey, true
		}
	}

	return "", false
}

// Condition is one attribute-equality check a Rule requires to match.
type Condition struct {
	Attribute string `json:"attribute"`
	Value     string `json:"value"`
}

// Rule is one entry of a FlagConfig's ordered targeting list, evaluated in
// order; the first Rule whose conditions match (or that is unconditional)
// wins.
type Rule struct {
	Conditions []Condition `json:"conditions,omitempty"`
	Outcome    Outcome     `json:"outcome"`
}

// matches reports whether this Rule applies to the given subject
// attributes. A Rule with no Conditions is an unconditional catch-all and
// always matches; a missing attribute never matches a Condition.
func (r Rule) matches(attributes map[string]string) bool {
	for _, condition := range r.Conditions {
		value, ok := attributes[condition.Attribute]
		if !ok || value != condition.Value {
			return false
		}
	}

	return true
}

// FlagConfig is one FeatureFlag's per-Environment view: whether it is
// enabled, its default Variation, its ordered Rules, and the full set of
// Variations rule outcomes may reference.
type FlagConfig struct {
	FlagKey          string      `json:"flag_key"`
	Enabled          bool        `json:"enabled"`
	DefaultVariation string      `json:"default_variation"`
	Variations       []Variation `json:"variations"`
	Rules            []Rule      `json:"rules"`
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
func EvaluateFlag(flag FlagConfig, configVersion int64, subjectKey string, attributes map[string]string) EvaluationResult {
	if !flag.Enabled {
		return defaultResult(flag, configVersion, ReasonDefaultDisabled)
	}

	for _, rule := range flag.Rules {
		if !rule.matches(attributes) {
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
