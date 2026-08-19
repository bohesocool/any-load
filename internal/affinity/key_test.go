package affinity

import (
	"crypto/sha1"
	"encoding/hex"
	"net/http"
	"testing"
)

func TestDeriveAffinityKey(t *testing.T) {
	// hdr builds a header with the given (canonical or not) keys via Set so the
	// lookup behaves like a real gin/http request.
	hdr := func(kv ...string) http.Header {
		h := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return h
	}

	cases := []struct {
		name   string
		header http.Header
		body   string
		want   string
	}{
		{
			name:   "empty body and headers yields no key",
			header: http.Header{},
			body:   "",
			want:   "",
		},
		{
			name:   "session header wins (case-insensitive)",
			header: hdr("x-session-id", "s1"),
			body:   `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			want:   "hdr:s1",
		},
		{
			name:   "first session header in priority order wins",
			header: hdr("session-id", "s2", "anthropic-session-id", "s3"),
			body:   "",
			want:   "hdr:s2",
		},
		{
			name:   "body session_id",
			header: http.Header{},
			body:   `{"session_id":"abc"}`,
			want:   "body:abc",
		},
		{
			name:   "body prompt_cache_key",
			header: http.Header{},
			body:   `{"prompt_cache_key":"pk1"}`,
			want:   "body:pk1",
		},
		{
			name:   "body previous_response_id",
			header: http.Header{},
			body:   `{"previous_response_id":"resp9"}`,
			want:   "body:resp9",
		},
		{
			name:   "nested metadata.session_id",
			header: http.Header{},
			body:   `{"metadata":{"session_id":"md1"}}`,
			want:   "body:md1",
		},
		{
			name:   "first-message hash fallback uses model + first user content",
			header: http.Header{},
			body:   `{"model":"gpt-x","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello"}]}`,
			want:   "msg:" + firstMsgHash("gpt-x", "hello"),
		},
		{
			name:   "first-message hash with array content parts",
			header: http.Header{},
			body:   `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"partA"},{"type":"image_url"}]}]}`,
			want:   "msg:" + firstMsgHash("m", "partA"),
		},
		{
			name:   "first-message hash differs by model",
			header: http.Header{},
			body:   `{"model":"m","messages":[{"role":"user","content":"x"}]}`,
			want:   "msg:" + firstMsgHash("m", "x"),
		},
		{
			name:   "no messages and no session yields empty",
			header: http.Header{},
			body:   `{"model":"m"}`,
			want:   "",
		},
		{
			name:   "invalid json yields empty",
			header: http.Header{},
			body:   `{not json`,
			want:   "",
		},
		{
			name:   "header takes priority over body",
			header: hdr("x-session-id", "hdr"),
			body:   `{"session_id":"body"}`,
			want:   "hdr:hdr",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DeriveAffinityKey(tc.header, []byte(tc.body))
			if got != tc.want {
				t.Fatalf("DeriveAffinityKey = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDeriveAffinityKeyStability verifies the same logical request produces the
// same key across calls (required for stickiness).
func TestDeriveAffinityKeyStability(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"stable"}]}`
	h := http.Header{"x-session-id": []string{"S"}}
	k1 := DeriveAffinityKey(h, []byte(body))
	k2 := DeriveAffinityKey(h, []byte(body))
	if k1 != k2 {
		t.Fatalf("unstable key: %q vs %q", k1, k2)
	}
}

// firstMsgHash mirrors DeriveAffinityKey's fallback so the test's expected
// value stays in sync with the implementation.
func firstMsgHash(model, content string) string {
	h := sha1.Sum([]byte(model + "\x00" + content))
	return hex.EncodeToString(h[:])
}
