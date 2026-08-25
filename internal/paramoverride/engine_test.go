package paramoverride

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

// j unmarshals a JSON string to map[string]any for legacy config construction.
func j(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("j: %v", err)
	}
	return m
}

// bodyAfter applies config to body and returns the result marshaled (sorted)
// so comparisons are stable regardless of map iteration order.
func bodyAfter(t *testing.T, body string, config map[string]any, ctx Context) string {
	t.Helper()
	out, err := Apply([]byte(body), config, ctx)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return string(out)
}

func TestApplyEmpty(t *testing.T) {
	if out, err := Apply(nil, map[string]any{"x": 1}, Context{}); err != nil || string(out) != "" {
		t.Fatalf("nil body should pass through, got %q err=%v", string(out), err)
	}
	if out, err := Apply([]byte(`{"a":1}`), map[string]any{}, Context{}); err != nil || string(out) != `{"a":1}` {
		t.Fatalf("empty config should pass through, got %q err=%v", string(out), err)
	}
}

// --- Legacy flat map (backward compatibility) ---

func TestLegacyFlat(t *testing.T) {
	out := bodyAfter(t, `{"model":"gpt-4","temperature":0.2}`, map[string]any{"temperature": 0.7}, Context{})
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatal(err)
	}
	if m["temperature"] != 0.7 {
		t.Fatalf("temperature = %v, want 0.7", m["temperature"])
	}
	if m["model"] != "gpt-4" {
		t.Fatalf("model = %v, want gpt-4", m["model"])
	}
}

// Legacy keys are literal: a dot does not become a nested path.
func TestLegacyLiteralKey(t *testing.T) {
	out := bodyAfter(t, `{}`, map[string]any{"a.b": 1}, Context{})
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatal(err)
	}
	if m["a.b"] != float64(1) {
		t.Fatalf(`literal "a.b" = %v, want 1`, m["a.b"])
	}
}

func TestLegacyInvalidJSON(t *testing.T) {
	out, err := Apply([]byte(`not json`), map[string]any{"x": 1}, Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != `not json` {
		t.Fatalf("invalid body should pass through, got %q", string(out))
	}
}

// --- Operations: set ---

func TestOpSet(t *testing.T) {
	cfg := map[string]any{"operations": []any{
		map[string]any{"path": "temperature", "mode": "set", "value": 0.9},
	}}
	out := bodyAfter(t, `{"model":"gpt-4"}`, cfg, Context{})
	if gjsonGet(out, "temperature") != "0.9" {
		t.Fatalf("temperature = %s, want 0.9", gjsonGet(out, "temperature"))
	}
}

func TestOpSetNested(t *testing.T) {
	cfg := map[string]any{"operations": []any{
		map[string]any{"path": "messages.0.role", "mode": "set", "value": "system"},
	}}
	out := bodyAfter(t, `{"messages":[{"role":"user","content":"hi"}]}`, cfg, Context{})
	if gjsonGet(out, "messages.0.role") != "system" {
		t.Fatalf("messages.0.role = %s, want system", gjsonGet(out, "messages.0.role"))
	}
}

func TestOpSetKeepOrigin(t *testing.T) {
	cfg := map[string]any{"operations": []any{
		map[string]any{"path": "temperature", "mode": "set", "value": 0.9, "keep_origin": true},
	}}
	// Existing value preserved.
	out := bodyAfter(t, `{"temperature":0.3}`, cfg, Context{})
	if gjsonGet(out, "temperature") != "0.3" {
		t.Fatalf("keep_origin should preserve 0.3, got %s", gjsonGet(out, "temperature"))
	}
	// Missing value set.
	out = bodyAfter(t, `{}`, cfg, Context{})
	if gjsonGet(out, "temperature") != "0.9" {
		t.Fatalf("keep_origin missing should set 0.9, got %s", gjsonGet(out, "temperature"))
	}
}

// --- Operations: delete ---

func TestOpDelete(t *testing.T) {
	cfg := map[string]any{"operations": []any{
		map[string]any{"path": "temperature", "mode": "delete"},
	}}
	out := bodyAfter(t, `{"model":"gpt-4","temperature":0.3}`, cfg, Context{})
	if gjsonExists(out, "temperature") {
		t.Fatalf("temperature should be deleted")
	}
	if gjsonGet(out, "model") != "gpt-4" {
		t.Fatalf("model should remain")
	}
}

func TestOpDeleteNested(t *testing.T) {
	cfg := map[string]any{"operations": []any{
		map[string]any{"path": "messages.1", "mode": "delete"},
	}}
	out := bodyAfter(t, `{"messages":[{"a":1},{"b":2},{"c":3}]}`, cfg, Context{})
	if gjsonGet(out, "messages.#") != "2" {
		t.Fatalf("array length = %s, want 2", gjsonGet(out, "messages.#"))
	}
	if gjsonGet(out, "messages.1.c") != "3" {
		t.Fatalf("remaining element = %s, want c=3", gjsonGet(out, "messages.1.c"))
	}
}

// --- Operations: copy / move ---

func TestOpCopy(t *testing.T) {
	cfg := map[string]any{"operations": []any{
		map[string]any{"mode": "copy", "from": "model", "to": "metadata.model"},
	}}
	out := bodyAfter(t, `{"model":"gpt-4"}`, cfg, Context{})
	if gjsonGet(out, "metadata.model") != "gpt-4" {
		t.Fatalf("metadata.model = %s, want gpt-4", gjsonGet(out, "metadata.model"))
	}
	if gjsonGet(out, "model") != "gpt-4" {
		t.Fatalf("source should still exist after copy")
	}
}

func TestOpCopyMissingSource(t *testing.T) {
	cfg := map[string]any{"operations": []any{
		map[string]any{"mode": "copy", "from": "nope", "to": "x"},
	}}
	out, err := Apply([]byte(`{"model":"gpt-4"}`), cfg, Context{})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Operation skipped; body unchanged.
	if gjsonExists(string(out), "x") {
		t.Fatalf("copy from missing source should be skipped")
	}
}

func TestOpMove(t *testing.T) {
	cfg := map[string]any{"operations": []any{
		map[string]any{"mode": "move", "from": "model", "to": "metadata.model"},
	}}
	out := bodyAfter(t, `{"model":"gpt-4"}`, cfg, Context{})
	if gjsonGet(out, "metadata.model") != "gpt-4" {
		t.Fatalf("metadata.model = %s, want gpt-4", gjsonGet(out, "metadata.model"))
	}
	if gjsonExists(string(out), "model") {
		t.Fatalf("source should be removed after move")
	}
}

// --- Operations: append / prepend ---

func TestOpAppendScalarToArray(t *testing.T) {
	cfg := map[string]any{"operations": []any{
		map[string]any{"mode": "append", "path": "tags", "value": "new"},
	}}
	out := bodyAfter(t, `{"tags":["a","b"]}`, cfg, Context{})
	if gjsonGet(out, "tags.#") != "3" || gjsonGet(out, "tags.2") != "new" {
		t.Fatalf("append scalar: %s", gjsonGet(out, "tags"))
	}
}

func TestOpAppendArrayToArray(t *testing.T) {
	cfg := map[string]any{"operations": []any{
		map[string]any{"mode": "append", "path": "tags", "value": []any{"x", "y"}},
	}}
	out := bodyAfter(t, `{"tags":["a","b"]}`, cfg, Context{})
	if gjsonGet(out, "tags.#") != "4" || gjsonGet(out, "tags.3") != "y" {
		t.Fatalf("append array: %s", gjsonGet(out, "tags"))
	}
}

func TestOpAppendString(t *testing.T) {
	cfg := map[string]any{"operations": []any{
		map[string]any{"mode": "append", "path": "prompt", "value": " world"},
	}}
	out := bodyAfter(t, `{"prompt":"hello"}`, cfg, Context{})
	if gjsonGet(out, "prompt") != "hello world" {
		t.Fatalf("append string: %s", gjsonGet(out, "prompt"))
	}
}

func TestOpAppendMissingCreatesArray(t *testing.T) {
	cfg := map[string]any{"operations": []any{
		map[string]any{"mode": "append", "path": "tags", "value": "first"},
	}}
	out := bodyAfter(t, `{}`, cfg, Context{})
	if gjsonGet(out, "tags.0") != "first" {
		t.Fatalf("append to missing should create array: %s", gjsonGet(out, "tags"))
	}
}

func TestOpPrepend(t *testing.T) {
	cfg := map[string]any{"operations": []any{
		map[string]any{"mode": "prepend", "path": "tags", "value": "z"},
	}}
	out := bodyAfter(t, `{"tags":["a","b"]}`, cfg, Context{})
	if gjsonGet(out, "tags.0") != "z" || gjsonGet(out, "tags.#") != "3" {
		t.Fatalf("prepend: %s", gjsonGet(out, "tags"))
	}
}

// --- Negative index (gjson native support check) ---

func TestNegativeIndex(t *testing.T) {
	cfg := map[string]any{"operations": []any{
		map[string]any{"path": "messages.-1.role", "mode": "set", "value": "assistant"},
	}}
	out := bodyAfter(t, `{"messages":[{"role":"user"},{"role":"user"}]}`, cfg, Context{})
	if gjsonGet(out, "messages.1.role") != "assistant" {
		t.Fatalf("negative index: last element role = %s, want assistant", gjsonGet(out, "messages.1.role"))
	}
}

// --- Conditions ---

func TestConditionPrefixBody(t *testing.T) {
	cfg := map[string]any{"operations": []any{
		map[string]any{
			"path": "temperature", "mode": "set", "value": 0.7,
			"conditions": []any{
				map[string]any{"path": "model", "mode": "prefix", "value": "gpt-"},
			},
		},
	}}
	// Matches: applied.
	out := bodyAfter(t, `{"model":"gpt-4"}`, cfg, Context{})
	if gjsonGet(out, "temperature") != "0.7" {
		t.Fatalf("matching condition should apply, got %s", gjsonGet(out, "temperature"))
	}
	// No match: skipped.
	out = bodyAfter(t, `{"model":"claude-3"}`, cfg, Context{})
	if gjsonExists(out, "temperature") {
		t.Fatalf("non-matching condition should skip, got %s", gjsonGet(out, "temperature"))
	}
}

func TestConditionContextFallback(t *testing.T) {
	// "model" not in body; falls back to Context.Model.
	cfg := map[string]any{"operations": []any{
		map[string]any{
			"path": "temperature", "mode": "set", "value": 0.7,
			"conditions": []any{
				map[string]any{"path": "model", "mode": "full", "value": "gpt-4"},
			},
		},
	}}
	out := bodyAfter(t, `{"prompt":"hi"}`, cfg, Context{Model: "gpt-4"})
	if gjsonGet(out, "temperature") != "0.7" {
		t.Fatalf("context model match should apply, got %s", gjsonGet(out, "temperature"))
	}
	out = bodyAfter(t, `{"prompt":"hi"}`, cfg, Context{Model: "claude"})
	if gjsonExists(out, "temperature") {
		t.Fatalf("context model mismatch should skip")
	}
}

func TestConditionLogicAND(t *testing.T) {
	cfg := map[string]any{"operations": []any{
		map[string]any{
			"path": "x", "mode": "set", "value": 1, "logic": "AND",
			"conditions": []any{
				map[string]any{"path": "model", "mode": "prefix", "value": "gpt-"},
				map[string]any{"path": "request_path", "mode": "prefix", "value": "/v1/chat"},
			},
		},
	}}
	// Both pass.
	out := bodyAfter(t, `{"model":"gpt-4"}`, cfg, Context{RequestPath: "/v1/chat/completions"})
	if !gjsonExists(out, "x") {
		t.Fatalf("AND both-pass should apply")
	}
	// Only one passes.
	out = bodyAfter(t, `{"model":"gpt-4"}`, cfg, Context{RequestPath: "/v1/messages"})
	if gjsonExists(out, "x") {
		t.Fatalf("AND one-fail should skip")
	}
}

func TestConditionInvert(t *testing.T) {
	cfg := map[string]any{"operations": []any{
		map[string]any{
			"path": "x", "mode": "set", "value": 1,
			"conditions": []any{
				map[string]any{"path": "model", "mode": "prefix", "value": "claude-", "invert": true},
			},
		},
	}}
	// model=gpt-4 → prefix claude- is false → invert → true → apply.
	out := bodyAfter(t, `{"model":"gpt-4"}`, cfg, Context{})
	if !gjsonExists(out, "x") {
		t.Fatalf("invert should make non-matching condition pass")
	}
	// model=claude-3 → prefix true → invert → false → skip.
	out = bodyAfter(t, `{"model":"claude-3"}`, cfg, Context{})
	if gjsonExists(out, "x") {
		t.Fatalf("invert should make matching condition fail")
	}
}

func TestConditionPassMissingKey(t *testing.T) {
	cfg := map[string]any{"operations": []any{
		map[string]any{
			"path": "x", "mode": "set", "value": 1,
			"conditions": []any{
				map[string]any{"path": "absent", "mode": "full", "value": "y", "pass_missing_key": true},
			},
		},
	}}
	out := bodyAfter(t, `{}`, cfg, Context{})
	if !gjsonExists(out, "x") {
		t.Fatalf("pass_missing_key should pass on missing path")
	}
}

func TestConditionNumericCompare(t *testing.T) {
	cfg := map[string]any{"operations": []any{
		map[string]any{
			"path": "x", "mode": "set", "value": 1,
			"conditions": []any{
				map[string]any{"path": "max_tokens", "mode": "gte", "value": 1000},
			},
		},
	}}
	out := bodyAfter(t, `{"max_tokens":2000}`, cfg, Context{})
	if !gjsonExists(out, "x") {
		t.Fatalf("gte 1000 with 2000 should pass")
	}
	out = bodyAfter(t, `{"max_tokens":500}`, cfg, Context{})
	if gjsonExists(out, "x") {
		t.Fatalf("gte 1000 with 500 should fail")
	}
}

// --- Mixed legacy + operations; operations win ---

func TestMixedLegacyAndOperations(t *testing.T) {
	cfg := map[string]any{
		"temperature": 0.3, // legacy first
		"operations": []any{
			map[string]any{"path": "temperature", "mode": "set", "value": 0.9},
		},
	}
	out := bodyAfter(t, `{"model":"gpt-4"}`, cfg, Context{})
	if gjsonGet(out, "temperature") != "0.9" {
		t.Fatalf("operations should win over legacy, got %s", gjsonGet(out, "temperature"))
	}
}

// --- Forgiving: unknown mode / malformed entries skipped ---

func TestUnknownModeSkipped(t *testing.T) {
	cfg := map[string]any{"operations": []any{
		map[string]any{"path": "x", "mode": "bogus", "value": 1},
		map[string]any{"path": "y", "mode": "set", "value": 2},
	}}
	out := bodyAfter(t, `{}`, cfg, Context{})
	if gjsonExists(out, "x") {
		t.Fatalf("unknown mode should be skipped")
	}
	if gjsonGet(out, "y") != "2" {
		t.Fatalf("valid op after skipped one should still apply")
	}
}

func TestMalformedOperationSkipped(t *testing.T) {
	cfg := map[string]any{"operations": []any{
		"not-an-object", // malformed entry
		map[string]any{"path": "y", "mode": "set", "value": 2},
	}}
	out := bodyAfter(t, `{}`, cfg, Context{})
	if gjsonGet(out, "y") != "2" {
		t.Fatalf("valid op after malformed entry should still apply, got %s", gjsonGet(out, "y"))
	}
}

// --- helpers using gjson directly ---

func gjsonGet(s, path string) string {
	return gjson.Get(s, path).String()
}

func gjsonExists(s, path string) bool {
	return gjson.Get(s, path).Exists()
}
