package protocol

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

// Sentinel errors shared across handlers.
var (
	errMultimodalUnsupported = errors.New("non-text (multimodal) content is not supported in protocol conversion phase 1")
	errInvalidContent        = errors.New("invalid content format")
	errGeminiURLImage        = errors.New("gemini upstream does not accept image URLs; provide base64 or a data: URI")
	errEOF                   = io.EOF
)

// errToolsUnsupported was used in the text-only phases to reject tool
// requests. Retained as a no-op sentinel for any handler path that still
// needs to refuse an unsupported tool variant; tools are otherwise supported.
var errToolsUnsupported = errors.New("tools are not supported on this path")

// createdNow returns the current unix timestamp for response `created` fields.
func createdNow() int64 {
	return time.Now().Unix()
}

// getString returns m[key] as a string, or "".
func getString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// toInt coerces a JSON number (float64) or int-like value to int.
func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}

// isBodyStreamFlag reports whether a request body carries a top-level
// boolean "stream": true. Used by formats that signal streaming via the body
// (OpenAI Chat, Anthropic).
func isBodyStreamFlag(body []byte) bool {
	var p struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return false
	}
	return p.Stream
}

// extractGeminiModel extracts the model name from a Gemini-style path such as
// /v1beta/models/<model>:generateContent or :streamGenerateContent. Returns
// "" if no model is found.
func extractGeminiModel(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if part == "models" && i+1 < len(parts) {
			return strings.Split(parts[i+1], ":")[0]
		}
	}
	return ""
}

// parseDataURI splits a "data:<mime>;base64,<data>" URI into its mime and
// base64 parts. Returns ok=false if s is not a data URI.
func parseDataURI(s string) (mime, base64 string, ok bool) {
	if !strings.HasPrefix(s, "data:") {
		return "", "", false
	}
	rest := strings.TrimPrefix(s, "data:")
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", "", false
	}
	meta := rest[:comma]
	data := rest[comma+1:]
	mime = strings.TrimSuffix(meta, ";base64")
	if mime == "" {
		mime = "image/png"
	}
	return mime, data, true
}

// argsToString returns the JSON object raw bytes as a string, defaulting to
// "{}" when empty. Used by OpenAI emitters which carry arguments as a JSON
// string.
func argsToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

// argsToObject unmarshals the JSON object raw bytes into a generic value for
// emitters that carry arguments as an object (Anthropic input, Gemini args).
// Returns an empty map on empty input.
func argsToObject(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// rawJSON converts any value (e.g. an object from Anthropic input_schema /
// Gemini parameters) to json.RawMessage, or returns nil for nil/missing.
func rawJSON(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// --- tool choice normalization (inbound format → IR ToolChoice) ---

// normalizeToolChoiceOpenAI parses an OpenAI/Responses tool_choice value
// (string "auto"/"none"/"required", or object {type:"function", function:{name}}).
func normalizeToolChoiceOpenAI(v any) *ToolChoice {
	switch t := v.(type) {
	case string:
		switch t {
		case "auto", "none", "required":
			return &ToolChoice{Type: t}
		}
	case map[string]any:
		if getString(t, "type") == "function" {
			if fn, ok := t["function"].(map[string]any); ok {
				return &ToolChoice{Type: "function", Name: getString(fn, "name")}
			}
		}
	}
	return nil
}

// normalizeToolChoiceAnthropic parses an Anthropic tool_choice object
// ({type:"auto"|"any"|"none"|"tool", name?}).
func normalizeToolChoiceAnthropic(v any) *ToolChoice {
	t, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	switch getString(t, "type") {
	case "auto":
		return &ToolChoice{Type: "auto"}
	case "none":
		return &ToolChoice{Type: "none"}
	case "any":
		return &ToolChoice{Type: "required"}
	case "tool":
		return &ToolChoice{Type: "function", Name: getString(t, "name")}
	}
	return nil
}

// normalizeToolChoiceGemini parses a Gemini toolConfig.functionCallingConfig
// object ({mode:"AUTO"|"ANY"|"NONE", allowedFunctionNames?[...]}).
func normalizeToolChoiceGemini(v any) *ToolChoice {
	t, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	switch getString(t, "mode") {
	case "AUTO":
		return &ToolChoice{Type: "auto"}
	case "NONE":
		return &ToolChoice{Type: "none"}
	case "ANY":
		if names, ok := t["allowedFunctionNames"].([]any); ok && len(names) == 1 {
			if n, ok := names[0].(string); ok {
				return &ToolChoice{Type: "function", Name: n}
			}
		}
		return &ToolChoice{Type: "required"}
	}
	return nil
}

// --- tool choice emission (IR ToolChoice → outbound format) ---

// emitToolChoiceOpenAI returns the OpenAI/Responses tool_choice value: a
// string for auto/none/required, or an object for a forced function.
func emitToolChoiceOpenAI(tc *ToolChoice) any {
	if tc == nil {
		return nil
	}
	switch tc.Type {
	case "auto", "none", "required":
		return tc.Type
	case "function":
		return map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
	}
	return nil
}

// emitToolChoiceAnthropic returns the Anthropic tool_choice object.
func emitToolChoiceAnthropic(tc *ToolChoice) map[string]any {
	if tc == nil {
		return nil
	}
	switch tc.Type {
	case "auto":
		return map[string]any{"type": "auto"}
	case "none":
		return map[string]any{"type": "none"}
	case "required":
		return map[string]any{"type": "any"}
	case "function":
		return map[string]any{"type": "tool", "name": tc.Name}
	}
	return nil
}

// emitToolChoiceGemini returns the Gemini toolConfig object.
func emitToolChoiceGemini(tc *ToolChoice) map[string]any {
	if tc == nil {
		return nil
	}
	fcc := map[string]any{}
	switch tc.Type {
	case "auto":
		fcc["mode"] = "AUTO"
	case "none":
		fcc["mode"] = "NONE"
	case "required":
		fcc["mode"] = "ANY"
	case "function":
		fcc["mode"] = "ANY"
		fcc["allowedFunctionNames"] = []string{tc.Name}
	}
	return map[string]any{"functionCallingConfig": fcc}
}

// --- tool result message grouping (IR role="tool" → target container) ---

// groupToolResultsForAnthropic walks the IR message list and merges each run
// of consecutive role="tool" messages into a single Anthropic "user" message
// whose content is an array of tool_result blocks. Non-tool messages pass
// through unchanged (as Anthropic message maps with string content).
func groupToolResultsForAnthropic(msgs []ChatMessage) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	i := 0
	for i < len(msgs) {
		if msgs[i].Role != "tool" {
			out = append(out, map[string]any{"role": msgs[i].Role, "content": msgs[i].Content})
			i++
			continue
		}
		// Collect consecutive tool messages into one user message.
		results := []map[string]any{}
		for i < len(msgs) && msgs[i].Role == "tool" {
			results = append(results, map[string]any{
				"type":        "tool_result",
				"tool_use_id": msgs[i].ToolCallID,
				"content":     msgs[i].Content,
			})
			i++
		}
		out = append(out, map[string]any{"role": "user", "content": results})
	}
	return out
}

// groupToolResultsForGemini walks the IR message list and merges each run of
// consecutive role="tool" messages into a single Gemini "user" content whose
// parts are functionResponse objects. Non-tool messages pass through as
// {role, parts:[{text}]}.
func groupToolResultsForGemini(msgs []ChatMessage) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	i := 0
	for i < len(msgs) {
		if msgs[i].Role != "tool" {
			out = append(out, map[string]any{
				"role":  msgs[i].Role,
				"parts": []map[string]any{{"text": msgs[i].Content}},
			})
			i++
			continue
		}
		parts := []map[string]any{}
		for i < len(msgs) && msgs[i].Role == "tool" {
			name := msgs[i].Name
			if name == "" {
				name = "tool"
			}
			// Gemini functionResponse.response is an object; parse the IR
			// content (a JSON string) if possible, else wrap as {"output": ...}.
			var resp any
			if err := json.Unmarshal([]byte(msgs[i].Content), &resp); err != nil {
				resp = map[string]any{"output": msgs[i].Content}
			}
			parts = append(parts, map[string]any{
				"functionResponse": map[string]any{"name": name, "response": resp},
			})
			i++
		}
		out = append(out, map[string]any{"role": "user", "parts": parts})
	}
	return out
}
