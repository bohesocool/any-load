package paramoverride

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Context carries request-scoped values available to condition matching that
// are not part of the request body (the body is always consulted first).
type Context struct {
	Model       string
	RequestPath string
	IsStream    bool
}

// Operation is a single override action applied to the request body.
type Operation struct {
	Path       string      `json:"path"`
	Mode       string      `json:"mode"` // set|delete|copy|move|append|prepend
	Value      any         `json:"value"`
	From       string      `json:"from,omitempty"`
	To         string      `json:"to,omitempty"`
	KeepOrigin bool        `json:"keep_origin"`
	Conditions []Condition `json:"conditions,omitempty"`
	Logic      string      `json:"logic,omitempty"` // AND|OR (default OR)
}

// Condition is a single match test gating an Operation. The value at Path
// (read from the body, then the Context) is compared against Value per Mode.
type Condition struct {
	Path           string `json:"path"`
	Mode           string `json:"mode"` // full|prefix|suffix|contains|gt|gte|lt|lte
	Value          any    `json:"value"`
	Invert         bool   `json:"invert"`
	PassMissingKey bool   `json:"pass_missing_key"`
}

// Apply applies a param-override config to the request body bytes.
//
// The config supports two coexisting formats in one document:
//   - A legacy flat map of top-level key→value pairs (e.g. {"temperature":0.7}).
//     Keys are treated literally (a dot in a key is not a nested path); this
//     reproduces the original behavior exactly.
//   - An "operations" array of Operation rules. Paths here use gjson/sjson
//     notation (nested: messages.0.role; negative index: arr.-1).
//
// Legacy keys are applied first (a full unmarshal/set/marshal round trip),
// then operations are applied to the result, so operations win on conflict.
// A failed individual operation (unknown mode, missing copy source, type
// mismatch) is logged and skipped; the request proceeds with the remaining
// changes — matching the proxy's existing pass-through-on-error philosophy.
func Apply(body []byte, config map[string]any, ctx Context) ([]byte, error) {
	if len(config) == 0 || len(body) == 0 {
		return body, nil
	}

	working := body

	// Legacy flat map first: literal top-level keys, exact prior behavior.
	if legacy := buildLegacy(config); len(legacy) > 0 {
		var m map[string]any
		if err := json.Unmarshal(working, &m); err != nil {
			logrus.Warnf("paramoverride: failed to unmarshal body for legacy override, passing through: %v", err)
			return body, nil
		}
		for k, v := range legacy {
			m[k] = v
		}
		b, err := json.Marshal(m)
		if err != nil {
			return body, fmt.Errorf("remarshal legacy overrides: %w", err)
		}
		working = b
	}

	// Operations second.
	if raw, ok := config["operations"]; ok {
		for i, op := range parseOperations(raw) {
			if !conditionsPass(working, op.Conditions, op.Logic, ctx) {
				continue
			}
			b, err := applyOp(working, op)
			if err != nil {
				logrus.Warnf("paramoverride: operation #%d (mode=%s path=%q) skipped: %v", i, op.Mode, op.Path, err)
				continue
			}
			working = b
		}
	}

	return working, nil
}

// buildLegacy returns a copy of config without the "operations" key, i.e. the
// legacy flat-map portion.
func buildLegacy(config map[string]any) map[string]any {
	legacy := make(map[string]any, len(config))
	for k, v := range config {
		if k == "operations" {
			continue
		}
		legacy[k] = v
	}
	return legacy
}

// parseOperations decodes the "operations" value into []Operation, tolerating
// individual malformed entries (they are logged and dropped).
func parseOperations(raw any) []Operation {
	arr, ok := raw.([]any)
	if !ok {
		logrus.Warnf("paramoverride: \"operations\" is not an array, ignoring")
		return nil
	}
	ops := make([]Operation, 0, len(arr))
	for i, item := range arr {
		b, err := json.Marshal(item)
		if err != nil {
			logrus.Warnf("paramoverride: skipping operation #%d: marshal: %v", i, err)
			continue
		}
		var op Operation
		if err := json.Unmarshal(b, &op); err != nil {
			logrus.Warnf("paramoverride: skipping operation #%d: %v", i, err)
			continue
		}
		ops = append(ops, op)
	}
	return ops
}

// applyOp executes a single operation against the body bytes.
func applyOp(body []byte, op Operation) ([]byte, error) {
	// Resolve negative array indices (-1 = last) to concrete indices up front,
	// before any mutation, so reads and writes use consistent paths. gjson/sjson
	// do not natively support negative indices.
	path := resolvePath(body, op.Path)
	from := resolvePath(body, op.From)
	to := resolvePath(body, op.To)
	switch strings.ToLower(op.Mode) {
	case "set":
		if op.KeepOrigin && gjson.GetBytes(body, path).Exists() {
			return body, nil
		}
		return sjson.SetBytes(body, path, op.Value)
	case "delete":
		return sjson.DeleteBytes(body, path)
	case "copy":
		src := gjson.GetBytes(body, from)
		if !src.Exists() {
			return body, fmt.Errorf("copy: source path %q not found", op.From)
		}
		return sjson.SetBytes(body, to, src.Value())
	case "move":
		src := gjson.GetBytes(body, from)
		if !src.Exists() {
			return body, fmt.Errorf("move: source path %q not found", op.From)
		}
		b, err := sjson.SetBytes(body, to, src.Value())
		if err != nil {
			return b, err
		}
		return sjson.DeleteBytes(b, from)
	case "append":
		return appendValue(body, path, op.Value, false)
	case "prepend":
		return appendValue(body, path, op.Value, true)
	default:
		return body, fmt.Errorf("unknown mode %q", op.Mode)
	}
}

// resolvePath converts negative array-index segments (e.g. "messages.-1") to
// concrete indices against the current body. Paths without negative indices
// are returned untouched (preserving gjson escaping). Resolution is naive
// about escaped dots, which is acceptable for the core subset.
func resolvePath(body []byte, path string) string {
	segs := strings.Split(path, ".")
	hasNeg := false
	for _, s := range segs {
		if isNegIndex(s) {
			hasNeg = true
			break
		}
	}
	if !hasNeg {
		return path
	}
	for i, s := range segs {
		if !isNegIndex(s) {
			continue
		}
		arr := gjson.GetBytes(body, strings.Join(segs[:i], "."))
		if arr.IsArray() {
			n := len(arr.Array())
			neg, _ := strconv.Atoi(s)
			idx := n + neg // neg is negative, e.g. -1 → n-1
			if idx >= 0 && idx < n {
				segs[i] = strconv.Itoa(idx)
			}
		}
	}
	return strings.Join(segs, ".")
}

func isNegIndex(s string) bool {
	if len(s) < 2 || s[0] != '-' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// appendValue appends (or prepends) a value to the element at path. Arrays are
// concatenated (value may be an array or a scalar); strings are concatenated;
// a missing path is created as a single-element array.
func appendValue(body []byte, path string, value any, prepend bool) ([]byte, error) {
	cur := gjson.GetBytes(body, path)
	if !cur.Exists() {
		return sjson.SetBytes(body, path, []any{value})
	}
	if cur.IsArray() {
		var arr []any
		cur.ForEach(func(_, item gjson.Result) bool {
			arr = append(arr, item.Value())
			return true
		})
		valArr, ok := value.([]any)
		if !ok {
			valArr = []any{value}
		}
		merged := make([]any, 0, len(arr)+len(valArr))
		if prepend {
			merged = append(merged, valArr...)
			merged = append(merged, arr...)
		} else {
			merged = append(merged, arr...)
			merged = append(merged, valArr...)
		}
		return sjson.SetBytes(body, path, merged)
	}
	if cur.Type == gjson.String {
		valStr, ok := value.(string)
		if !ok {
			return body, fmt.Errorf("append: target at %q is a string but value is not", path)
		}
		curStr := cur.String()
		if prepend {
			return sjson.SetBytes(body, path, valStr+curStr)
		}
		return sjson.SetBytes(body, path, curStr+valStr)
	}
	return body, fmt.Errorf("append: cannot append to %s at %q", cur.Type, path)
}

// conditionsPass evaluates a slice of conditions under the given logic.
// Empty conditions pass. Logic "AND" requires all; anything else (including
// empty) is "OR" and passes if any condition passes.
func conditionsPass(body []byte, conds []Condition, logic string, ctx Context) bool {
	if len(conds) == 0 {
		return true
	}
	isAnd := strings.EqualFold(logic, "AND")
	for _, c := range conds {
		pass := conditionPass(body, c, ctx)
		if isAnd {
			if !pass {
				return false
			}
		} else if pass {
			return true
		}
	}
	return isAnd // AND: all passed; OR: none passed
}

// conditionPass evaluates a single condition: resolve the actual value (body
// first, then Context), compare per Mode, apply Invert. A missing key passes
// iff PassMissingKey is set.
func conditionPass(body []byte, c Condition, ctx Context) bool {
	actual, exists := resolveValue(body, c.Path, ctx)
	if !exists {
		return c.PassMissingKey
	}
	pass := compare(c.Mode, actual, c.Value)
	if c.Invert {
		return !pass
	}
	return pass
}

// resolveValue reads the value at path from the body via gjson; if absent it
// falls back to well-known Context fields.
func resolveValue(body []byte, path string, ctx Context) (any, bool) {
	r := gjson.GetBytes(body, path)
	if r.Exists() {
		return r.Value(), true
	}
	switch path {
	case "model":
		if ctx.Model != "" {
			return ctx.Model, true
		}
	case "request_path":
		if ctx.RequestPath != "" {
			return ctx.RequestPath, true
		}
	case "is_stream":
		return ctx.IsStream, true
	}
	return nil, false
}

func compare(mode string, actual, expected any) bool {
	switch strings.ToLower(mode) {
	case "full":
		return equalValue(actual, expected)
	case "prefix":
		return strings.HasPrefix(toStr(actual), toStr(expected))
	case "suffix":
		return strings.HasSuffix(toStr(actual), toStr(expected))
	case "contains":
		return strings.Contains(toStr(actual), toStr(expected))
	case "gt", "gte", "lt", "lte":
		a, okA := toFloat(actual)
		e, okE := toFloat(expected)
		if !okA || !okE {
			return false
		}
		switch strings.ToLower(mode) {
		case "gt":
			return a > e
		case "gte":
			return a >= e
		case "lt":
			return a < e
		case "lte":
			return a <= e
		}
	}
	return false
}

// equalValue compares two values. Numbers are compared as float64 so that
// 0.7 == 0.7 across JSON round trips; everything else is compared by string
// representation (booleans, nil, strings).
func equalValue(actual, expected any) bool {
	if af, ok := toFloat(actual); ok {
		if ef, ok := toFloat(expected); ok {
			return af == ef
		}
		return false
	}
	return toStr(actual) == toStr(expected)
}

func toStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}
