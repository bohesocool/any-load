// Package affinity implements channel affinity (sticky routing): requests that
// share a derived affinity key are bound to a consistent (upstream, key) pair,
// mirroring the session-affinity approach used by CLIProxyAPI (CPA).
package affinity

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// sessionHeaders lists request headers (matched case-insensitively) that
// explicitly carry a session identifier, in priority order. The first
// non-empty value wins.
var sessionHeaders = []string{
	"X-Session-Id",
	"Session-Id",
	"Anthropic-Session-Id",
}

// bodySessionFields lists JSON body fields (top-level) that carry a session /
// conversation identifier, in priority order.
var bodySessionFields = []string{
	"session_id",
	"prompt_cache_key",
	"previous_response_id",
}

// DeriveAffinityKey computes a stable affinity key from the request headers and
// body. The derivation priority mirrors CPA's chain, generalized for the
// OpenAI / Anthropic / Gemini formats this proxy supports:
//  1. Explicit session headers (X-Session-Id / Session-Id / Anthropic-Session-Id)
//  2. Body JSON fields: session_id / prompt_cache_key / previous_response_id,
//     or nested metadata.session_id
//  3. First-message hash: sha1(model + NUL + first role=user message content)
//
// Returns "" when no usable trait is present; callers must then skip affinity
// and fall back to plain weighted round-robin (no binding is stored).
func DeriveAffinityKey(header http.Header, body []byte) string {
	// 1. Session headers.
	for _, h := range sessionHeaders {
		if v := strings.TrimSpace(header.Get(h)); v != "" {
			return "hdr:" + v
		}
	}

	if len(body) == 0 {
		return ""
	}

	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return ""
	}

	// 2. Top-level body session/conversation fields.
	for _, f := range bodySessionFields {
		if v, ok := stringifyField(data[f]); ok && v != "" {
			return "body:" + v
		}
	}
	// Nested metadata.session_id.
	if md, ok := data["metadata"].(map[string]any); ok {
		if v, ok := stringifyField(md["session_id"]); ok && v != "" {
			return "body:" + v
		}
	}

	// 3. First-message hash (binds a multi-turn conversation to one channel).
	model, _ := data["model"].(string)
	if firstUser := firstUserMessageContent(data); firstUser != "" {
		h := sha1.Sum([]byte(model + "\x00" + firstUser))
		return "msg:" + hex.EncodeToString(h[:])
	}

	return ""
}

// firstUserMessageContent extracts the text content of the first message whose
// role is "user" from a chat-style "messages" array. Content may be a string or
// an array of content parts (OpenAI multimodal format); only textual parts are
// concatenated. Returns "" if no usable user message is found.
func firstUserMessageContent(data map[string]any) string {
	msgs, ok := data["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return ""
	}
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if !strings.EqualFold(role, "user") {
			continue
		}
		if s, ok := msg["content"].(string); ok {
			return s
		}
		if parts, ok := msg["content"].([]any); ok {
			var b strings.Builder
			for _, p := range parts {
				if part, ok := p.(map[string]any); ok {
					if t, _ := part["type"].(string); t == "text" {
						if txt, ok := part["text"].(string); ok {
							b.WriteString(txt)
						}
					}
				}
			}
			if b.Len() > 0 {
				return b.String()
			}
		}
	}
	return ""
}

// stringifyField returns the string form of a JSON scalar used as a session id.
func stringifyField(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	}
	return "", false
}
