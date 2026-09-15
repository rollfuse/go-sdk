package rollfuse

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// AttributeType is the typed shape a targeting attribute or a Clause's
// literal value carries, per expand-targeting-model's "An Attribute
// Carries A Typed Value" requirement. Mirrors
// apps/api/internal/evaluation/domain/clause.go's own AttributeType
// field-for-field (independent implementation, no shared code).
type AttributeType string

const (
	AttributeTypeString  AttributeType = "string"
	AttributeTypeNumber  AttributeType = "number"
	AttributeTypeBoolean AttributeType = "boolean"
	AttributeTypeList    AttributeType = "list"
)

// AttributeValue is one subject-supplied attribute, or a Clause's own
// literal, carrying exactly the field(s) its Type names. A list is
// always a list of strings, per the spec's own "list of strings"
// wording.
type AttributeValue struct {
	Type    AttributeType
	String  string
	Number  float64
	Boolean bool
	List    []string
}

// StringAttr, NumberAttr, BoolAttr and ListAttr construct a typed
// AttributeValue for WithTypedAttributes, so a caller never has to build
// the struct's Type/String/Number/Boolean/List fields by hand.
func StringAttr(v string) AttributeValue { return AttributeValue{Type: AttributeTypeString, String: v} }

func NumberAttr(v float64) AttributeValue {
	return AttributeValue{Type: AttributeTypeNumber, Number: v}
}

func BoolAttr(v bool) AttributeValue     { return AttributeValue{Type: AttributeTypeBoolean, Boolean: v} }
func ListAttr(v []string) AttributeValue { return AttributeValue{Type: AttributeTypeList, List: v} }

// ClauseOp is one of the enumerated, closed set of operators
// environment-flag-targeting's "A Clause Supports Operators Beyond
// Equality" requirement names. Never grows informally, per design.md's
// "The operator set is enumerated and closed" decision.
type ClauseOp string

const (
	OpEqual          ClauseOp = "eq"
	OpNotEqual       ClauseOp = "neq"
	OpGreaterThan    ClauseOp = "gt"
	OpGreaterOrEqual ClauseOp = "gte"
	OpLessThan       ClauseOp = "lt"
	OpLessOrEqual    ClauseOp = "lte"
	OpIn             ClauseOp = "in"
	OpPrefix         ClauseOp = "prefix"
	OpSuffix         ClauseOp = "suffix"
	OpSubstring      ClauseOp = "substring"
	OpRegex          ClauseOp = "regex"
	OpSemverGT       ClauseOp = "semver_gt"
	OpSemverGTE      ClauseOp = "semver_gte"
	OpSemverLT       ClauseOp = "semver_lt"
	OpSemverLTE      ClauseOp = "semver_lte"
	OpPresent        ClauseOp = "present"
)

// Clause is one leaf targeting condition against a single typed
// attribute. Composition (AND/OR/negation, section 5) and
// segment-membership clauses (section 7) are not modeled here yet — a
// Rule whose wire shape contains one is skipped as non-matching rather
// than failing evaluation, per feature-evaluation's "A Malformed Or
// Unsupported Construct Fails Safe" requirement (see Rule.matches).
//
// Candidates is only meaningful for OpIn, always []string regardless of
// the attribute's own scalar type (compared as string form), matching
// the conformance fixture's own shape.
type Clause struct {
	Attribute  string
	Type       AttributeType
	Op         ClauseOp
	Value      AttributeValue
	Candidates []string
}

// wireClause is Clause's JSON wire shape: a Clause's typed Value can't be
// unmarshaled generically (a bare JSON value doesn't say whether "true"
// means the string "true" or the boolean true), so UnmarshalJSON decodes
// into this first and resolves the value against the declared Type. For
// OpIn, "value" itself carries the candidate array (the fixture's own
// shape — there is no separate "candidates" field on the wire).
type wireClause struct {
	Attribute string          `json:"attribute"`
	Type      AttributeType   `json:"type"`
	Op        ClauseOp        `json:"op"`
	Value     json.RawMessage `json:"value"`
}

// UnmarshalJSON implements Clause's typed decode from the wire shape
// above. An empty/absent "op" defaults to OpEqual and an empty/absent
// "type" defaults to AttributeTypeString, so a config published before
// expand-targeting-model's contract change (bare {"attribute","value"}
// equality) still decodes correctly — additive, per design.md's own
// "existing configurations remain expressible" goal.
func (c *Clause) UnmarshalJSON(data []byte) error {
	var w wireClause
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}

	c.Attribute = w.Attribute
	c.Type = w.Type

	if c.Type == "" {
		c.Type = AttributeTypeString
	}

	c.Op = w.Op
	if c.Op == "" {
		c.Op = OpEqual
	}

	if c.Op == OpPresent || len(w.Value) == 0 {
		return nil
	}

	if c.Op == OpIn {
		if err := json.Unmarshal(w.Value, &c.Candidates); err != nil {
			// A malformed "in" value: leave Candidates empty, which never
			// matches anything — fail-safe, not a decode error.
			return nil
		}

		return nil
	}

	value, err := decodeAttributeValue(c.Type, w.Value)
	if err != nil {
		// A value this client cannot decode against its declared type is
		// treated the same as any other malformed construct: the clause
		// itself carries no usable literal, so Match below will never
		// match it, rather than failing the whole evaluation.
		return nil
	}

	c.Value = value

	return nil
}

func decodeAttributeValue(typ AttributeType, raw json.RawMessage) (AttributeValue, error) {
	v := AttributeValue{Type: typ}

	var err error

	switch typ {
	case AttributeTypeNumber:
		err = json.Unmarshal(raw, &v.Number)
	case AttributeTypeBoolean:
		err = json.Unmarshal(raw, &v.Boolean)
	case AttributeTypeList:
		err = json.Unmarshal(raw, &v.List)
	default:
		err = json.Unmarshal(raw, &v.String)
	}

	return v, err
}

// Match reports whether attributes satisfies this Clause. It never
// panics: an absent attribute, a type mismatch, or an invalid literal
// (a "gt" clause against a non-numeric attribute, an unparsable semver)
// all evaluate false — the diagnostic string explains why, for a caller
// that wants to surface it, but never changes the boolean outcome.
func (c Clause) Match(attributes map[string]AttributeValue) (matched bool, diagnostic string) {
	value, present := attributes[c.Attribute]

	if c.Op == OpPresent {
		return present, ""
	}

	if !present {
		return false, ""
	}

	if value.Type != c.Type {
		return false, "type mismatch on attribute \"" + c.Attribute + "\""
	}

	switch c.Op {
	case OpEqual:
		return equalAttributeValue(value, c.Value), ""
	case OpNotEqual:
		return !equalAttributeValue(value, c.Value), ""
	case OpGreaterThan, OpGreaterOrEqual, OpLessThan, OpLessOrEqual:
		return matchOrdered(c.Op, value, c.Value)
	case OpIn:
		return matchIn(c.Type, value, c.Candidates), ""
	case OpPrefix:
		return value.Type == AttributeTypeString && strings.HasPrefix(value.String, c.Value.String), ""
	case OpSuffix:
		return value.Type == AttributeTypeString && strings.HasSuffix(value.String, c.Value.String), ""
	case OpSubstring:
		return value.Type == AttributeTypeString && strings.Contains(value.String, c.Value.String), ""
	case OpRegex:
		return matchRegex(value.String, c.Value.String)
	case OpSemverGT, OpSemverGTE, OpSemverLT, OpSemverLTE:
		return matchSemver(c.Op, value.String, c.Value.String)
	default:
		return false, "unrecognized operator \"" + string(c.Op) + "\" on attribute \"" + c.Attribute + "\""
	}
}

func equalAttributeValue(a, b AttributeValue) bool {
	switch a.Type {
	case AttributeTypeString:
		return a.String == b.String
	case AttributeTypeNumber:
		return a.Number == b.Number
	case AttributeTypeBoolean:
		return a.Boolean == b.Boolean
	case AttributeTypeList:
		if len(a.List) != len(b.List) {
			return false
		}

		for i := range a.List {
			if a.List[i] != b.List[i] {
				return false
			}
		}

		return true
	default:
		return false
	}
}

func matchOrdered(op ClauseOp, attribute, literal AttributeValue) (bool, string) {
	if attribute.Type != AttributeTypeNumber {
		return false, "ordered comparison requires a number attribute"
	}

	switch op {
	case OpGreaterThan:
		return attribute.Number > literal.Number, ""
	case OpGreaterOrEqual:
		return attribute.Number >= literal.Number, ""
	case OpLessThan:
		return attribute.Number < literal.Number, ""
	case OpLessOrEqual:
		return attribute.Number <= literal.Number, ""
	default:
		return false, ""
	}
}

func matchIn(clauseType AttributeType, attribute AttributeValue, candidates []string) bool {
	if clauseType == AttributeTypeList {
		if attribute.Type != AttributeTypeList {
			return false
		}

		for _, have := range attribute.List {
			for _, want := range candidates {
				if have == want {
					return true
				}
			}
		}

		return false
	}

	attributeAsString, ok := scalarAsString(attribute)
	if !ok {
		return false
	}

	for _, want := range candidates {
		if attributeAsString == want {
			return true
		}
	}

	return false
}

func scalarAsString(v AttributeValue) (string, bool) {
	switch v.Type {
	case AttributeTypeString:
		return v.String, true
	case AttributeTypeNumber:
		return strconv.FormatFloat(v.Number, 'g', -1, 64), true
	case AttributeTypeBoolean:
		return strconv.FormatBool(v.Boolean), true
	default:
		return "", false
	}
}

// matchRegex implements the bounded regex operator. Go's regexp package
// is RE2-native: it structurally cannot express backreferences or
// lookaround, so compiling the pattern IS the bounds check, matching the
// platform's own reasoning (testdata/README.md's task 1.2 decision).
func matchRegex(subject, pattern string) (bool, string) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false, "regex clause has an invalid or unbounded pattern: " + err.Error()
	}

	return re.MatchString(subject), ""
}

// ValidateBoundedRegex reports whether pattern is expressible in the
// bounded RE2 subset. Exported so a caller authoring configuration
// locally (or validating one before use) can check a pattern without
// constructing a full Clause.
func ValidateBoundedRegex(pattern string) error {
	if _, err := regexp.Compile(pattern); err != nil {
		return ErrRegexPatternUnbounded
	}

	return nil
}

// MaxClauseNestingDepth is expand-targeting-model task 1.2's bound
// decision (testdata/README.md: "nesting depth 5"), enforced here as a
// defensive fail-safe against a ClauseTree deeper than authoring should
// ever have allowed to be persisted — mirrors
// apps/api/internal/evaluation/domain/clause.go's own
// MaxClauseNestingDepth (independent implementation, same bound).
const MaxClauseNestingDepth = 5

// ClauseTreeOp discriminates a ClauseTree node's shape.
type ClauseTreeOp string

const (
	ClauseTreeLeaf ClauseTreeOp = "leaf"
	ClauseTreeAnd  ClauseTreeOp = "and"
	ClauseTreeOr   ClauseTreeOp = "or"
	ClauseTreeNot  ClauseTreeOp = "not"
)

// ClauseTree is a rule condition: a single leaf Clause, or a group of
// child ClauseTrees combined with AND/OR, or a negation of exactly one
// child — environment-flag-targeting's "Clauses Compose With AND, OR And
// Negation" requirement. Groups nest to MaxClauseNestingDepth.
type ClauseTree struct {
	Op       ClauseTreeOp
	Leaf     Clause
	Children []ClauseTree
}

// wireClauseTree decodes the fixture/wire clause-tree shape (testdata/
// README.md): a leaf clause has one of the enumerated ClauseOp values
// directly on "op"; "and"/"or" carry "clauses"; "not" carries a single
// "clause"; "segment" (section 7, not implemented by this package yet)
// decodes to an always-non-matching leaf rather than failing the whole
// unmarshal, per feature-evaluation's "A Malformed Or Unsupported
// Construct Fails Safe" requirement.
type wireClauseTree struct {
	Op         string            `json:"op"`
	Clauses    []json.RawMessage `json:"clauses,omitempty"`
	Clause     json.RawMessage   `json:"clause,omitempty"`
	SegmentKey string            `json:"segment_key,omitempty"`
}

// UnmarshalJSON implements ClauseTree's decode from the wire shape above.
func (t *ClauseTree) UnmarshalJSON(data []byte) error {
	var w wireClauseTree
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}

	switch ClauseTreeOp(w.Op) {
	case ClauseTreeAnd, ClauseTreeOr:
		t.Op = ClauseTreeOp(w.Op)
		t.Children = make([]ClauseTree, 0, len(w.Clauses))

		for _, raw := range w.Clauses {
			var child ClauseTree
			if err := json.Unmarshal(raw, &child); err != nil {
				return err
			}

			t.Children = append(t.Children, child)
		}

		return nil
	case ClauseTreeNot:
		t.Op = ClauseTreeNot

		var child ClauseTree
		if len(w.Clause) > 0 && string(w.Clause) != "null" {
			if err := json.Unmarshal(w.Clause, &child); err != nil {
				return err
			}
		}

		t.Children = []ClauseTree{child}

		return nil
	case "segment":
		// Segment-membership clauses are reusable-segments/section 7's
		// job. Until this package implements them, a segment clause
		// fails safe as a leaf that never matches, rather than aborting
		// decode of the whole rule.
		t.Op = ClauseTreeLeaf
		t.Leaf = Clause{Op: "unsupported_segment_clause"}

		return nil
	default:
		// A leaf: decode the whole payload as a Clause directly (Clause
		// already knows how to read {"op","attribute","type","value"}).
		t.Op = ClauseTreeLeaf

		return json.Unmarshal(data, &t.Leaf)
	}
}

// Match reports whether attributes satisfies this ClauseTree, recursively.
// Negating a "present" leaf naturally implements the spec's "explicit
// negated presence test" with no special-casing: OpPresent returns false
// for an absent attribute, and Not inverts that to true.
//
// depth is the caller's own nesting level (0 for the tree's root).
// Exceeding MaxClauseNestingDepth fails safe (non-matching, no
// diagnostic — a defensive bound, not an operator-authored condition).
func (t ClauseTree) Match(attributes map[string]AttributeValue, depth int) (matched bool, diagnostic string) {
	if depth > MaxClauseNestingDepth {
		return false, ""
	}

	switch t.Op {
	case ClauseTreeLeaf:
		return t.Leaf.Match(attributes)
	case ClauseTreeNot:
		if len(t.Children) != 1 {
			return false, ""
		}

		childMatched, childDiagnostic := t.Children[0].Match(attributes, depth+1)

		return !childMatched, childDiagnostic
	case ClauseTreeAnd:
		var diagnostic string

		for _, child := range t.Children {
			childMatched, childDiagnostic := child.Match(attributes, depth+1)
			if childDiagnostic != "" {
				diagnostic = childDiagnostic
			}

			if !childMatched {
				return false, diagnostic
			}
		}

		return true, diagnostic
	case ClauseTreeOr:
		var diagnostic string

		for _, child := range t.Children {
			childMatched, childDiagnostic := child.Match(attributes, depth+1)
			if childDiagnostic != "" {
				diagnostic = childDiagnostic
			}

			if childMatched {
				return true, diagnostic
			}
		}

		return false, diagnostic
	default:
		return false, ""
	}
}
